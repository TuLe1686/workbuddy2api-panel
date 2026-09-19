// export.go 面板导出能力：账号凭证导出（与「导入账号文件」同格式，可原样导回）
// 与券码 xlsx 导出。
//
// 安全口径：
//   - 账号导出含**明文凭证**，属管理面能力：只在 withAuth（panel_key）之后挂载；
//   - 响应一律 Cache-Control: no-store，避免中间层/浏览器缓存落盘；
//   - 日志只记数量与域名，绝不记 token 正文。
package panel

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// exportAccount 账号导出条目：字段名与「导入账号文件」支持的扁平 snake_case 一致，
// 因此导出的文件可直接再导入（显式带 realm，不依赖 domain/id 前缀推断）。
//
// expires_at 用**毫秒**：与外部导出（codebuddy_cn_accounts_*.json）同形；
// 导入侧按量级自动归一，两种单位都能吃回。
type exportAccount struct {
	ID           string `json:"id"`
	UID          string `json:"uid"`
	Nickname     string `json:"nickname,omitempty"`
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token,omitempty"`
	TokenType    string `json:"token_type"`
	ExpiresAtMS  int64  `json:"expires_at"`
	Domain       string `json:"domain,omitempty"`
	Realm        string `json:"realm"`
	Status       string `json:"status"`
	ExportedAt   string `json:"exported_at"`
}

// accountsExport 导出账号凭证为 JSON 文件（与导入同格式）。
//
// 请求体：{"uids":["..."]}（缺省/空数组 = 全量；单账号导出即只给一个 uid）。
// 已禁用账号照常导出——导出的是凭证本身，与池内运行状态无关。
func (p *Panel) accountsExport(w http.ResponseWriter, r *http.Request) {
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

	picked, missing := p.pickAccountsForExport(req.UIDs)
	if len(picked) == 0 {
		if len(missing) > 0 {
			writeErr(w, http.StatusNotFound, "账号不存在: "+strings.Join(missing, ", "))
			return
		}
		writeErr(w, http.StatusBadRequest, "没有可导出的账号")
		return
	}

	now := time.Now()
	items := make([]exportAccount, 0, len(picked))
	realms := map[string]bool{}
	for _, a := range picked {
		realm := a.Realm()
		realms[realm] = true
		prefix := "codebuddy_cn_"
		if realm == "global" {
			prefix = "codebuddy_global_"
		}
		items = append(items, exportAccount{
			ID:           prefix + a.UID,
			UID:          a.UID,
			Nickname:     a.Nickname,
			AccessToken:  a.AccessTokenValue(),
			RefreshToken: a.RefreshTokenValue(),
			TokenType:    "Bearer",
			ExpiresAtMS:  a.ExpiresAt * 1000,
			Domain:       a.DomainValue(),
			Realm:        realm,
			Status:       "normal",
			ExportedAt:   now.Format(time.RFC3339),
		})
	}

	raw, err := json.MarshalIndent(items, "", "  ")
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "序列化失败: "+err.Error())
		return
	}
	name := exportFileName(realms, now)
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Content-Disposition", "attachment; filename=\""+name+"\"")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(raw)
	log.Printf("panel: 导出账号 %d 个（realm=%s，文件 %s）", len(items), realmLabel(realms), name)
}

// pickAccountsForExport 解析导出目标：uids 为空 = 全量；否则逐个取池内账号，
// 取不到的记入 missing（单账号导出据此回 404）。
func (p *Panel) pickAccountsForExport(uids []string) ([]*auth.Auth, []string) {
	if len(uids) == 0 {
		st := p.cfg.Pool.List()
		out := make([]*auth.Auth, 0, len(st))
		for _, s := range st {
			if a := p.cfg.Pool.AuthByUID(s.UID); a != nil {
				out = append(out, a)
			}
		}
		return out, nil
	}
	out := make([]*auth.Auth, 0, len(uids))
	var missing []string
	for _, uid := range uids {
		if a := p.cfg.Pool.AuthByUID(uid); a != nil {
			out = append(out, a)
			continue
		}
		missing = append(missing, uid)
	}
	return out, missing
}

// exportFileName 导出文件名：单域 codebuddy_<realm>_accounts_<date>.json，
// 混域退回 codebuddy_accounts_<date>.json（两种前缀都能被导入识别）。
func exportFileName(realms map[string]bool, now time.Time) string {
	date := now.Format("2006-01-02")
	if len(realms) == 1 {
		for r := range realms {
			return fmt.Sprintf("codebuddy_%s_accounts_%s.json", r, date)
		}
	}
	return "codebuddy_accounts_" + date + ".json"
}

func realmLabel(realms map[string]bool) string {
	if len(realms) == 1 {
		for r := range realms {
			return r
		}
	}
	return "mixed"
}

// accountsRemoveAll 一键移除全部账号：先出池（立即落盘 state），再删各自凭证文件。
// 需要显式 {"confirm":true} 才执行——不可撤销的批量操作，挡的是误触。
func (p *Panel) accountsRemoveAll(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Confirm bool `json:"confirm"`
	}
	if r.Body != nil {
		raw, _ := io.ReadAll(io.LimitReader(r.Body, 1<<12))
		_ = json.Unmarshal(raw, &req)
	}
	if !req.Confirm {
		writeErr(w, http.StatusBadRequest, "needs_confirm")
		return
	}
	accounts := p.cfg.Pool.List()
	removed := 0
	var fileErrs []string
	for _, s := range accounts {
		a := p.cfg.Pool.Remove(s.UID)
		if a == nil {
			continue
		}
		removed++
		if a.FilePath != "" {
			if err := os.Remove(a.FilePath); err != nil && !os.IsNotExist(err) {
				fileErrs = append(fileErrs, a.UID+": "+err.Error())
			}
		}
	}
	log.Printf("panel: 一键全部移除 removed=%d file_errors=%d", removed, len(fileErrs))
	resp := map[string]any{"ok": true, "removed": removed}
	if len(fileErrs) > 0 {
		resp["file_errors"] = fileErrs
	}
	writeJSON(w, http.StatusOK, resp)
}

// voucherRow 券码导出行（xlsx 三列：商品名 / 有效期 / 券码）。
type voucherRow struct {
	Prize   string
	ValidTo string
	Code    string
}

// schoolVouchersXLSX 导出全部已中奖券码为 xlsx（三列：商品名 / 有效期 / 券码）。
// 查询口径与「查询券码」弹窗一致（并发上限 + 逐账号容错），只是把结果摊平成表格。
func (p *Panel) schoolVouchersXLSX(w http.ResponseWriter, r *http.Request) {
	rows := p.collectVoucherRows()
	out := make([][]string, 0, len(rows))
	for _, v := range rows {
		out = append(out, []string{v.Prize, v.ValidTo, v.Code})
	}
	var buf bytes.Buffer
	if err := writeXLSX(&buf, "券码", []string{"商品名", "有效期", "券码"}, out, []float64{24, 16, 30}); err != nil {
		writeErr(w, http.StatusInternalServerError, "生成 xlsx 失败: "+err.Error())
		return
	}
	name := "vouchers_" + time.Now().Format("2006-01-02") + ".xlsx"
	w.Header().Set("Content-Type", "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet")
	w.Header().Set("Content-Disposition", "attachment; filename=\""+name+"\"")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(buf.Bytes())
	log.Printf("panel: 导出券码 xlsx %d 行（文件 %s）", len(out), name)
}

// collectVoucherRows 汇总全池券码为扁平行（商品名/有效期/券码），按有效期、商品名排序。
// 查询失败或 global 域账号跳过（导出场景不因个别账号失败而整体失败）。
func (p *Panel) collectVoucherRows() []voucherRow {
	accts := p.cfg.Pool.List()
	results := make([][]voucherRow, len(accts))
	sem := make(chan struct{}, 3)
	var wg sync.WaitGroup
	for i, st := range accts {
		a := p.cfg.Pool.AuthByUID(st.UID)
		if a == nil || st.Disabled || a.IsGlobal() {
			continue
		}
		wg.Add(1)
		go func(i int, a *auth.Auth) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			vs, err := p.cfg.Upstream.SchoolVouchers(a)
			if err != nil {
				return
			}
			for _, v := range vs {
				results[i] = append(results[i], voucherRow{
					Prize:   voucherPrize(v),
					ValidTo: voucherValidTo(v),
					Code:    v.Code,
				})
			}
		}(i, a)
	}
	wg.Wait()
	var out []voucherRow
	for _, r := range results {
		out = append(out, r...)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].ValidTo != out[j].ValidTo {
			return out[i].ValidTo < out[j].ValidTo
		}
		return out[i].Prize < out[j].Prize
	})
	return out
}

// voucherPrize 商品名：优先上游 prize_name，兜底 sku_code，都空给占位名。
func voucherPrize(v upstream.SchoolVoucher) string {
	if s := strings.TrimSpace(v.PrizeName); s != "" {
		return s
	}
	if s := strings.TrimSpace(v.SKUCode); s != "" {
		return s
	}
	return "（未命名奖品）"
}

// voucherValidTo 有效期：上游 valid_to（形如 2026-10-24）；起点非空时拼成区间。
func voucherValidTo(v upstream.SchoolVoucher) string {
	from := strings.TrimSpace(v.ValidFrom)
	to := strings.TrimSpace(v.ValidTo)
	switch {
	case from != "" && to != "":
		return from + " ~ " + to
	case to != "":
		return to
	case from != "":
		return from + " 起"
	default:
		return "—"
	}
}
