// Package livecfg 运行期可变配置的并发安全持有者。
//
// 背景：进程启动时读入的配置是普通字段（读多写零），但管理面板允许在线改配置，
// 于是少量"可热生效"的字段需要有并发安全的读写点。此处用不可变快照 + atomic 指针：
// 读方 Load 拿到一致视图，写方 Store 整体替换，无锁无数据竞争。
//
// 只承载**读路径深、热改需求强**的少数字段；池参数/排程参数等各有既有 setter
// （pool.SetBreaker、scheduler.Reconfigure 等），不重复收编到这里。
package livecfg

import (
	"sync/atomic"
	"time"
)

// Snapshot 一次读取的不可变配置视图。
type Snapshot struct {
	// APIKey 下游 /v1/* 密钥；空 = /v1 不鉴权。可分发给其它项目/客户端。
	APIKey string
	// PanelKey 管理面密钥（/panel/* 与 /status）；空 = 回落 APIKey（旧配置兼容）。
	PanelKey             string
	SoftCooldown         time.Duration // 429 软冷却基数（<=0 时调用方回退内置默认）
	SanitizeFingerprints bool          // 出站请求体指纹脱敏
}

// AdminKey 返回管理面当前应校验的密钥：PanelKey 非空优先，否则回落 APIKey。
//
// 回落是刻意的兼容口：只配了 api_key 的老部署升级后行为完全不变（面板仍能用原
// 密钥登录）；要让两把密钥真正分离，填上 panel_key 即可——判断依据只有
// 「panel_key 是否为空」这一个开关，不引入第二处配置。
func AdminKey(s Snapshot) string {
	if s.PanelKey != "" {
		return s.PanelKey
	}
	return s.APIKey
}

// Holder 原子持有当前快照。
type Holder struct {
	p atomic.Pointer[Snapshot]
}

// New 以初始快照构建。
func New(s Snapshot) *Holder {
	h := &Holder{}
	h.Store(s)
	return h
}

// Load 返回当前快照（Holder 为 nil 或从未 Store 时返回零值快照，调用方无需判空）。
func (h *Holder) Load() Snapshot {
	if h == nil {
		return Snapshot{}
	}
	if s := h.p.Load(); s != nil {
		return *s
	}
	return Snapshot{}
}

// Store 整体替换快照。
func (h *Holder) Store(s Snapshot) { h.p.Store(&s) }
