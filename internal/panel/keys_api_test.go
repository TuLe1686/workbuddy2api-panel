package panel

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/apikeys"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
)

func newKeysPanel() (*Panel, *apikeys.Store) {
	keys := apikeys.New("")
	return New(Config{Version: "test", PanelKey: "admin", Pool: pool.New(""), Keys: keys}), keys
}

func callKeys(p *Panel, method, path, body string) (*httptest.ResponseRecorder, map[string]any) {
	var rd *bytes.Reader
	if body == "" {
		rd = bytes.NewReader(nil)
	} else {
		rd = bytes.NewReader([]byte(body))
	}
	req := httptest.NewRequest(method, path, rd)
	req.Header.Set("Authorization", "Bearer admin")
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec, out
}

// 管理面密钥才能管理 keys；签发出的下游 key 打不开 keys 接口。
func TestKeysAPIRequiresAdminKey(t *testing.T) {
	p, keys := newKeysPanel()
	k, err := keys.Create("下游A", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	// 下游 key 调 keys 列表 → 401（面板 API 只认管理面密钥）
	req := httptest.NewRequest("GET", "/panel/api/keys", nil)
	req.Header.Set("Authorization", "Bearer "+k.Secret)
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("下游 key 管理 keys code=%d want 401", rec.Code)
	}
	if r, _ := callKeys(p, "GET", "/panel/api/keys", ""); r.Code != http.StatusOK {
		t.Errorf("管理面密钥 code=%d want 200", r.Code)
	}
}

// 创建 → 列表可见 → 更新限额 → 清零 → 删除 全链路。
func TestKeysCRUDFlow(t *testing.T) {
	p, _ := newKeysPanel()
	rec, out := callKeys(p, "POST", "/panel/api/keys", `{"name":"客户X","max_credits":50,"max_tokens":100000}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("create code=%d body=%s", rec.Code, rec.Body)
	}
	keyObj, _ := out["key"].(map[string]any)
	if keyObj == nil {
		t.Fatalf("create 应返回 key: %s", rec.Body)
	}
	id, _ := keyObj["id"].(string)
	secret, _ := keyObj["key"].(string)
	if id == "" || secret == "" {
		t.Fatalf("key 对象缺 id/key: %v", keyObj)
	}
	if mc, _ := keyObj["max_credits"].(float64); mc != 50 {
		t.Errorf("max_credits=%v want 50", mc)
	}

	// 更新限额（改小）+ 名字
	rec, _ = callKeys(p, "POST", "/panel/api/keys/"+id, `{"name":"客户X改","max_credits":10}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("update code=%d body=%s", rec.Code, rec.Body)
	}
	r2, out2 := callKeys(p, "GET", "/panel/api/keys", "")
	if r2.Code != http.StatusOK {
		t.Fatalf("list code=%d", r2.Code)
	}
	list, _ := out2["keys"].([]any)
	if len(list) != 1 {
		t.Fatalf("列表应有 1 把: %v", out2)
	}
	got := list[0].(map[string]any)
	if n, _ := got["name"].(string); n != "客户X改" {
		t.Errorf("name=%q", n)
	}
	if mc, _ := got["max_credits"].(float64); mc != 10 {
		t.Errorf("max_credits=%v want 10", mc)
	}

	// 空体更新 = 全不改
	rec, _ = callKeys(p, "POST", "/panel/api/keys/"+id, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("空体更新失败: %d", rec.Code)
	}
	_, out3 := callKeys(p, "GET", "/panel/api/keys", "")
	if g := out3["keys"].([]any)[0].(map[string]any); g["max_credits"].(float64) != 10 {
		t.Errorf("空体不应改限额: %v", g["max_credits"])
	}

	// 清零统计（先记账再清）——直接清零接口验证
	if r, _ := callKeys(p, "POST", "/panel/api/keys/"+id+"/reset", ""); r.Code != http.StatusOK {
		t.Fatalf("reset code=%d", r.Code)
	}
	// 删除
	if r, _ := callKeys(p, "POST", "/panel/api/keys/"+id+"/remove", ""); r.Code != http.StatusOK {
		t.Fatalf("remove code=%d", r.Code)
	}
	_, out4 := callKeys(p, "GET", "/panel/api/keys", "")
	if l := out4["keys"].([]any); len(l) != 0 {
		t.Errorf("删除后应为空: %v", l)
	}
}

// 非法请求体 / 负限额 → 400；不存在 id → 404。
func TestKeysValidation(t *testing.T) {
	p, _ := newKeysPanel()
	if r, _ := callKeys(p, "POST", "/panel/api/keys", `{"max_credits":-5}`); r.Code != http.StatusBadRequest {
		t.Errorf("负积分限额 code=%d want 400", r.Code)
	}
	if r, _ := callKeys(p, "POST", "/panel/api/keys", `{bad json`); r.Code != http.StatusBadRequest {
		t.Errorf("坏 JSON code=%d want 400", r.Code)
	}
	if r, _ := callKeys(p, "POST", "/panel/api/keys/kid-nope", `{"name":"x"}`); r.Code != http.StatusNotFound {
		t.Errorf("不存在 id code=%d want 404", r.Code)
	}
	if r, _ := callKeys(p, "POST", "/panel/api/keys/kid-nope/reset", ""); r.Code != http.StatusNotFound {
		t.Errorf("reset 不存在 id code=%d want 404", r.Code)
	}
}

// 未装配 Keys（nil）：接口 501 而不是 panic。
func TestKeysStoreNilReturns501(t *testing.T) {
	p := New(Config{Version: "test", PanelKey: "admin", Pool: pool.New("")})
	if r, _ := callKeys(p, "GET", "/panel/api/keys", ""); r.Code != http.StatusNotImplemented {
		t.Errorf("nil store code=%d want 501", r.Code)
	}
}
