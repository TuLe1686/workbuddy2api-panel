package pool

import (
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// 高额号判定：剩余占比 **严格大于** 阈值才排除（等于阈值不排除）；总额未知不排除；
// 关掉策略后一律不判；阈值可调。
func TestHighCreditsThreshold(t *testing.T) {
	p := New("")
	cases := []struct {
		credits, total int64
		want           bool
	}{
		{100, 100, true}, // 满额
		{96, 100, true},  // 96% > 95%
		{95, 100, false}, // 恰好 95%：不排除（严格大于）
		{94, 100, false}, //
		{50, 0, false},   // 总额未知（旧状态/查询失败）不排除
		{0, 0, false},    //
		{0, 100, false},  // 额度耗尽：不是高额号（会走别的健康口径）
	}
	for _, c := range cases {
		if got := p.highCreditsLocked(&entry{credits: c.credits, creditsTotal: c.total}); got != c.want {
			t.Errorf("credits=%d total=%d → %v want %v", c.credits, c.total, got, c.want)
		}
	}

	p.SetExcludeHighCredits(false, 0.95)
	if p.highCreditsLocked(&entry{credits: 100, creditsTotal: 100}) {
		t.Error("关闭策略后不应判为高额号")
	}
	p.SetExcludeHighCredits(true, 50.0/100)
	if !p.highCreditsLocked(&entry{credits: 60, creditsTotal: 100}) {
		t.Error("阈值 50% 时 60% 应判为高额号")
	}
	p.SetExcludeHighCredits(true, 0) // 非法阈值 → 保留内置默认 95%
	if !p.highCreditsLocked(&entry{credits: 100, creditsTotal: 100}) {
		t.Error("非法阈值应回退默认 95%")
	}
}

// 全池高额时回退为不排除：否则候选集为空、请求全部 503（最危险的回归）。
func TestHighCreditsFallbackWhenAllHigh(t *testing.T) {
	withNoPickGap(t)
	p := New("")
	p.Add(&auth.Auth{UID: "a"})
	p.Add(&auth.Auth{UID: "b"})
	p.SetCredits("a", 100, 100)
	p.SetCredits("b", 99, 100)
	p.SetRandomSource(func(n int64) int64 { return 0 })
	if got := p.Pick(); got == nil {
		t.Fatal("全池高额时应回退为可用，而不是没有候选（否则请求全 503）")
	}
	if uids := p.AvailableUIDs(); len(uids) != 2 {
		t.Fatalf("全池高额时 AvailableUIDs=%v want 2 个（会话分配同样回退）", uids)
	}
}

// 池内尚有低额号时：高额号被排除，选号与会话分配都只用低额号。
func TestHighCreditsExcludedWhenLowExists(t *testing.T) {
	withNoPickGap(t)
	p := New("")
	p.Add(&auth.Auth{UID: "high"})
	p.Add(&auth.Auth{UID: "low"})
	p.SetCredits("high", 100, 100) // 满额 → 排除
	p.SetCredits("low", 30, 100)   // 已消耗 → 保留
	p.SetRandomSource(func(n int64) int64 { return 0 })

	for i := 0; i < 20; i++ {
		if got := p.Pick(); got == nil || got.UID != "low" {
			t.Fatalf("第 %d 次应选到低额号, got %+v", i+1, got)
		}
	}
	if uids := p.AvailableUIDs(); len(uids) != 1 || uids[0] != "low" {
		t.Fatalf("AvailableUIDs=%v want [low]（会话分配必须与选号同口径）", uids)
	}
	if uids := p.AvailableUIDsForModel("glm-5.2"); len(uids) != 1 || uids[0] != "low" {
		t.Fatalf("AvailableUIDsForModel=%v want [low]", uids)
	}

	p.SetExcludeHighCredits(false, 0.95)
	if got := p.Pick(); got == nil || got.UID != "high" {
		t.Fatalf("关闭策略后高额号（credits 更高）应重新可选, got %+v", got)
	}
	if uids := p.AvailableUIDs(); len(uids) != 2 {
		t.Fatalf("关闭策略后 AvailableUIDs=%v want 2 个", uids)
	}
}

// Status 透出 HighCredits 标记（面板据此解释"为什么没在用它"）。
func TestStatusExposesHighCredits(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.SetCredits("u1", 100, 100)
	st, ok := p.Status("u1")
	if !ok || !st.HighCredits {
		t.Fatalf("满额号应带 HighCredits 标记: ok=%v st=%+v", ok, st)
	}
	p.SetCredits("u1", 10, 100)
	if st, _ := p.Status("u1"); st.HighCredits {
		t.Error("已消耗号不应带 HighCredits 标记")
	}
	p.SetCredits("u2", 50, 0) // 不存在的账号：SetCredits 应静默忽略
	if _, ok := p.Status("u2"); ok {
		t.Error("未加入池的账号不应有状态")
	}
}
