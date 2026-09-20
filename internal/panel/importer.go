// importer.go 面板「导入账号文件」：把外部账号导出格式（扁平 snake_case，形如
// codebuddy_cn_accounts_*.json）归一化为网关 auth 文件（嵌套形）并热加载进池。
//
// 支持三种来源形态（同一端点容错解析）：
//
//	[a] 外部导出数组 / 单对象：{uid, access_token, refresh_token, expires_at(毫秒), domain, ...}
//	[b] 包裹对象：{"accounts":[...], "realm":"cn", "overwrite":false}
//	[c] 网关既有 auth 文件（嵌套形 {"auth":{...},"account":{...}}）——迁移旧 auths 目录用
//
// 落盘与热加载复用 OAuth 登录路径的同一语义（auth.SaveAtomic + pool.Add + Revive），
// 因此导入的账号与面板登录的账号在池内、重启后行为完全一致；不发起任何上游变更请求，
// 只在导入完成后异步刷新一次余额。
package panel

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// importMaxAccounts 单次导入条数上限：导出文件是人工产物，超过该量级更可能是
// 选错了文件；早失败早提示，避免异常输入引发写盘风暴。
const importMaxAccounts = 500

// importMaxBodyBytes 请求体上限（8MiB）：单条账号记录约 2KB，正常导出文件远小于此，
// 上限只用于挡异常/恶意输入。
const importMaxBodyBytes = 8 << 20

// importItemResult 单条导入结果。只回显 uid/nickname/域，绝不回显 token。
type importItemResult struct {
	Index    int    `json:"index"`
	UID      string `json:"uid,omitempty"`
	Nickname string `json:"nickname,omitempty"`
	Realm    string `json:"realm,omitempty"`
	Status   string `json:"status"` // imported / updated / skipped / failed
	Error    string `json:"error,omitempty"`
}

// importSummary 汇总计数（items 为逐条明细，前端只展示计数与首几条错误）。
type importSummary struct {
	Imported int
	Updated  int
	Skipped  int
	Failed   int
	Items    []importItemResult
}

// strField 取第一个非空字符串字段：键按优先级尝试，兼容 snake_case（外部导出）
// 与 camelCase（网关 auth 文件）双形态；空白字符串视为缺省。
func strField(m map[string]json.RawMessage, keys ...string) string {
	for _, k := range keys {
		raw, ok := m[k]
		if !ok {
			continue
		}
		var s string
		if err := json.Unmarshal(raw, &s); err == nil && strings.TrimSpace(s) != "" {
			return strings.TrimSpace(s)
		}
	}
	return ""
}

// epochSecondsField 解析过期时间并归一为 Unix 秒。
//
// 量级判定为什么必需：外部导出用毫秒（1794484878670），网关 auth 文件用秒
// （1794484878）——直接相互套用会把过期时间放大 1000 倍（token 永不刷新）或
// 缩小 1000 倍（每次请求都刷 token）。1e11 秒 ≈ 公元 5138 年，任何真实秒值都
// 远小于它，故 >1e11 视为毫秒。
func epochSecondsField(m map[string]json.RawMessage, keys ...string) int64 {
	for _, k := range keys {
		raw, ok := m[k]
		if !ok {
			continue
		}
		var v int64
		if err := json.Unmarshal(raw, &v); err != nil {
			var s string
			if err := json.Unmarshal(raw, &s); err != nil {
				continue
			}
			n, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
			if err != nil {
				continue
			}
			v = n
		}
		if v <= 0 {
			continue
		}
		if v > 1e11 {
			v /= 1000
		}
		return v
	}
	return 0
}

// expiryFromJWT 从 access token 的 JWT payload 里取 exp（Unix 秒）。
//
// 为什么要这条兜底：外部导出文件未必带 expires_at（或字段名/格式对不上），缺了它
// 落盘就是 expiresAt=0；而 expiresAt=0 会被 NeedsRefresh 判为"需要刷新"，于是**每个
// 聊天请求前都白刷一次 token**（多一次上游往返 + 多一次风控暴露）。token 本身就是
// JWT，读自己的 exp 即可补齐，不需要验签（这里只用于本地过期判断，不用于信任决策）。
//
// 非 JWT / 解不出来返回 0（保持"未知"，由刷新路径按需补齐）。
func expiryFromJWT(tok string) int64 {
	parts := strings.Split(tok, ".")
	if len(parts) < 2 {
		return 0
	}
	seg := strings.TrimSpace(parts[1])
	if seg == "" {
		return 0
	}
	raw, err := base64.RawURLEncoding.DecodeString(seg)
	if err != nil {
		// 容忍带 padding 的变体（部分实现会补 '='）。
		if raw, err = base64.URLEncoding.DecodeString(seg + strings.Repeat("=", (4-len(seg)%4)%4)); err != nil {
			return 0
		}
	}
	var claims struct {
		Exp int64 `json:"exp"`
	}
	if err := json.Unmarshal(raw, &claims); err != nil || claims.Exp <= 0 {
		return 0
	}
	return claims.Exp
}

// normalizeRealm 收敛 realm 取值：非 cn/global 一律返回空（由调用方走下一优先级）。
func normalizeRealm(s string) string {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "cn":
		return "cn"
	case "global":
		return "global"
	}
	return ""
}

// resolveImportRealm 判定条目归属域，优先级：
// 条目内显式 realm > 请求级缺省 realm > domain 后缀（.workbuddy.ai）> id 前缀 > cn。
//
// id 前缀是兜底信号：外部导出文件名形如 codebuddy_cn_/codebuddy_global_，
// 其 id 字段同前缀；domain 缺失时靠它区分双域。
func resolveImportRealm(m map[string]json.RawMessage, reqRealm string) string {
	if r := normalizeRealm(strField(m, "realm")); r != "" {
		return r
	}
	if r := normalizeRealm(reqRealm); r != "" {
		return r
	}
	if d := strField(m, "domain"); d != "" && auth.ResolveRealm("", d) == "global" {
		return "global"
	}
	if id := strings.ToLower(strField(m, "id")); id != "" {
		switch {
		case strings.Contains(id, "_global_"), strings.HasSuffix(id, "_global"),
			strings.Contains(id, "_intl_"), strings.HasPrefix(id, "workbuddy_"):
			return "global"
		case strings.Contains(id, "_cn_"), strings.HasSuffix(id, "_cn"):
			return "cn"
		}
	}
	return "cn"
}

// parseImportAccount 把一条外部记录归一化为 *auth.Auth（不含 FilePath）。
// reqRealm 为请求级缺省域（空 = 不指定）。
func parseImportAccount(raw json.RawMessage, reqRealm string) (*auth.Auth, error) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("不是 JSON 对象: %v", err)
	}

	// 嵌套形（网关 auth 文件原样）：直接走 auth.Parse，零字段映射。
	if _, nested := m["auth"]; nested {
		a, err := auth.Parse(raw)
		if err != nil {
			return nil, err
		}
		if a.ExpiresAt <= 0 {
			a.ExpiresAt = expiryFromJWT(a.AccessTokenValue()) // 缺过期时间则从 JWT 推
		}
		if a.RealmStored() == "" {
			if r := normalizeRealm(reqRealm); r != "" {
				if _, err := auth.BackfillRealmFor(a, r); err != nil {
					return nil, err
				}
			} else {
				a.BackfillRealm() // domain 推断；无 domain 落 cn
			}
		}
		return a, nil
	}

	// 扁平形（外部导出 / 手写）。
	uid := strField(m, "uid")
	if uid == "" {
		return nil, errors.New("缺少 uid")
	}
	at := strField(m, "access_token", "accessToken")
	if at == "" {
		return nil, errors.New("缺少 access_token")
	}
	expiresAt := epochSecondsField(m, "expires_at", "expiresAt", "expire_time", "expired_at")
	if expiresAt <= 0 {
		expiresAt = expiryFromJWT(at) // 来源没给过期时间 → 从 token 自身推
	}
	a := &auth.Auth{
		AccessToken:  at,
		RefreshToken: strField(m, "refresh_token", "refreshToken"),
		ExpiresAt:    expiresAt,
		Domain:       strField(m, "domain"),
		UID:          uid,
		EnterpriseID: strField(m, "enterprise_id", "enterpriseId"),
		Nickname:     strField(m, "nickname"),
	}
	if _, err := auth.BackfillRealmFor(a, resolveImportRealm(m, reqRealm)); err != nil {
		return nil, err
	}
	return a, nil
}

// parseImportBody 解析请求体：数组直读；对象取 accounts 字段；对象本身是单条
// 账号（含 access_token / auth 键）时视为单元素数组。
//
// tags 为请求级标签：这一批（未自带 tags 的条目）统一打上的运营标记。
func parseImportBody(body []byte) (items []json.RawMessage, realm string, overwrite bool, tags []string, err error) {
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 {
		return nil, "", false, nil, errors.New("请求体为空")
	}
	if trimmed[0] == '[' {
		if err := json.Unmarshal(trimmed, &items); err != nil {
			return nil, "", false, nil, fmt.Errorf("账号数组解析失败: %v", err)
		}
		return items, "", false, nil, nil
	}
	var env struct {
		Accounts  []json.RawMessage `json:"accounts"`
		Realm     string            `json:"realm"`
		Overwrite bool              `json:"overwrite"`
		Tags      []string          `json:"tags"`
	}
	if err := json.Unmarshal(trimmed, &env); err != nil {
		return nil, "", false, nil, fmt.Errorf("请求体解析失败: %v", err)
	}
	if len(env.Accounts) == 0 {
		var probe map[string]json.RawMessage
		if json.Unmarshal(trimmed, &probe) == nil {
			for _, k := range []string{"access_token", "accessToken", "auth"} {
				if _, ok := probe[k]; ok {
					return []json.RawMessage{trimmed}, env.Realm, env.Overwrite, env.Tags, nil
				}
			}
		}
	}
	return env.Accounts, env.Realm, env.Overwrite, env.Tags, nil
}

// itemTags 读取条目自带的 tags（导出文件会写它，导入据此回填标签）。
// 条目没写 tags 时返回空，由请求级 tags 兜底。
func itemTags(raw json.RawMessage) []string {
	var probe struct {
		Tags []string `json:"tags"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil {
		return nil
	}
	return normalizeTags(probe.Tags)
}

// accountsImport 导入账号文件：解析 → 校验 → 落盘 auths/workbuddy-<uid>.json →
// 热加载进池（免重启）。
//
// 已存在同 uid 的账号默认跳过（overwrite=true 时以文件内新凭证覆盖并复活）。
// 与 OAuth 登录一致：UID 经 validUID 白名单校验后再拼文件名（防路径穿越）。
func (p *Panel) accountsImport(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, importMaxBodyBytes))
	if err != nil {
		writeErr(w, http.StatusRequestEntityTooLarge, "请求体读取失败（上限 8MiB）")
		return
	}
	items, reqRealm, overwrite, reqTags, err := parseImportBody(body)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if len(items) == 0 {
		writeErr(w, http.StatusBadRequest, `未找到账号数组：支持 [...] 或 {"accounts":[...]}`)
		return
	}
	if len(items) > importMaxAccounts {
		writeErr(w, http.StatusBadRequest,
			fmt.Sprintf("单次导入上限 %d 条（本次 %d 条），请拆分文件", importMaxAccounts, len(items)))
		return
	}
	if err := os.MkdirAll(p.cfg.AuthDir, 0o755); err != nil {
		writeErr(w, http.StatusInternalServerError, "mkdir auth dir: "+err.Error())
		return
	}

	sum := importSummary{Items: make([]importItemResult, 0, len(items))}
	seen := make(map[string]bool, len(items))
	var importedUIDs []string
	tagAssign := map[string][]string{} // uid → 本批要打的标签（条目级优先）

	for i, item := range items {
		it := importItemResult{Index: i, Status: "failed"}
		a, err := parseImportAccount(item, reqRealm)
		if err != nil {
			it.Error = err.Error()
			sum.Failed++
			sum.Items = append(sum.Items, it)
			continue
		}
		it.UID, it.Nickname, it.Realm = a.UID, a.Nickname, a.Realm()
		if !validUID(a.UID) {
			it.Error = "uid 缺失或含非法字符，拒绝落盘（防路径穿越）"
			sum.Failed++
			sum.Items = append(sum.Items, it)
			continue
		}
		if seen[a.UID] {
			it.Error = "文件内重复 uid（前一条已处理）"
			sum.Failed++
			sum.Items = append(sum.Items, it)
			continue
		}
		seen[a.UID] = true

		_, exists := p.cfg.Pool.Status(a.UID)
		if exists && !overwrite {
			it.Status = "skipped"
			it.Error = "账号已存在（未开启覆盖）"
			sum.Skipped++
			sum.Items = append(sum.Items, it)
			continue
		}

		a.FilePath = filepath.Join(p.cfg.AuthDir, fmt.Sprintf("workbuddy-%s.json", a.UID))
		if err := a.SaveAtomic(); err != nil {
			it.Error = "写凭证失败: " + err.Error()
			sum.Failed++
			sum.Items = append(sum.Items, it)
			continue
		}
		p.cfg.Pool.Add(a)
		p.cfg.Pool.Revive(a.UID) // 导入是人工动作：清掉旧号遗留的禁用/冷却/熔断
		importedUIDs = append(importedUIDs, a.UID)
		if ts := itemTags(item); len(ts) > 0 {
			tagAssign[a.UID] = ts
		} else if len(reqTags) > 0 {
			tagAssign[a.UID] = normalizeTags(reqTags)
		}
		if exists {
			it.Status = "updated"
			sum.Updated++
		} else {
			it.Status = "imported"
			sum.Imported++
		}
		sum.Items = append(sum.Items, it)
	}

	// 标签：导入这一批统一打标（条目自带 tags 优先，否则用请求级 tags）。
	// 标签只影响面板运营视图，写失败不改变导入结果，只记日志。
	if p.tags != nil && len(tagAssign) > 0 {
		tagged := 0
		for uid, ts := range tagAssign {
			if _, err := p.tags.assign([]string{uid}, ts, "add"); err != nil {
				log.Printf("panel: 导入后打标签 uid=%s 失败: %v", uid, err)
				continue
			}
			tagged++
		}
		log.Printf("panel: 导入后打标签 %d 个账号（标签：%s）", tagged, strings.Join(reqTags, "/"))
	}

	// 余额刷新异步执行：接口立即返回，面板下一次 overview 轮询即见真实积分。
	if len(importedUIDs) > 0 && p.cfg.Upstream != nil {
		go p.refreshImportedBalances(importedUIDs)
	}

	realmLabel := normalizeRealm(reqRealm)
	if realmLabel == "" {
		realmLabel = "auto"
	}
	log.Printf("panel: 导入账号 imported=%d updated=%d skipped=%d failed=%d（realm=%s overwrite=%v）",
		sum.Imported, sum.Updated, sum.Skipped, sum.Failed, realmLabel, overwrite)
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":       true,
		"imported": sum.Imported,
		"updated":  sum.Updated,
		"skipped":  sum.Skipped,
		"failed":   sum.Failed,
		"items":    sum.Items,
	})
}

// refreshImportedBalances 导入后异步刷新余额（并发上限 3）：新导入账号在池内
// 初始积分为 0，刷新后才反映真实值（影响选号权重与面板展示）。失败只记日志，
// 不改变导入结果。
func (p *Panel) refreshImportedBalances(uids []string) {
	sem := make(chan struct{}, 3)
	var wg sync.WaitGroup
	for _, uid := range uids {
		wg.Add(1)
		go func(uid string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			a := p.cfg.Pool.AuthByUID(uid)
			if a == nil {
				return
			}
			remain, total, err := p.cfg.Upstream.UserResource(a)
			if err != nil {
				log.Printf("panel: 导入后刷新余额 uid=%s 失败: %v", uid, err)
				return
			}
			p.cfg.Pool.SetCredits(uid, remain, total)
			log.Printf("panel: 导入后刷新余额 uid=%s credits=%d/%d", uid, remain, total)
		}(uid)
	}
	wg.Wait()
}
