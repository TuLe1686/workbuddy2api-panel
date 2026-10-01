package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/apikeys"
	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// sseWithCredit 同 sseOK，末帧 usage 带 credit（积分消耗明细断言用）。
const sseWithCredit = "data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"created\":1753600000,\"model\":\"glm-5.2\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"hi\"}}]}\n\n" +
	"data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"created\":1753600000,\"model\":\"glm-5.2\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":100,\"completion_tokens\":50,\"total_tokens\":150,\"credit\":2.5}}\n\n" +
	"data: [DONE]\n\n"

func newKeyedHandler(t *testing.T, apiKey string, keys *apikeys.Store, behavior func(string) (int, string, bool)) *Handler {
	t.Helper()
	return NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: newFakeUpstream(t, behavior),
		APIKey:   apiKey,
		Keys:     keys,
	})
}

func postChat(h *Handler, key string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[]}`))
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// 多 key 启用后：静态 api_key 与面板签发 key 都能通过，错误 key 401。
func TestMultiKeyAuthAcceptsStaticAndIssued(t *testing.T) {
	keys := apikeys.New("")
	k, err := keys.Create("客户A", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	h := newKeyedHandler(t, "static-key", keys, func(string) (int, string, bool) {
		return 200, sseOK, true
	})
	if code := postChat(h, "static-key").Code; code != 200 {
		t.Errorf("静态 key code=%d want 200", code)
	}
	if code := postChat(h, k.Secret).Code; code != 200 {
		t.Errorf("签发 key code=%d want 200", code)
	}
	if code := postChat(h, "wrong").Code; code != http.StatusUnauthorized {
		t.Errorf("错误 key code=%d want 401", code)
	}
}

// 只有仓库 key、没配静态 api_key：key 生效，缺 key 的请求 401。
func TestMultiKeyAuthWithoutStaticKey(t *testing.T) {
	keys := apikeys.New("")
	k, _ := keys.Create("无静态", 0, 0)
	h := newKeyedHandler(t, "", keys, func(string) (int, string, bool) {
		return 200, sseOK, true
	})
	if code := postChat(h, "").Code; code != http.StatusUnauthorized {
		t.Errorf("缺 key code=%d want 401", code)
	}
	if code := postChat(h, k.Secret).Code; code != 200 {
		t.Errorf("签发 key code=%d want 200", code)
	}
}

// 仓库为 nil：与旧版逐字节同路径（仅静态 key）。
func TestKeysNilKeepsLegacyBehavior(t *testing.T) {
	h := newKeyedHandler(t, "static-key", nil, func(string) (int, string, bool) {
		return 200, sseOK, true
	})
	if code := postChat(h, "static-key").Code; code != 200 {
		t.Errorf("静态 key code=%d want 200", code)
	}
	if code := postChat(h, "other").Code; code != http.StatusUnauthorized {
		t.Errorf("其他 key code=%d want 401", code)
	}
}

// 静态 api_key 不参与限额：消耗再多也放行。
func TestStaticKeyNotQuotaChecked(t *testing.T) {
	keys := apikeys.New("")
	k, _ := keys.Create("受限", 1, 1)
	// 先把签发 key 打满
	keys.NoteUsage(k.ID, 999, true, 999, true)
	h := newKeyedHandler(t, "static-key", keys, func(string) (int, string, bool) {
		return 200, sseOK, true
	})
	if code := postChat(h, "static-key").Code; code != 200 {
		t.Errorf("静态 key 不应受限 code=%d", code)
	}
}

// 限额达成的 key：新请求 429 key_quota_exceeded，且计入请求台账。
func TestQuotaExceededReturns429(t *testing.T) {
	keys := apikeys.New("")
	k, _ := keys.Create("打满", 10, 0)
	keys.NoteUsage(k.ID, 100, true, 10.5, true) // Requests 已是 1
	h := newKeyedHandler(t, "static-key", keys, func(string) (int, string, bool) {
		return 200, sseOK, true
	})
	rec := postChat(h, k.Secret)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("code=%d want 429 body=%s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "key_quota_exceeded") {
		t.Errorf("响应应带 key_quota_exceeded: %s", rec.Body)
	}
	// 被拒请求也计一次尝试（NoteUsage 预置 1 次 + 被拒 1 次 = 2）。
	got := keys.List()
	if len(got) != 1 || got[0].Requests != 2 {
		t.Errorf("被拒请求应计入台账: %+v", got)
	}
}

// 成功请求的记账：token/credit 进入 key 台账（credit 来自上游 usage.credit）。
func TestKeyLedgerRecordsCreditAndTokens(t *testing.T) {
	keys := apikeys.New("")
	k, _ := keys.Create("记账", 0, 0)
	h := newKeyedHandler(t, "static-key", keys, func(string) (int, string, bool) {
		return 200, sseWithCredit, true
	})
	if code := postChat(h, k.Secret).Code; code != 200 {
		t.Fatalf("code=%d", code)
	}
	got := keys.List()[0]
	if got.Tokens != 150 {
		t.Errorf("tokens=%d want 150", got.Tokens)
	}
	if got.Credits != 2.5 {
		t.Errorf("credits=%v want 2.5", got.Credits)
	}
	if got.Requests != 1 {
		t.Errorf("requests=%d want 1", got.Requests)
	}
}

// 日志行带 key 列与 credit 列（积分消耗明细）。
func TestChatLogsRowHasKeyAndCredit(t *testing.T) {
	withChatLog(t)
	keys := apikeys.New("")
	k, _ := keys.Create("客户B", 0, 0)
	h := newKeyedHandler(t, "static-key", keys, func(string) (int, string, bool) {
		return 200, sseWithCredit, true
	})
	out := captureStdout(t, func() {
		if code := postChat(h, k.Secret).Code; code != 200 {
			t.Fatalf("code=%d", code)
		}
	})
	for _, want := range []string{"客户B", "credit=2.50cr", "tok=50"} {
		if !strings.Contains(out, want) {
			t.Errorf("row missing %q:\n%s", want, out)
		}
	}
	// 静态 key 请求：key 列显示 "-"
	out2 := captureStdout(t, func() {
		if code := postChat(h, "static-key").Code; code != 200 {
			t.Fatalf("code=%d", code)
		}
	})
	if !strings.Contains(out2, "| -            |") && !strings.Contains(out2, " -        | TTFB") {
		// key 列为 "-" 即可（宽度填充细节不严格断言，仅要求不含 key 名）
		if strings.Contains(out2, "客户B") {
			t.Errorf("静态 key 行不应带签发 key 名:\n%s", out2)
		}
	}
}

// token 限额（无 credit）路径：按 total tokens 拒绝。
func TestQuotaTokensExceeded(t *testing.T) {
	keys := apikeys.New("")
	k, _ := keys.Create("token限", 0, 100)
	keys.NoteUsage(k.ID, 100, true, 0, false)
	h := newKeyedHandler(t, "static-key", keys, func(string) (int, string, bool) {
		return 200, sseOK, true
	})
	if code := postChat(h, k.Secret).Code; code != http.StatusTooManyRequests {
		t.Errorf("token 超限 code=%d want 429", code)
	}
}
