package panel

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
)

func newImportPanel(t *testing.T) (*Panel, string) {
	t.Helper()
	dir := t.TempDir()
	p := New(Config{
		Version: "test",
		APIKey:  "test-key",
		AuthDir: dir,
		Pool:    pool.New(""), // 空 stateFp：不起后台落盘，测试用内存态
	})
	return p, dir
}

func postImport(t *testing.T, p *Panel, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("POST", "/panel/api/accounts/import", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer test-key")
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	return rec
}

func decodeImport(t *testing.T, rec *httptest.ResponseRecorder) (int, int, int, int, []importItemResult) {
	t.Helper()
	var got struct {
		Imported int                `json:"imported"`
		Updated  int                `json:"updated"`
		Skipped  int                `json:"skipped"`
		Failed   int                `json:"failed"`
		Items    []importItemResult `json:"items"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v body=%s", err, rec.Body.String())
	}
	return got.Imported, got.Updated, got.Skipped, got.Failed, got.Items
}

// 外部导出形态（codebuddy_cn_accounts_*.json）：snake_case + 毫秒 expires_at。
func TestParseImportExternalFlatFormat(t *testing.T) {
	item := json.RawMessage(`{
		"id":"codebuddy_cn_e0cc71fc","email":"64772617",
		"uid":"f434ab43-d3dd-4d65-96a2-eb2d1c4d876a","nickname":"70416225",
		"access_token":"AT","refresh_token":"RT","token_type":"Bearer",
		"expires_at":1794484878670,"domain":"www.codebuddy.cn",
		"dosage_notify_code":"0","payment_type":"free","status":"normal"
	}`)
	a, err := parseImportAccount(item, "")
	if err != nil {
		t.Fatal(err)
	}
	if a.UID != "f434ab43-d3dd-4d65-96a2-eb2d1c4d876a" || a.AccessToken != "AT" || a.RefreshToken != "RT" {
		t.Fatalf("mapping wrong: %+v", a)
	}
	if a.Domain != "www.codebuddy.cn" {
		t.Fatalf("domain=%q", a.Domain)
	}
	if a.ExpiresAt != 1794484878 {
		t.Fatalf("expires_at 毫秒未归一为秒: got %d want 1794484878", a.ExpiresAt)
	}
	if a.Realm() != "cn" {
		t.Fatalf("realm=%q want cn", a.Realm())
	}
}

// 秒/毫秒/字符串三形态归一 + 缺省保留 0（0 = 需刷新，由 refresh 路径接管）。
func TestParseImportExpiresAtUnits(t *testing.T) {
	cases := []struct {
		raw  string
		want int64
	}{
		{`1794484878`, 1794484878},      // 秒
		{`1794484878670`, 1794484878},   // 毫秒
		{`"1794484878670"`, 1794484878}, // 字符串毫秒
		{`0`, 0},                        // 缺省
		{`null`, 0},
	}
	for _, c := range cases {
		item := json.RawMessage(`{"uid":"u1","access_token":"AT","expires_at":` + c.raw + `}`)
		a, err := parseImportAccount(item, "")
		if err != nil {
			t.Fatalf("%s: %v", c.raw, err)
		}
		if a.ExpiresAt != c.want {
			t.Errorf("expires_at %s → %d, want %d", c.raw, a.ExpiresAt, c.want)
		}
	}
}

// 网关 auth 文件（嵌套形）原样可导入：字段零映射，realm 按请求级缺省补。
func TestParseImportNestedAuthFile(t *testing.T) {
	item := json.RawMessage(`{
		"auth":{"accessToken":"AT2","refreshToken":"RT2","expiresAt":1794484878,"domain":"www.codebuddy.cn"},
		"account":{"uid":"u-nested","enterpriseId":"e1","nickname":"昵称"}
	}`)
	a, err := parseImportAccount(item, "")
	if err != nil {
		t.Fatal(err)
	}
	if a.UID != "u-nested" || a.AccessToken != "AT2" || a.ExpiresAt != 1794484878 {
		t.Fatalf("nested parse wrong: %+v", a)
	}
	if a.Realm() != "cn" {
		t.Fatalf("realm=%q want cn", a.Realm())
	}
}

// 域判定优先级：条目 realm > 请求缺省 > domain 后缀 > id 前缀 > cn。
func TestParseImportRealmResolution(t *testing.T) {
	global := `{"auth":{"accessToken":"AT","domain":"www.workbuddy.ai"}}`
	if a, err := parseImportAccount(json.RawMessage(global), ""); err != nil || a.Realm() != "global" {
		t.Fatalf("domain 后缀判定 global 失败: realm=%v err=%v", a, err)
	}
	idGlobal := `{"uid":"u1","access_token":"AT","id":"codebuddy_global_abc"}`
	if a, err := parseImportAccount(json.RawMessage(idGlobal), ""); err != nil || a.Realm() != "global" {
		t.Fatalf("id 前缀判定 global 失败: err=%v", err)
	}
	explicit := `{"uid":"u1","access_token":"AT","realm":"global","domain":"www.codebuddy.cn"}`
	if a, err := parseImportAccount(json.RawMessage(explicit), ""); err != nil || a.Realm() != "global" {
		t.Fatalf("显式 realm 未优先: err=%v", err)
	}
	reqWins := `{"uid":"u1","access_token":"AT","id":"codebuddy_cn_abc"}`
	if a, err := parseImportAccount(json.RawMessage(reqWins), "global"); err != nil || a.Realm() != "global" {
		t.Fatalf("请求级缺省未生效: err=%v", err)
	}
}

// 缺字段必须失败且错误信息可读（不落到写盘阶段）。
func TestParseImportRejectsIncomplete(t *testing.T) {
	for name, raw := range map[string]string{
		"缺 access_token": `{"uid":"u1"}`,
		"缺 uid":          `{"access_token":"AT"}`,
		"空 access_token": `{"uid":"u1","access_token":"  "}`,
		"非对象":            `[1,2,3]`,
	} {
		if _, err := parseImportAccount(json.RawMessage(raw), ""); err == nil {
			t.Errorf("%s: 应报错", name)
		}
	}
}

// 端到端：导入 → 落盘（嵌套形 0600）→ 热加载进池 → 重复导入跳过 → 覆盖导入更新。
func TestAccountsImportEndToEnd(t *testing.T) {
	p, dir := newImportPanel(t)

	body := `[{"uid":"acct-a","access_token":"AT-a","refresh_token":"RT-a","expires_at":1794484878670,"domain":"www.codebuddy.cn","nickname":"A"},
	          {"uid":"acct-b","access_token":"AT-b","refresh_token":"RT-b","expires_at":1794484878,"nickname":"B"}]`
	rec := postImport(t, p, body)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	imp, upd, skip, fail, items := decodeImport(t, rec)
	if imp != 2 || upd != 0 || skip != 0 || fail != 0 {
		t.Fatalf("imported=%d updated=%d skipped=%d failed=%d items=%+v", imp, upd, skip, fail, items)
	}

	// 池内可查 + 凭证文件为嵌套形且 0600（重启后 LoadDir 能重新装载）。
	for _, uid := range []string{"acct-a", "acct-b"} {
		if _, ok := p.cfg.Pool.Status(uid); !ok {
			t.Fatalf("池内缺少 %s", uid)
		}
		f := filepath.Join(dir, "workbuddy-"+uid+".json")
		fi, err := os.Stat(f)
		if err != nil {
			t.Fatalf("凭证文件未落盘: %v", err)
		}
		if perm := fi.Mode().Perm(); runtime.GOOS != "windows" && perm != 0o600 {
			// Windows 不保留 POSIX 权限位（Go 回读恒 0666），故仅在 Linux/CI 上断言；
			// 生产目标是 Linux 容器，这一条保护的是真实部署面。
			t.Errorf("%s 权限 = %o, want 600", f, perm)
		}
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		var nested struct {
			Auth struct {
				AccessToken string `json:"accessToken"`
				ExpiresAt   int64  `json:"expiresAt"`
				Realm       string `json:"realm"`
			} `json:"auth"`
			Account struct {
				UID string `json:"uid"`
			} `json:"account"`
		}
		if err := json.Unmarshal(raw, &nested); err != nil {
			t.Fatalf("落盘非嵌套形: %v raw=%s", err, raw)
		}
		if nested.Account.UID != uid || nested.Auth.Realm != "cn" {
			t.Errorf("%s 落盘内容 = %+v", f, nested)
		}
		if uid == "acct-a" && nested.Auth.ExpiresAt != 1794484878 {
			t.Errorf("acct-a expiresAt=%d want 1794484878（毫秒→秒）", nested.Auth.ExpiresAt)
		}
	}

	// 重复导入（默认不覆盖）：全部跳过，计数正确，不改文件。
	rec = postImport(t, p, `[{"uid":"acct-a","access_token":"AT-a2"}]`)
	if imp, upd, skip, fail, _ := decodeImport(t, rec); imp|upd|fail != 0 || skip != 1 {
		t.Fatalf("重复导入应 skipped=1，实得 imported=%d updated=%d skipped=%d failed=%d", imp, upd, skip, fail)
	}
	raw, _ := os.ReadFile(filepath.Join(dir, "workbuddy-acct-a.json"))
	if !strings.Contains(string(raw), "AT-a") {
		t.Fatal("未开启覆盖时不该改写既有凭证")
	}

	// 覆盖导入：updated=1，凭证更新为新值。
	rec = postImport(t, p, `{"accounts":[{"uid":"acct-a","access_token":"AT-a2"}],"overwrite":true}`)
	if imp, upd, skip, fail, _ := decodeImport(t, rec); upd != 1 || imp|skip|fail != 0 {
		t.Fatalf("覆盖导入应 updated=1，实得 imported=%d updated=%d skipped=%d failed=%d", imp, upd, skip, fail)
	}
	raw, _ = os.ReadFile(filepath.Join(dir, "workbuddy-acct-a.json"))
	if !strings.Contains(string(raw), "AT-a2") {
		t.Fatal("覆盖导入未写入新凭证")
	}
}

// 单条对象形态（非数组）也要能导入：用户可能只导出/手写一个账号。
func TestAccountsImportSingleObjectBody(t *testing.T) {
	p, _ := newImportPanel(t)
	rec := postImport(t, p, `{"uid":"solo","access_token":"AT-solo","expires_at":1794484878}`)
	if imp, _, _, fail, _ := decodeImport(t, rec); imp != 1 || fail != 0 {
		t.Fatalf("单条对象导入失败: code=%d body=%s", rec.Code, rec.Body.String())
	}
}

// 非法 uid 必须被拦在写盘之前（路径穿越防护），且不计入导入成功。
func TestAccountsImportRejectsPathTraversalUID(t *testing.T) {
	p, dir := newImportPanel(t)
	rec := postImport(t, p, `[{"uid":"../../evil","access_token":"AT"}]`)
	if _, _, _, fail, items := decodeImport(t, rec); fail != 1 {
		t.Fatalf("非法 uid 应失败: %+v", items)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(dir), "evil.json")); err == nil {
		t.Fatal("路径穿越写盘未拦住")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("auths 目录不该有新文件: %v", entries)
	}
}

// 文件内重复 uid：只处理第一条，第二条报重复（防同批覆盖出不确定结果）。
func TestAccountsImportDuplicateUIDInPayload(t *testing.T) {
	p, _ := newImportPanel(t)
	body := `[{"uid":"dup","access_token":"AT1"},{"uid":"dup","access_token":"AT2"}]`
	rec := postImport(t, p, body)
	imp, _, _, fail, items := decodeImport(t, rec)
	if imp != 1 || fail != 1 {
		t.Fatalf("imported=%d failed=%d items=%+v", imp, fail, items)
	}
	if !strings.Contains(items[1].Error, "重复") {
		t.Fatalf("第二条应报重复: %+v", items[1])
	}
}

// 请求体错误形态：空体、坏 JSON、无账号数组、超上限，全部 400 且不写盘。
func TestAccountsImportBadRequests(t *testing.T) {
	p, dir := newImportPanel(t)
	cases := []struct {
		name string
		body string
		want int
	}{
		{"空体", ``, http.StatusBadRequest},
		{"坏 JSON", `{"accounts":`, http.StatusBadRequest},
		{"无账号数组", `{"foo":1}`, http.StatusBadRequest},
		{"空数组", `[]`, http.StatusBadRequest},
	}
	for _, c := range cases {
		rec := postImport(t, p, c.body)
		if rec.Code != c.want {
			t.Errorf("%s: code=%d want %d body=%s", c.name, rec.Code, c.want, rec.Body.String())
		}
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Fatalf("失败请求不该写盘: %v", entries)
	}
}

// 超过单次条数上限：整体拒绝（不部分导入），避免"导了一半"的不确定状态。
func TestAccountsImportRejectsOverLimit(t *testing.T) {
	p, dir := newImportPanel(t)
	items := make([]string, 0, importMaxAccounts+1)
	for i := 0; i <= importMaxAccounts; i++ {
		items = append(items, `{"uid":"u`+string(rune('a'+i%26))+`","access_token":"AT"}`)
	}
	rec := postImport(t, p, "["+strings.Join(items, ",")+"]")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code=%d want 400 body=%s", rec.Code, rec.Body.String())
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Fatalf("超限请求不该写盘: %v", entries)
	}
}

// 超大请求体：413（MaxBytesReader 生效），不落半截文件。
func TestAccountsImportRejectsHugeBody(t *testing.T) {
	p, dir := newImportPanel(t)
	big := `[{"uid":"u1","access_token":"` + strings.Repeat("A", importMaxBodyBytes+16) + `"}]`
	rec := postImport(t, p, big)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("code=%d want 413 body=%s", rec.Code, rec.Body.String())
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Fatalf("超限请求不该写盘: %v", entries)
	}
}

// 未带密钥必须 401（导入端点与其它面板接口同口径）。
func TestAccountsImportRequiresAuth(t *testing.T) {
	p, _ := newImportPanel(t)
	req := httptest.NewRequest("POST", "/panel/api/accounts/import", strings.NewReader(`[{"uid":"u1","access_token":"AT"}]`))
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("code=%d want 401", rec.Code)
	}
}
