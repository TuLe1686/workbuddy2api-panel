package panel

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// fakeJWT 生成一个只有 payload 有意义的 JWT（exp 用给定值）——只用于本地过期判断测试。
func fakeJWT(t *testing.T, exp int64) string {
	t.Helper()
	enc := func(v any) string {
		raw, _ := json.Marshal(v)
		return base64.RawURLEncoding.EncodeToString(raw)
	}
	return enc(map[string]string{"alg": "RS256", "typ": "JWT"}) + "." +
		enc(map[string]any{"exp": exp, "sub": "u"}) + ".sig"
}

// JWT exp 解析：正常值取到；非 JWT / 空 / 段数不足 / payload 非法一律 0（不猜）。
func TestExpiryFromJWT(t *testing.T) {
	if got := expiryFromJWT(fakeJWT(t, 1794484878)); got != 1794484878 {
		t.Fatalf("exp=%d want 1794484878", got)
	}
	for _, bad := range []string{"", "not-a-jwt", "a.b", "a.@@@.c", "only-one-part"} {
		if got := expiryFromJWT(bad); got != 0 {
			t.Errorf("非法 token %q 应返回 0，得到 %d", bad, got)
		}
	}
	// 带 padding 的变体也要能解出来
	raw := base64.URLEncoding.EncodeToString([]byte(`{"exp":1700000000}`))
	padded := "h." + raw + ".s"
	if got := expiryFromJWT(padded); got != 1700000000 {
		t.Errorf("带 padding 的 payload 应解析成功，得到 %d", got)
	}
}

// 导入时来源没给过期时间 → 从 token 的 exp 补齐（否则 expiresAt=0 会让每个请求都白刷 token）。
func TestImportDerivesExpiryFromToken(t *testing.T) {
	tok := fakeJWT(t, 1794484878)
	item := json.RawMessage(`{"uid":"jx-1","access_token":"` + tok + `"}`)
	a, err := parseImportAccount(item, "")
	if err != nil {
		t.Fatal(err)
	}
	if a.ExpiresAt != 1794484878 {
		t.Fatalf("应从 JWT 推导过期时间，得到 %d", a.ExpiresAt)
	}

	// 显式 expires_at（毫秒）优先于 JWT
	item2 := json.RawMessage(`{"uid":"jx-2","access_token":"` + tok + `","expires_at":1800000000000}`)
	a2, err := parseImportAccount(item2, "")
	if err != nil {
		t.Fatal(err)
	}
	if a2.ExpiresAt != 1800000000 {
		t.Fatalf("显式 expires_at 应优先，得到 %d", a2.ExpiresAt)
	}

	// 另一组别名（expire_time）也认
	item3 := json.RawMessage(`{"uid":"jx-3","access_token":"` + tok + `","expire_time":1811111111}`)
	a3, err := parseImportAccount(item3, "")
	if err != nil {
		t.Fatal(err)
	}
	if a3.ExpiresAt != 1811111111 {
		t.Fatalf("expire_time 别名应生效，得到 %d", a3.ExpiresAt)
	}
}

// newHealthUpstream 用测试服务器扮演上游余额接口：token 决定 401 还是正常信封。
func newHealthUpstream(t *testing.T) *upstream.Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if tok == "dead-token" {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte("<html><head><title>401 Authorization Required</title></head><body>openresty</body></html>"))
			return
		}
		if tok == "flaky-token" {
			w.WriteHeader(http.StatusBadGateway)
			_, _ = w.Write([]byte("<html>502</html>"))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":0,"msg":"","data":{"Response":{"Data":{"Accounts":[` +
			`{"CapacityRemain":120,"CapacityUsed":30,"CapacitySize":150,` +
			`"CycleCapacityRemain":120,"CycleCapacityUsed":30,"CycleCapacitySize":150}]}}}}`))
	}))
	t.Cleanup(srv.Close)
	up := upstream.New()
	up.BillingBaseCN = srv.URL
	up.HTTP = srv.Client()
	return up
}

func newHealthPanel(t *testing.T) (*Panel, string) {
	t.Helper()
	dir := t.TempDir()
	authDir := filepath.Join(dir, "auths")
	if err := os.MkdirAll(authDir, 0o755); err != nil {
		t.Fatal(err)
	}
	p := New(Config{
		Version: "test", APIKey: "test-key",
		AuthDir:    authDir,
		TagFile:    filepath.Join(dir, "tags.json"),
		HealthFile: filepath.Join(dir, "health.json"),
		Pool:       pool.New(""),
		Upstream:   newHealthUpstream(t),
	})
	return p, dir
}

func callPanelAPI(t *testing.T, p *Panel, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer test-key")
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	return rec
}

// 体检三分类 + 有效号余额回写 + 行内标记。
func TestHealthCheckClassifiesAccounts(t *testing.T) {
	p, dir := newHealthPanel(t)
	seedAuth(t, p, dir, "good-uid", "好号", "cn", "www.codebuddy.cn", "good-token")
	seedAuth(t, p, dir, "dead-uid", "失效号", "cn", "www.codebuddy.cn", "dead-token")
	seedAuth(t, p, dir, "flaky-uid", "抖动号", "cn", "www.codebuddy.cn", "flaky-token")

	rec := callPanelAPI(t, p, "POST", "/panel/api/accounts/healthcheck", `{}`)
	if rec.Code != 200 {
		t.Fatalf("启动体检 code=%d body=%s", rec.Code, rec.Body.String())
	}
	// 并发体检已在跑时再次启动：409（不重复锤上游）
	if rec2 := callPanelAPI(t, p, "POST", "/panel/api/accounts/healthcheck", `{}`); rec2.Code != 409 && rec2.Code != 200 {
		t.Fatalf("重复启动 code=%d want 409/200", rec2.Code)
	}

	// 等体检结束
	deadline := time.Now().Add(15 * time.Second)
	var stat struct {
		Running bool           `json:"running"`
		OKCount int            `json:"ok_count"`
		Invalid int            `json:"invalid"`
		Unknown int            `json:"unknown"`
		Items   []healthResult `json:"items"`
	}
	for {
		rec = callPanelAPI(t, p, "GET", "/panel/api/accounts/healthcheck", "")
		if err := json.Unmarshal(rec.Body.Bytes(), &stat); err != nil {
			t.Fatal(err)
		}
		if !stat.Running {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("体检超时未结束")
		}
		time.Sleep(80 * time.Millisecond)
	}
	if stat.OKCount != 1 || stat.Invalid != 1 || stat.Unknown != 1 {
		t.Fatalf("分类结果 ok=%d invalid=%d unknown=%d want 1/1/1（items=%+v）",
			stat.OKCount, stat.Invalid, stat.Unknown, stat.Items)
	}
	if got := p.healthForUID("dead-uid"); got != "invalid" {
		t.Errorf("失效号标记=%q want invalid", got)
	}
	if got := p.healthForUID("flaky-uid"); got != "unknown" {
		t.Errorf("抖动号应为 unknown（不作结论），得到 %q", got)
	}
	// 有效号顺带回写余额（体检复用余额接口）
	if st, ok := p.cfg.Pool.Status("good-uid"); !ok || st.Credits != 120 {
		t.Errorf("有效号余额应回写 120，得到 %+v", st)
	}
	// 失效号的余额仍是 0（没有可用数据，不该瞎写）
	if st, _ := p.cfg.Pool.Status("dead-uid"); st.Credits != 0 {
		t.Errorf("失效号余额应保持 0，得到 %d", st.Credits)
	}

	// 结论落盘 → 新面板实例能载入（重启后仍能标红）
	raw, err := os.ReadFile(filepath.Join(dir, "health.json"))
	if err != nil {
		t.Fatalf("体检结论未落盘: %v", err)
	}
	if !strings.Contains(string(raw), "dead-uid") {
		t.Errorf("落盘内容缺失效号: %s", string(raw)[:200])
	}
	p2 := New(Config{Version: "test", APIKey: "test-key", HealthFile: filepath.Join(dir, "health.json"), Pool: pool.New("")})
	p2.loadHealth()
	if got := p2.healthForUID("dead-uid"); got != "invalid" {
		t.Errorf("重新载入后失效标记=%q want invalid", got)
	}
}

// 按 uid 移除：无 confirm 拒绝；带 confirm 清池 + 删文件 + 清标签与体检结论。
func TestRemoveSelectedAccounts(t *testing.T) {
	p, dir := newHealthPanel(t)
	seedAuth(t, p, dir, "dead-uid", "失效号", "cn", "www.codebuddy.cn", "dead-token")
	_, _ = p.tags.assign([]string{"dead-uid"}, []string{"待清理"}, "add")
	p.health.mu.Lock()
	p.health.results["dead-uid"] = healthResult{UID: "dead-uid", Status: "invalid"}
	p.health.mu.Unlock()

	if rec := callPanelAPI(t, p, "POST", "/panel/api/accounts/remove_selected", `{"uids":["dead-uid"]}`); rec.Code != 400 {
		t.Errorf("无 confirm code=%d want 400", rec.Code)
	}
	if _, ok := p.cfg.Pool.Status("dead-uid"); !ok {
		t.Fatal("无 confirm 不该移除账号")
	}

	rec := callPanelAPI(t, p, "POST", "/panel/api/accounts/remove_selected",
		`{"uids":["dead-uid","ghost-uid"],"confirm":true}`)
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Removed int `json:"removed"`
		Missing int `json:"missing"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Removed != 1 || resp.Missing != 1 {
		t.Errorf("removed=%d missing=%d want 1/1", resp.Removed, resp.Missing)
	}
	if _, ok := p.cfg.Pool.Status("dead-uid"); ok {
		t.Error("移除后账号仍能查到")
	}
	if _, err := os.Stat(filepath.Join(dir, "workbuddy-dead-uid.json")); !os.IsNotExist(err) {
		t.Error("凭证文件未删除")
	}
	if _, ok := p.tags.Snapshot()["dead-uid"]; ok {
		t.Error("标签未随账号清理")
	}
	if got := p.healthForUID("dead-uid"); got != "" {
		t.Errorf("体检结论未随账号清理，得到 %q", got)
	}
}

// 空 uids 体检：拒绝（避免语义含糊），与"全量"用缺省字段区分。
func TestHealthCheckEmptyUIDsRejected(t *testing.T) {
	p, _ := newHealthPanel(t)
	if rec := callPanelAPI(t, p, "POST", "/panel/api/accounts/healthcheck", `{"uids":[]}`); rec.Code != 400 {
		t.Errorf("空池体检 code=%d want 400（没有可体检的账号）", rec.Code)
	}
	// 未鉴权：401
	req := httptest.NewRequest("GET", "/panel/api/accounts/healthcheck", nil)
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("未鉴权 code=%d want 401", rec.Code)
	}
}
