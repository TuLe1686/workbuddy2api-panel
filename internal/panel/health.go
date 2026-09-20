// health.go 凭证体检：批量探活账号凭证（余额接口），列出失效号，支持一键移除。
//
// 为什么需要它：批量导入的凭证可能在导出后就被上游吊销（同账号在别处再登录一次即失效），
// 表现为**所有**上游调用 401（openresty/APISIX 的 HTML 拦截页）。这类号留在池里会
// 反复白跑：选号抽到→失败一次→换号，定时任务也会一直报错。体检把"哪些号已经死了"
// 一次性问清楚，并给出移除入口。
//
// 判据（只认确定性证据，宁缺毋滥）：
//   - ok      ：余额接口正常返回 → 凭证可用（顺带把余额写回池）
//   - invalid ：上游明确 401（token 被拒）→ 凭证失效，需要重新登录
//   - unknown ：其余失败（网络抖动、限流 429、上游 5xx 等）→ 不作结论，避免误杀好号
package panel

import (
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// healthConcurrency 体检并发（与余额后台刷新/券码查询同口径 3）：更高并发对上游不友好，
// 也更可能触发风控；3 路下 200+ 账号约 40 秒完成，面板轮询足够跟得上。
const healthConcurrency = 3

// healthResult 单账号体检结论。
type healthResult struct {
	UID       string `json:"uid"`
	Nickname  string `json:"nickname,omitempty"`
	Status    string `json:"status"` // ok | invalid | unknown
	Error     string `json:"error,omitempty"`
	CheckedAt string `json:"checked_at"`
}

// healthJob 体检任务状态。面板同一时刻只允许一个体检在跑（避免重复锤上游）。
type healthJob struct {
	mu        sync.Mutex
	running   bool
	startedAt time.Time
	checkedAt time.Time
	total     int
	done      int
	results   map[string]healthResult // uid → 结论（历史保留，便于面板标红）
}

func newHealthJob() *healthJob {
	return &healthJob{results: map[string]healthResult{}}
}

func (j *healthJob) snapshot() (running bool, total, done int, checkedAt time.Time, items []healthResult) {
	j.mu.Lock()
	defer j.mu.Unlock()
	items = make([]healthResult, 0, len(j.results))
	for _, r := range j.results {
		items = append(items, r)
	}
	// 失效优先、未知次之、正常最后：面板一眼看到要处理的号。
	rank := map[string]int{"invalid": 0, "unknown": 1, "ok": 2}
	sort.SliceStable(items, func(a, b int) bool {
		if rank[items[a].Status] != rank[items[b].Status] {
			return rank[items[a].Status] < rank[items[b].Status]
		}
		return items[a].UID < items[b].UID
	})
	return j.running, j.total, j.done, j.checkedAt, items
}

func (j *healthJob) count(status string) int {
	j.mu.Lock()
	defer j.mu.Unlock()
	n := 0
	for _, r := range j.results {
		if r.Status == status {
			n++
		}
	}
	return n
}

// healthPath 体检结果落盘路径（空 = 只存内存）。
func (p *Panel) healthPath() string { return p.cfg.HealthFile }

// loadHealth 启动时载入上次体检结论（文件缺失/损坏按空表继续——体检结论不是关键数据）。
func (p *Panel) loadHealth() {
	if p.health == nil || p.healthPath() == "" {
		return
	}
	raw, err := os.ReadFile(p.healthPath())
	if err != nil {
		return
	}
	var doc struct {
		CheckedAt string                  `json:"checked_at"`
		Results   map[string]healthResult `json:"results"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		log.Printf("WARN: 体检结果解析失败（忽略）: %v", err)
		return
	}
	p.health.mu.Lock()
	defer p.health.mu.Unlock()
	p.health.results = doc.Results
	if t, err := time.Parse(time.RFC3339, doc.CheckedAt); err == nil {
		p.health.checkedAt = t
	}
	if len(doc.Results) > 0 {
		log.Printf("已载入上次凭证体检结论：%d 个账号（%s）", len(doc.Results), doc.CheckedAt)
	}
}

// saveHealth 体检结束后落盘（原子替换：health.json 在目录挂载里，tmp+rename 可用）。
func (p *Panel) saveHealth() {
	if p.health == nil || p.healthPath() == "" {
		return
	}
	p.health.mu.Lock()
	doc := map[string]any{
		"version":    1,
		"checked_at": p.health.checkedAt.Format(time.RFC3339),
		"results":    p.health.results,
	}
	p.health.mu.Unlock()
	raw, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return
	}
	if dir := filepath.Dir(p.healthPath()); dir != "" && dir != "." {
		_ = os.MkdirAll(dir, 0o755)
	}
	tmp := p.healthPath() + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		log.Printf("WARN: 体检结果落盘失败: %v", err)
		return
	}
	if err := os.Rename(tmp, p.healthPath()); err != nil {
		log.Printf("WARN: 体检结果落盘失败: %v", err)
	}
}

// accountsHealthCheck 启动一次凭证体检（异步）。请求体可选 {"uids":[...]}：给了就只体检这些
// （面板"体检所选"），缺省全体检池内账号。
func (p *Panel) accountsHealthCheck(w http.ResponseWriter, r *http.Request) {
	var req struct {
		UIDs []string `json:"uids"`
	}
	if r.Body != nil {
		raw, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		if len(strings.TrimSpace(string(raw))) > 0 {
			if err := json.Unmarshal(raw, &req); err != nil {
				writeErr(w, http.StatusBadRequest, "请求体解析失败: "+err.Error())
				return
			}
		}
	}
	if p.health == nil {
		writeErr(w, http.StatusNotImplemented, "health checker not available")
		return
	}
	if p.cfg.Upstream == nil {
		writeErr(w, http.StatusNotImplemented, "upstream client not available")
		return
	}

	// 复用导出的"取账号"口径：uids 为空 = 全量，取不到的记入 missing。
	picked, missing := p.pickAccountsForExport(req.UIDs)
	if len(picked) == 0 {
		writeErr(w, http.StatusBadRequest, "没有可体检的账号")
		return
	}

	p.health.mu.Lock()
	if p.health.running {
		p.health.mu.Unlock()
		writeErr(w, http.StatusConflict, "已有体检在进行中，请等它跑完")
		return
	}
	p.health.running = true
	p.health.startedAt = time.Now()
	p.health.total = len(picked)
	p.health.done = 0
	p.health.mu.Unlock()

	go p.runHealthCheck(picked)

	log.Printf("panel: 凭证体检已启动：%d 个账号（跳过 %d 个不在池内）", len(picked), len(missing))
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "started": true, "total": len(picked), "skipped": len(missing)})
}

// runHealthCheck 体检主循环：并发 3 逐号探活，结果写回 job 并由 /accounts/healthcheck 轮询读取。
func (p *Panel) runHealthCheck(accounts []*auth.Auth) {
	sem := make(chan struct{}, healthConcurrency)
	var wg sync.WaitGroup
	now := time.Now().Format(time.RFC3339)
	for _, a := range accounts {
		wg.Add(1)
		go func(a *auth.Auth) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			res := p.probeAccountCredential(a)
			res.CheckedAt = now
			p.health.mu.Lock()
			p.health.results[a.UID] = res
			p.health.done++
			p.health.mu.Unlock()
		}(a)
	}
	wg.Wait()

	p.health.mu.Lock()
	p.health.running = false
	p.health.checkedAt = time.Now()
	p.health.mu.Unlock()
	p.saveHealth()
	log.Printf("panel: 凭证体检完成：有效 %d / 失效 %d / 未知 %d（共 %d）",
		p.health.count("ok"), p.health.count("invalid"), p.health.count("unknown"), len(accounts))
}

// probeAccountCredential 单号探活：调余额接口，按结果分类。
//
// 为什么用余额接口：它是"只读 + 顺带有用产出"（拿回余额并写回池）的调用，
// 且与签到/任务/开学季同域同鉴权口径——它 401 就意味着这批凭证对所有上游接口 401。
func (p *Panel) probeAccountCredential(a *auth.Auth) healthResult {
	res := healthResult{UID: a.UID, Nickname: a.Nickname, Status: "unknown"}
	remain, total, err := p.cfg.Upstream.UserResource(a)
	if err == nil {
		res.Status = "ok"
		p.cfg.Pool.SetCredits(a.UID, remain, total)
		p.cfg.Pool.ReenableIfCredits(a.UID, remain, total)
		return res
	}
	res.Error = err.Error()
	var uerr *upstream.Error
	if errors.As(err, &uerr) && uerr.Status == http.StatusUnauthorized {
		res.Status = "invalid"
	}
	return res
}

// accountsHealthStatus 体检进度与结论（面板轮询）。
func (p *Panel) accountsHealthStatus(w http.ResponseWriter, r *http.Request) {
	if p.health == nil {
		writeErr(w, http.StatusNotImplemented, "health checker not available")
		return
	}
	running, total, done, checkedAt, items := p.health.snapshot()
	resp := map[string]any{
		"ok":       true,
		"running":  running,
		"total":    total,
		"done":     done,
		"ok_count": p.health.count("ok"),
		"invalid":  p.health.count("invalid"),
		"unknown":  p.health.count("unknown"),
		"items":    items,
	}
	if !checkedAt.IsZero() {
		resp["checked_at"] = checkedAt.Format(time.RFC3339)
	}
	writeJSON(w, http.StatusOK, resp)
}

// accountsRemoveSelected 移除指定账号（含凭证文件与标签）。需显式 confirm，
// 语义与"全部移除"一致，只是范围由调用方给出（面板用它清理体检出来的失效号）。
func (p *Panel) accountsRemoveSelected(w http.ResponseWriter, r *http.Request) {
	var req struct {
		UIDs    []string `json:"uids"`
		Confirm bool     `json:"confirm"`
	}
	raw, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err := json.Unmarshal(raw, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "请求体解析失败: "+err.Error())
		return
	}
	if !req.Confirm {
		writeErr(w, http.StatusBadRequest, "needs_confirm")
		return
	}
	if len(req.UIDs) == 0 {
		writeErr(w, http.StatusBadRequest, "uids 不能为空")
		return
	}
	removed, missing, fileErrs := p.removeByUIDs(req.UIDs)
	log.Printf("panel: 移除所选账号 removed=%d missing=%d file_errors=%d", removed, missing, len(fileErrs))
	resp := map[string]any{"ok": true, "removed": removed, "missing": missing}
	if len(fileErrs) > 0 {
		resp["file_errors"] = fileErrs
	}
	writeJSON(w, http.StatusOK, resp)
}

// removeByUIDs 按 uid 出池 + 删凭证文件 + 清标签 + 清体检结论（调用方负责日志与响应）。
func (p *Panel) removeByUIDs(uids []string) (removed, missing int, fileErrs []string) {
	var removedUIDs []string
	for _, uid := range uids {
		a := p.cfg.Pool.Remove(uid)
		if a == nil {
			missing++
			continue
		}
		removed++
		removedUIDs = append(removedUIDs, a.UID)
		if a.FilePath != "" {
			if err := os.Remove(a.FilePath); err != nil && !os.IsNotExist(err) {
				fileErrs = append(fileErrs, a.UID+": "+err.Error())
			}
		}
	}
	if p.tags != nil {
		p.tags.forget(removedUIDs)
	}
	if p.health != nil && len(removedUIDs) > 0 {
		p.health.mu.Lock()
		for _, uid := range removedUIDs {
			delete(p.health.results, uid)
		}
		p.health.mu.Unlock()
		p.saveHealth()
	}
	return removed, missing, fileErrs
}

// invalidUIDs 返回最近一次体检判定为失效的 uid（一键移除用）。
func (p *Panel) invalidUIDs() []string {
	if p.health == nil {
		return nil
	}
	p.health.mu.Lock()
	defer p.health.mu.Unlock()
	var out []string
	for uid, r := range p.health.results {
		if r.Status == "invalid" {
			out = append(out, uid)
		}
	}
	sort.Strings(out)
	return out
}

// healthForUID 面板行内标记用：返回该账号的体检结论（无结论返回空串）。
func (p *Panel) healthForUID(uid string) string {
	if p.health == nil {
		return ""
	}
	p.health.mu.Lock()
	defer p.health.mu.Unlock()
	return p.health.results[uid].Status
}
