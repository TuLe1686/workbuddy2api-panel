package pool

import (
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// 满额度账号权重压到最低档：默认开启时低于同等总额度下已消耗的账号；
// 关掉开关后回到「credits 越多权重越高」的旧行为（满额号重新占优）。
func TestFullCreditsLowestPriorityWeights(t *testing.T) {
	p := New("")
	now := time.Now()
	full := &entry{credits: 100, creditsTotal: 100}
	partial := &entry{credits: 30, creditsTotal: 100}

	wFullOn := p.weightOf(full, 100, now)
	wPartial := p.weightOf(partial, 100, now)
	if wFullOn >= wPartial {
		t.Fatalf("开启时满额号权重应低于已消耗号: full=%v partial=%v", wFullOn, wPartial)
	}

	p.SetFullCreditsLast(false)
	wFullOff := p.weightOf(full, 100, now)
	if wFullOff <= wPartial {
		t.Fatalf("关闭后满额号应按 credits 占优: full=%v partial=%v", wFullOff, wPartial)
	}
}

// credits_total 未知（0）不参与满额判定：同样 credits 下，它应与"有总额但未满"
// 的账号同权，且明显高于被压权重的满额号（旧状态/查询失败不能被误压）。
func TestFullCreditsUnknownTotalNotPenalized(t *testing.T) {
	p := New("")
	now := time.Now()
	unknown := &entry{credits: 50}                    // 总额未知
	partial := &entry{credits: 50, creditsTotal: 100} // 有总额、未满
	full := &entry{credits: 50, creditsTotal: 50}     // 满额
	wUnknown := p.weightOf(unknown, 50, now)
	wPartial := p.weightOf(partial, 50, now)
	wFull := p.weightOf(full, 50, now)
	if wUnknown != wPartial {
		t.Fatalf("总额未知应与未满账号同权: unknown=%v partial=%v", wUnknown, wPartial)
	}
	if wUnknown <= wFull {
		t.Fatalf("总额未知不应被当成满额压权重: unknown=%v full=%v", wUnknown, wFull)
	}
}

// 全员满额时同比缩放：相对次序不变（等同于关闭开关的行为）。
func TestFullCreditsAllFullKeepsRelativeOrder(t *testing.T) {
	p := New("")
	now := time.Now()
	high := &entry{credits: 100, creditsTotal: 100}
	low := &entry{credits: 50, creditsTotal: 50}
	wHigh := p.weightOf(high, 100, now)
	wLow := p.weightOf(low, 100, now)
	if wHigh <= wLow {
		t.Fatalf("全员满额时 credits 高者权重仍应更高: high=%v low=%v", wHigh, wLow)
	}
}

// 端到端：池内同时有满额号与已消耗号时，Pick 稳定选中已消耗的那个。
func TestPickPrefersNonFullAccount(t *testing.T) {
	withNoPickGap(t)
	p := New("")
	p.Add(&auth.Auth{UID: "full"})
	p.Add(&auth.Auth{UID: "partial"})
	p.SetCredits("full", 100, 100)   // 满额：未动用
	p.SetCredits("partial", 30, 100) // 已消耗 70%
	p.SetRandomSource(func(n int64) int64 { return 0 })

	for i := 0; i < 20; i++ {
		got := p.Pick()
		if got == nil {
			t.Fatal("Pick 返回 nil")
		}
		if got.UID == "full" {
			t.Fatalf("第 %d 次选中了满额账号（应让位给已消耗的号）", i+1)
		}
	}

	// 关掉开关后，满额号（credits 更高）应重新成为首选
	p.SetFullCreditsLast(false)
	if got := p.Pick(); got == nil || got.UID != "full" {
		t.Fatalf("关闭开关后应选中 credits 更高的满额号, got %+v", got)
	}
}
