// Package apikeys 下游 API Key 管理：多 key 签发 / 校验 / 限额（积分或 token 上限）
// / 消耗归因，存 data/api_keys.json。
//
// 与既有静态 api_key 的关系：
//   - 静态 api_key 继续有效（handler 侧优先校验，命中即 keyID=""）：它是不设限额
//     的管理员信任通道，旧客户端零迁移；
//   - 本包的 key 面向"发给他方项目/SDK"的分发场景：每个 key 独立备注名、独立
//     积分/token 上限、独立消耗台账（requests / credits / tokens 累计，重启不丢）。
//
// 限额语义：事前检查（CheckQuota）+ 事后记账（NoteUsage）。在途请求不会被中断，
// 并发下可能轻微超出上限后即拒绝新请求——与 kiro 客户端 key 的 maxCredits 同口径。
//
// 存储形态：明文（与 config.json 里 api_key 明文同级安全边界，面板可再查看/复制
// 分发）。落盘走 tmp+rename 原子替换（目录挂载，同 tags.json）。
//
// 设计对标 internal/panel 的 tagStore：RWMutex + 原子落盘 + 损坏文件改名留证后
// 空表启动——key 表不是关键路径数据，不值得为它让网关起不来。
package apikeys

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// keyFileVersion api_keys.json 结构版本（未来若改结构，据此迁移）。
const keyFileVersion = 1

// Key 一个下游 API Key 及其消耗台账。
type Key struct {
	ID        string    `json:"id"`         // kid-<8hex>，创建后不变
	Name      string    `json:"name"`       // 备注名（发给谁 / 用在哪）
	Secret    string    `json:"key"`        // "sk-" 前缀明文密钥（与 config api_key 同级安全边界）
	MaxCredits float64  `json:"max_credits"` // 积分上限（上游 usage.credit 累计）；<=0 = 不限
	MaxTokens  int64    `json:"max_tokens"`  // token 上限（total tokens 累计）；<=0 = 不限
	Disabled   bool     `json:"disabled"`    // true = 校验直接拒绝（401），保留台账

	CreatedAt time.Time `json:"created_at"`

	// 消耗台账（持久化；面板「清零统计」整体归零）。
	Requests   int64     `json:"requests"`    // 尝试次数（含失败，与 usage 口径一致）
	Credits    float64   `json:"credits"`     // 累计消耗积分（成功且有 usage.credit）
	Tokens     int64     `json:"tokens"`      // 累计 total tokens（成功且有 usage）
	LastUsedAt time.Time `json:"last_used_at"`
}

// keyFile 落盘结构。
type keyFile struct {
	Version int    `json:"version"`
	Saved   string `json:"saved"`
	Keys    []Key  `json:"keys"`
}

// Store API Key 仓库（并发安全，落盘原子替换）。
type Store struct {
	mu   sync.RWMutex
	path string
	keys map[string]*Key // id → key
}

// New 从 path 载入 key 表（文件缺失/损坏都当作空表：key 表不是关键路径数据）。
// path 为空 = 纯内存（测试用）。
func New(path string) *Store {
	s := &Store{path: path, keys: map[string]*Key{}}
	if path == "" {
		return s
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		if !os.IsNotExist(err) {
			log.Printf("WARN: [apikeys] 读取 key 文件 %s 失败: %v（按空表继续）", path, err)
		}
		return s
	}
	var f keyFile
	if err := json.Unmarshal(raw, &f); err != nil {
		bad := path + ".corrupt"
		_ = os.Rename(path, bad)
		log.Printf("WARN: [apikeys] key 文件解析失败（已改名 %s 留证）: %v", bad, err)
		return s
	}
	for i := range f.Keys {
		k := f.Keys[i]
		if k.ID == "" || k.Secret == "" {
			continue // 脏条目不进内存
		}
		s.keys[k.ID] = &k
	}
	log.Printf("[apikeys] 已载入 %d 个 API Key（%s）", len(s.keys), path)
	return s
}

// ---------------------------------------------------------------- 校验与限额 ----

// Verify 校验一个 Bearer token 是否命中某个 key。
// 命中返回 (keyID, true)；禁用的 key 视为未命中（401，不泄漏"存在但被禁"）。
// 用 SHA-256 摘要做 map 键：长度差异被摘要吸收，map 查找比较的是定长摘要。
func (s *Store) Verify(token string) (string, bool) {
	if token == "" {
		return "", false
	}
	sum := sha256.Sum256([]byte(token))
	digest := hex.EncodeToString(sum[:])
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, k := range s.keys {
		if k.digest() == digest && !k.Disabled {
			return k.ID, true
		}
	}
	return "", false
}

// digest 返回 Secret 的 SHA-256 hex（每次校验都算一次；key 数量为个位数到几十，
// 线性扫描的常数量可忽略，且省去"增删 key 时维护摘要索引"的一致性负担）。
func (k *Key) digest() string {
	sum := sha256.Sum256([]byte(k.Secret))
	return hex.EncodeToString(sum[:])
}

// QuotaInfo 一把 key 的限额快照（供面板与 429 响应组装文案）。
type QuotaInfo struct {
	ID         string
	Name       string
	MaxCredits float64
	MaxTokens  int64
	Credits    float64
	Tokens     int64
}

// Quota 返回 keyID 的限额快照；不存在返回 ok=false（静态 api_key 走 keyID=""，
// 不在本表内，调用方按"不限额"处理）。
func (s *Store) Quota(keyID string) (QuotaInfo, bool) {
	if keyID == "" {
		return QuotaInfo{}, false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	k, ok := s.keys[keyID]
	if !ok {
		return QuotaInfo{}, false
	}
	return QuotaInfo{
		ID: k.ID, Name: k.Name,
		MaxCredits: k.MaxCredits, MaxTokens: k.MaxTokens,
		Credits: k.Credits, Tokens: k.Tokens,
	}, true
}

// CheckQuota 报告该 key 是否已达限额。空 reason = 放行。
// 判定用 >=（记账是事后的，达到上限后的**新请求**被拒，在途请求照常完成）。
func (s *Store) CheckQuota(keyID string) string {
	if keyID == "" {
		return ""
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	k, ok := s.keys[keyID]
	if !ok {
		return ""
	}
	if k.MaxCredits > 0 && k.Credits >= k.MaxCredits {
		return fmt.Sprintf("credit quota exceeded: %.2f/%.2f credits used", k.Credits, k.MaxCredits)
	}
	if k.MaxTokens > 0 && k.Tokens >= k.MaxTokens {
		return fmt.Sprintf("token quota exceeded: %d/%d tokens used", k.Tokens, k.MaxTokens)
	}
	return ""
}

// ---------------------------------------------------------------- 记账 ----

// NoteUsage 记一次请求的消耗：请求计数恒加；tokens/credit 只在对应 has 为 true
// 时累计（失败尝试没有 usage，与 usage.Recorder 的口径一致）。
// hasTokens=false 且 hasCredit=false 也可以只做请求计数。keyID 为空（静态 api_key
// 或不鉴权模式）时静默跳过。
func (s *Store) NoteUsage(keyID string, tokens int64, hasTokens bool, credit float64, hasCredit bool) {
	if keyID == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	k, ok := s.keys[keyID]
	if !ok {
		return
	}
	k.Requests++
	k.LastUsedAt = time.Now()
	if hasTokens && tokens > 0 {
		k.Tokens += tokens
	}
	if hasCredit && credit > 0 {
		k.Credits += credit
	}
	_ = s.saveLocked()
}

// NoteRequest 只计一次请求尝试（无 usage 的失败路径）。
func (s *Store) NoteRequest(keyID string) {
	if keyID == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	k, ok := s.keys[keyID]
	if !ok {
		return
	}
	k.Requests++
	k.LastUsedAt = time.Now()
	_ = s.saveLocked()
}

// ---------------------------------------------------------------- CRUD ----

// Create 签发一把新 key。name 为空时给 "key-<n>"；限额 <=0 = 不限。
// 返回完整 Key（含明文 Secret，面板只在创建响应里展示一次完整值的习惯不适用——
// 明文存储，列表随时可查）。
func (s *Store) Create(name string, maxCredits float64, maxTokens int64) (Key, error) {
	secret, err := randomSecret()
	if err != nil {
		return Key{}, err
	}
	id, err := randomID()
	if err != nil {
		return Key{}, err
	}
	name = strings.TrimSpace(name)
	if name == "" {
		name = fmt.Sprintf("key-%d", time.Now().Unix())
	}
	k := Key{
		ID:         id,
		Name:       name,
		Secret:     secret,
		MaxCredits: maxCredits,
		MaxTokens:  maxTokens,
		CreatedAt:  time.Now(),
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.keys[k.ID] = &k
	if err := s.saveLocked(); err != nil {
		delete(s.keys, k.ID)
		return Key{}, err
	}
	return k, nil
}

// Update 修改 key 的可变字段（name / 限额 / 启停）。字段指针为 nil = 不改；
// 显式传值才能改（区分"未提交"与"清零"）。返回更新后的快照。
func (s *Store) Update(id string, name *string, maxCredits *float64, maxTokens *int64, disabled *bool) (Key, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	k, ok := s.keys[id]
	if !ok {
		return Key{}, false, nil
	}
	if name != nil {
		if v := strings.TrimSpace(*name); v != "" {
			k.Name = v
		}
	}
	if maxCredits != nil {
		k.MaxCredits = *maxCredits
	}
	if maxTokens != nil {
		k.MaxTokens = *maxTokens
	}
	if disabled != nil {
		k.Disabled = *disabled
	}
	err := s.saveLocked()
	return *k, true, err
}

// Remove 删除一把 key（台账一并删除；网关在途请求不受影响）。
func (s *Store) Remove(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.keys[id]; !ok {
		return false
	}
	delete(s.keys, id)
	_ = s.saveLocked()
	return true
}

// ResetStats 清零一把 key 的消耗台账（限额重新从零起算）。
func (s *Store) ResetStats(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	k, ok := s.keys[id]
	if !ok {
		return false
	}
	k.Requests, k.Credits, k.Tokens = 0, 0, 0
	k.LastUsedAt = time.Time{}
	_ = s.saveLocked()
	return true
}

// List 返回全部 key 的快照（按创建时间升序；Secret 一并返回——明文存储，
// 面板掩码展示但可切换显示）。
func (s *Store) List() []Key {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Key, 0, len(s.keys))
	for _, k := range s.keys {
		out = append(out, *k)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out
}

// Count 返回 key 总数（启动日志与 overview 用）。
func (s *Store) Count() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.keys)
}

// Name 返回 keyID 的备注名（日志行展示用；不存在/静态 key 返回空串）。
func (s *Store) Name(keyID string) string {
	if keyID == "" {
		return ""
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if k, ok := s.keys[keyID]; ok {
		return k.Name
	}
	return ""
}

// ---------------------------------------------------------------- 落盘 ----

// saveLocked 落盘（持锁调用）：tmp + rename 原子替换；失败只记日志不阻断
// （内存表仍是权威值，下次任何一次写操作会再试）。
func (s *Store) saveLocked() error {
	if s.path == "" {
		return nil
	}
	doc := keyFile{Version: keyFileVersion, Saved: time.Now().Format(time.RFC3339), Keys: s.snapshotLocked()}
	raw, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	if dir := filepath.Dir(s.path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("mkdir apikeys dir: %w", err)
		}
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

// snapshotLocked 深拷贝一份 key 列表（落盘与 List 不共享内部指针）。
func (s *Store) snapshotLocked() []Key {
	out := make([]Key, 0, len(s.keys))
	for _, k := range s.keys {
		out = append(out, *k)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out
}

// randomSecret 生成 "sk-" 前缀密钥（crypto/rand，24 字节）。
func randomSecret() (string, error) {
	raw := make([]byte, 24)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("gen key: %w", err)
	}
	return "sk-" + base64.RawURLEncoding.EncodeToString(raw), nil
}

// randomID 生成 "kid-" 前缀的短 ID（4 字节 hex，同进程内碰撞概率可忽略；
// 冲突时 Create 落盘前再试一次的代价小于全局唯一性工程）。
func randomID() (string, error) {
	raw := make([]byte, 4)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("gen id: %w", err)
	}
	return "kid-" + hex.EncodeToString(raw), nil
}
