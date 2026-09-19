// tags_api.go 标签接口：读取标签表 + 批量打标签/去标签。
//
// 全部挂在管理面（withAuth）之后：标签本身不敏感，但赋值动作会改变运营视图，
// 与其它账号运维接口同口径更安全。
package panel

import (
	"encoding/json"
	"io"
	"log"
	"net/http"
	"strings"
)

// tagsList 返回标签全量视图：by_uid（每账号标签）+ all（全部标签及计数）。
// 顺带清掉池内已不存在的 uid 的标签（池重建/账号移除后不留幽灵标签）。
func (p *Panel) tagsList(w http.ResponseWriter, r *http.Request) {
	if p.tags == nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "by_uid": map[string][]string{}, "all": []tagCount{}})
		return
	}
	alive := map[string]bool{}
	for _, s := range p.cfg.Pool.List() {
		alive[s.UID] = true
	}
	p.tags.prune(alive)
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":     true,
		"by_uid": p.tags.Snapshot(),
		"all":    p.tags.AllTags(),
	})
}

// tagsAssign 批量打标签 / 去标签。
//
// 请求体：{"uids":[...], "tags":["主力","备用"], "mode":"add"|"remove"|"replace"}
//   - add：并入（幂等，重复打同一标签不产生重复项）
//   - remove：移除指定标签；tags 为空 = 清空这些账号的全部标签
//   - replace：整体替换为给定标签
//
// uids 为空视为**不操作**（而不是"全部账号"）：批量改标签误伤面太大，
// 前端始终显式传勾选集合，这里不放宽语义。
func (p *Panel) tagsAssign(w http.ResponseWriter, r *http.Request) {
	if p.tags == nil {
		writeErr(w, http.StatusNotImplemented, "tags store not available")
		return
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "read body: "+err.Error())
		return
	}
	var req struct {
		UIDs []string `json:"uids"`
		Tags []string `json:"tags"`
		Mode string   `json:"mode"`
	}
	if err := json.Unmarshal(raw, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "请求体解析失败: "+err.Error())
		return
	}
	if len(req.UIDs) == 0 {
		writeErr(w, http.StatusBadRequest, "uids 不能为空")
		return
	}
	mode := strings.TrimSpace(req.Mode)
	if mode == "" {
		mode = "add"
	}
	if mode != "add" && mode != "remove" && mode != "replace" {
		writeErr(w, http.StatusBadRequest, "mode 必须是 add / remove / replace")
		return
	}
	// 只接受池内真实存在的 uid：避免前端传了过期 uid 后，标签文件里出现幽灵账号。
	known := map[string]bool{}
	for _, s := range p.cfg.Pool.List() {
		known[s.UID] = true
	}
	uids := make([]string, 0, len(req.UIDs))
	skipped := 0
	for _, uid := range req.UIDs {
		if known[uid] {
			uids = append(uids, uid)
			continue
		}
		skipped++
	}
	if len(uids) == 0 {
		writeErr(w, http.StatusNotFound, "所选账号都不在池内")
		return
	}
	changed, err := p.tags.assign(uids, req.Tags, mode)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "保存标签失败: "+err.Error())
		return
	}
	log.Printf("panel: 标签 %s mode=%s 命中 %d 个账号（变更 %d，跳过 %d）", strings.Join(req.Tags, "/"), mode, len(uids), changed, skipped)
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":      true,
		"changed": changed,
		"skipped": skipped,
		"by_uid":  p.tags.Snapshot(),
		"all":     p.tags.AllTags(),
	})
}

// tagsForUIDs 返回这些账号的标签（导出用；缺省给空切片保证字段稳定）。
func (p *Panel) tagsForUIDs(uids []string) map[string][]string {
	out := map[string][]string{}
	if p.tags == nil {
		return out
	}
	snap := p.tags.Snapshot()
	for _, uid := range uids {
		if t, ok := snap[uid]; ok && len(t) > 0 {
			out[uid] = t
		}
	}
	return out
}
