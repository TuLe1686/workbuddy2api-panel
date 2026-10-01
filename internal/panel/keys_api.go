// keys_api.go 下游 API Key 管理接口：签发 / 改限额 / 启停 / 清零台账 / 删除。
//
// 全部挂 withAuth（管理面密钥）：签发出的 key 只能调 /v1/*，绝不能反过来管理
// key 表本身（否则拿到下游 key 就能给自己解除限额）。
package panel

import (
	"encoding/json"
	"io"
	"log"
	"math"
	"net/http"
	"strings"
)

// keysGuard 返回 key 仓库；未装配（nil）时写 501 并返回 false。
func (p *Panel) keysGuard(w http.ResponseWriter) bool {
	if p.cfg.Keys == nil {
		writeErr(w, http.StatusNotImplemented, "api keys store not available")
		return false
	}
	return true
}

// keysList 返回全部 key（含明文 Secret 与消耗台账）。
// 明文存储是既定决策（与 config.json 里 api_key 同级安全边界）：面板默认掩码
// 展示，点「显示」才露出完整值。
func (p *Panel) keysList(w http.ResponseWriter, r *http.Request) {
	if !p.keysGuard(w) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "keys": p.cfg.Keys.ListView()})
}

// keysCreate 签发一把新 key。请求体：{"name":"客户A","max_credits":100,"max_tokens":0,"max_concurrency":0}。
// 限额 <=0 或缺省 = 不限。name 缺省自动生成。
func (p *Panel) keysCreate(w http.ResponseWriter, r *http.Request) {
	if !p.keysGuard(w) {
		return
	}
	var req struct {
		Name           string  `json:"name"`
		MaxCredits     float64 `json:"max_credits"`
		MaxTokens      int64   `json:"max_tokens"`
		MaxConcurrency int     `json:"max_concurrency"`
	}
	if err := readKeysBody(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if req.MaxCredits < 0 || req.MaxTokens < 0 || req.MaxConcurrency < 0 || math.IsNaN(req.MaxCredits) || math.IsInf(req.MaxCredits, 0) {
		writeErr(w, http.StatusBadRequest, "限额必须是非负数（0 = 不限）")
		return
	}
	k, err := p.cfg.Keys.Create(req.Name, req.MaxCredits, req.MaxTokens, req.MaxConcurrency)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "创建失败: "+err.Error())
		return
	}
	log.Printf("panel: 签发 API Key id=%s name=%q max_credits=%.2f max_tokens=%d max_concurrency=%d", k.ID, k.Name, k.MaxCredits, k.MaxTokens, k.MaxConcurrency)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "key": k, "keys": p.cfg.Keys.ListView()})
}

// keysUpdate 修改 key 的可变字段。请求体字段缺省 = 不改；
// {"name":..,"max_credits":..,"max_tokens":..,"max_concurrency":..,"disabled":true}。
// 显式传 0 才能清掉限额（与「未提交」区分）。
func (p *Panel) keysUpdate(w http.ResponseWriter, r *http.Request) {
	if !p.keysGuard(w) {
		return
	}
	id := r.PathValue("id")
	var req struct {
		Name           *string  `json:"name"`
		MaxCredits     *float64 `json:"max_credits"`
		MaxTokens      *int64   `json:"max_tokens"`
		MaxConcurrency *int     `json:"max_concurrency"`
		Disabled       *bool    `json:"disabled"`
	}
	if err := readKeysBody(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if req.MaxCredits != nil && (*req.MaxCredits < 0 || math.IsNaN(*req.MaxCredits) || math.IsInf(*req.MaxCredits, 0)) {
		writeErr(w, http.StatusBadRequest, "max_credits 必须是非负数")
		return
	}
	if req.MaxTokens != nil && *req.MaxTokens < 0 {
		writeErr(w, http.StatusBadRequest, "max_tokens 必须是非负数")
		return
	}
	if req.MaxConcurrency != nil && *req.MaxConcurrency < 0 {
		writeErr(w, http.StatusBadRequest, "max_concurrency 必须是非负数")
		return
	}
	k, ok, err := p.cfg.Keys.Update(id, req.Name, req.MaxCredits, req.MaxTokens, req.MaxConcurrency, req.Disabled)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "保存失败: "+err.Error())
		return
	}
	if !ok {
		writeErr(w, http.StatusNotFound, "key 不存在")
		return
	}
	log.Printf("panel: 更新 API Key id=%s name=%q max_credits=%.2f max_tokens=%d max_concurrency=%d disabled=%v", k.ID, k.Name, k.MaxCredits, k.MaxTokens, k.MaxConcurrency, k.Disabled)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "key": k, "keys": p.cfg.Keys.ListView()})
}

// keysReset 清零一把 key 的消耗台账（限额重新从零起算）。
func (p *Panel) keysReset(w http.ResponseWriter, r *http.Request) {
	if !p.keysGuard(w) {
		return
	}
	id := r.PathValue("id")
	if !p.cfg.Keys.ResetStats(id) {
		writeErr(w, http.StatusNotFound, "key 不存在")
		return
	}
	log.Printf("panel: 清零 API Key 台账 id=%s", id)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "keys": p.cfg.Keys.ListView()})
}

// keysRemove 删除一把 key（立即失效：在途请求不受影响，新请求 401）。
func (p *Panel) keysRemove(w http.ResponseWriter, r *http.Request) {
	if !p.keysGuard(w) {
		return
	}
	id := r.PathValue("id")
	if !p.cfg.Keys.Remove(id) {
		writeErr(w, http.StatusNotFound, "key 不存在")
		return
	}
	log.Printf("panel: 删除 API Key id=%s", id)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "keys": p.cfg.Keys.ListView()})
}

// readKeysBody 读取并解析 keys 接口的 JSON 请求体（上限 64KB，够用且防滥用）。
func readKeysBody(r *http.Request, v any) error {
	raw, err := io.ReadAll(io.LimitReader(r.Body, 64<<10))
	if err != nil {
		return errRequest("read body: " + err.Error())
	}
	if strings.TrimSpace(string(raw)) == "" {
		return nil // 空体 = 全部字段不提交（Update 全不改）
	}
	if err := json.Unmarshal(raw, v); err != nil {
		return errRequest("请求体解析失败: " + err.Error())
	}
	return nil
}

// errRequest 轻量错误类型（避免为两处 body 读取引入 errors 包）。
type errRequest string

func (e errRequest) Error() string { return string(e) }
