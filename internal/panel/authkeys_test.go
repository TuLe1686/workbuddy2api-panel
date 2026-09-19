package panel

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
)

func newKeyedPanel(apiKey, panelKey string) *Panel {
	return New(Config{Version: "test", APIKey: apiKey, PanelKey: panelKey, Pool: pool.New("")})
}

func getOverview(p *Panel, key string) int {
	req := httptest.NewRequest("GET", "/panel/api/overview", nil)
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	return rec.Code
}

// panel_key 与 api_key 分离后：下游密钥打不开面板，管理面密钥可以。
func TestPanelUsesAdminKeyNotDownstreamKey(t *testing.T) {
	p := newKeyedPanel("downstream-key", "admin-key")
	if code := getOverview(p, "downstream-key"); code != http.StatusUnauthorized {
		t.Errorf("下游密钥打开面板 code=%d want 401", code)
	}
	if code := getOverview(p, "admin-key"); code != http.StatusOK {
		t.Errorf("管理面密钥打开面板 code=%d want 200", code)
	}
	if code := getOverview(p, ""); code != http.StatusUnauthorized {
		t.Errorf("无密钥 code=%d want 401", code)
	}
}

// 未设置 panel_key 的旧配置：面板回落 api_key（升级后原密钥照常登录）。
func TestPanelAdminKeyFallsBackToAPIKey(t *testing.T) {
	p := newKeyedPanel("only-key", "")
	if code := getOverview(p, "only-key"); code != http.StatusOK {
		t.Errorf("回落场景 code=%d want 200", code)
	}
	if code := getOverview(p, "wrong"); code != http.StatusUnauthorized {
		t.Errorf("错密钥 code=%d want 401", code)
	}
}
