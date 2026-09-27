package pool

import (
	"bytes"
	"log"
	"strings"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// TestModelLimitedFallbackPicksLeastUsed 全池「仅该模型 6004 冷却」时不再直接
// 无候选（旧语义 → handler 503）：回落当日该模型用量最少的账号放行。
func TestModelLimitedFallbackPicksLeastUsed(t *testing.T) {
	withNoPickGap(t)
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.Add(&auth.Auth{UID: "u2"})
	p.Add(&auth.Auth{UID: "u3"})
	reset := time.Now().Add(30 * time.Minute)
	for _, uid := range []string{"u1", "u2", "u3"} {
		p.CooldownSoftForModel(uid, 600*time.Second, reset, "glm-5.3", "6004 model rate limit")
	}
	p.SetModelDayUsage(func(model string) map[string]int64 {
		if model != "glm-5.3" {
			t.Errorf("探针收到意外模型 %q", model)
		}
		return map[string]int64{"u1": 10, "u2": 3, "u3": 7}
	})

	got := p.PickExcludingForModel(nil, "glm-5.3")
	if got == nil {
		t.Fatal("全池同模型冷却应回落放行（此前为 nil → 503）")
	}
	if got.UID != "u2" {
		t.Fatalf("应按当日用量最少选 u2，got %s", got.UID)
	}
	// 其他模型不受影响：三号对 hy4 均无可选障碍，正常选号。
	if got2 := p.PickExcludingForModel(nil, "hy4"); got2 == nil {
		t.Fatal("其他模型应正常可选")
	}
}

// TestModelLimitedFallbackTieBreakEarliestReset 无用量数据（探针未注入）时：
// 并列按上游重置时刻早者优先，再按 uid 稳定序。
func TestModelLimitedFallbackTieBreakEarliestReset(t *testing.T) {
	withNoPickGap(t)
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.Add(&auth.Auth{UID: "u2"})
	p.CooldownSoftForModel("u1", 600*time.Second, time.Now().Add(90*time.Minute), "glm-5.3", "6004")
	p.CooldownSoftForModel("u2", 600*time.Second, time.Now().Add(30*time.Minute), "glm-5.3", "6004")

	got := p.PickExcludingForModel(nil, "glm-5.3")
	if got == nil || got.UID != "u2" {
		t.Fatalf("并列用量应取重置更早的 u2，got %+v", got)
	}
}

// TestModelLimitedFallbackSkipsModelBlock 11102「该后端无此模型」负缓存不参与回落
// （重试是确定性失败，放行只会加深负缓存退避）：无候选维持 nil（503）。
func TestModelLimitedFallbackSkipsModelBlock(t *testing.T) {
	withNoPickGap(t)
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.BlockModelBackoff("u1", "glm-5.3", "11102 no such model")

	if got := p.PickExcludingForModel(nil, "glm-5.3"); got != nil {
		t.Fatalf("11102 条目不参与回落，want nil，got %+v", got)
	}
}

// TestModelLimitedFallbackCoolHardStillNil 全 CoolHard（余额耗尽）不参与任何兜底：
// 维持无候选（503）——放行调用必 402，空转轮换只产生噪音。
func TestModelLimitedFallbackCoolHardStillNil(t *testing.T) {
	withNoPickGap(t)
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.Add(&auth.Auth{UID: "u2"})
	p.CooldownUntilTomorrow4AM("u1", "余额不足")
	p.CooldownUntilTomorrow4AM("u2", "余额不足")

	if got := p.PickExcludingForModel(nil, "glm-5.3"); got != nil {
		t.Fatalf("全 CoolHard 应维持 nil（503），got %+v", got)
	}
}

// TestModelLimitedFallbackLayeredOrder 混冷却（账号级 + 模型级）各按各自口径：
// 账号级兜底（最早到期探针）优先于模型级回落；账号级候选耗尽（tried）后模型级接管。
func TestModelLimitedFallbackLayeredOrder(t *testing.T) {
	withNoPickGap(t)
	p := New("")
	p.Add(&auth.Auth{UID: "accountCooled"})
	p.Add(&auth.Auth{UID: "modelCooled"})
	p.Cooldown("accountCooled", CoolSoft, time.Hour, "429")
	p.CooldownSoftForModel("modelCooled", 600*time.Second, time.Now().Add(30*time.Minute), "glm-5.3", "6004")

	got := p.PickExcludingForModel(nil, "glm-5.3")
	if got == nil || got.UID != "accountCooled" {
		t.Fatalf("混冷却应保持账号级兜底优先（既有语义），got %+v", got)
	}
	got = p.PickExcludingForModel(map[string]bool{"accountCooled": true}, "glm-5.3")
	if got == nil || got.UID != "modelCooled" {
		t.Fatalf("账号级候选耗尽后应回落模型级，got %+v", got)
	}
}

// TestModelLimitedFallbackWarnThrottled 回落 WARN 同分钟只记一条（pick 高频调用不刷屏）。
func TestModelLimitedFallbackWarnThrottled(t *testing.T) {
	withNoPickGap(t)
	var buf bytes.Buffer
	old := log.Writer()
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(old) })

	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.CooldownSoftForModel("u1", 600*time.Second, time.Now().Add(30*time.Minute), "glm-5.3", "6004")
	for i := 0; i < 3; i++ {
		if got := p.PickExcludingForModel(nil, "glm-5.3"); got == nil {
			t.Fatal("应回落放行")
		}
	}
	if n := strings.Count(buf.String(), "fallback_model_rate_limited"); n != 1 {
		t.Fatalf("同分钟应只记 1 条回落 WARN，实际 %d 条\n%s", n, buf.String())
	}
	if !strings.Contains(buf.String(), "model=glm-5.3") {
		t.Fatalf("回落 WARN 应含模型名与重置时刻\n%s", buf.String())
	}
}
