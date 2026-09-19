package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// 管理面密钥（panel_key）与下游密钥（api_key）必须完全分离：
// 拿到下游密钥的一方能调 /v1/*，但打不开 /status（账号池明细）；反过来管理面
// 密钥也不被下游接口接受——两侧不能互相顶替，否则"分发出去的密钥"就等于控制台。
func TestPanelKeySeparatesAdminFromDownstreamKey(t *testing.T) {
	p := testPoolWith(&auth.Auth{UID: "u1", Nickname: "nick", AccessToken: "at", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: upstream.New(), APIKey: "downstream-key", PanelKey: "admin-key"})

	cases := []struct {
		name string
		path string
		key  string
		want int
	}{
		{"下游密钥调 /v1", "/v1/models", "downstream-key", http.StatusOK},
		{"下游密钥调 /status 应被拒", "/status", "downstream-key", http.StatusUnauthorized},
		{"管理面密钥调 /status", "/status", "admin-key", http.StatusOK},
		{"管理面密钥调 /v1 应被拒", "/v1/models", "admin-key", http.StatusUnauthorized},
		{"无密钥调 /status", "/status", "", http.StatusUnauthorized},
	}
	for _, c := range cases {
		req := httptest.NewRequest("GET", c.path, nil)
		if c.key != "" {
			req.Header.Set("Authorization", "Bearer "+c.key)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != c.want {
			t.Errorf("%s: code=%d want %d body=%s", c.name, rec.Code, c.want, strings.TrimSpace(rec.Body.String()))
		}
	}
}

// 未设置 panel_key 的旧配置：管理面回落 api_key，行为与升级前一致（零回归）。
func TestAdminFallsBackToAPIKeyWhenPanelKeyEmpty(t *testing.T) {
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: upstream.New(), APIKey: "only-key"})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/status", nil)
	req.Header.Set("Authorization", "Bearer only-key")
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("回落场景 /status code=%d want 200 body=%s", rec.Code, strings.TrimSpace(rec.Body.String()))
	}
}
