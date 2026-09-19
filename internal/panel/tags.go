// tags.go 面板标签：账号维度的运营标记（主力/备用/测试…），存 data/tags.json。
//
// 为什么单独存一个文件，而不是塞进 auth 文件或池状态：
//   - auth 文件由 auth.SaveAtomic 以**固定字段集**重写（token 刷新时），任何额外键
//     都会在刷新后丢失——标签放进去等于随时可能被清空；
//   - 池状态（state.json）是**运行态**（冷却/熔断/用量），与"运维给账号贴的标签"
//     是两类数据，混在一起会让状态清理/重建连带丢标签。
//
// 设计取舍：标签是面板侧元数据，网关路由**完全不读**它（不影响选号/转发），
// 因此这里只有简单的"读-改-写 + 原子替换"，失败只影响面板展示。
package panel

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

// tagMaxLen 单个标签长度上限（防把整段文本当标签塞进来）。
const tagMaxLen = 32

// tagFileVersion tags.json 结构版本（未来若改结构，据此迁移）。
const tagFileVersion = 1

// tagStore 账号标签仓库（并发安全，落盘原子替换）。
type tagStore struct {
	mu    sync.Mutex
	path  string
	byUID map[string][]string
}

// newTagStore 从 path 载入标签（文件缺失/损坏都当作空表：标签不是关键路径数据，
// 不值得为它让面板起不来；损坏时把坏文件改名留证）。
func newTagStore(path string) *tagStore {
	s := &tagStore{path: path, byUID: map[string][]string{}}
	if path == "" {
		return s
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		if !os.IsNotExist(err) {
			log.Printf("WARN: 读取标签文件 %s 失败: %v（按空表继续）", path, err)
		}
		return s
	}
	var doc struct {
		Version int                 `json:"version"`
		ByUID   map[string][]string `json:"by_uid"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		bad := path + ".corrupt"
		_ = os.Rename(path, bad)
		log.Printf("WARN: 标签文件解析失败（已改名 %s 留证）: %v", bad, err)
		return s
	}
	for uid, tags := range doc.ByUID {
		if clean := normalizeTags(tags); len(clean) > 0 {
			s.byUID[uid] = clean
		}
	}
	log.Printf("标签已载入：%d 个账号带标签（%s）", len(s.byUID), path)
	return s
}

// normalizeTags 归一化标签列表：去空白、去重、去空项、截断超长、保序。
func normalizeTags(tags []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(tags))
	for _, t := range tags {
		t = strings.TrimSpace(t)
		if t == "" {
			continue
		}
		if len([]rune(t)) > tagMaxLen {
			t = string([]rune(t)[:tagMaxLen])
		}
		if seen[t] {
			continue
		}
		seen[t] = true
		out = append(out, t)
	}
	return out
}

// parseTagList 解析逗号/空格分隔的标签输入（中英文逗号都收）。
func parseTagList(s string) []string {
	fields := strings.FieldsFunc(s, func(r rune) bool {
		return r == ',' || r == '，' || r == ' ' || r == '\t' || r == '\n' || r == ';' || r == '；'
	})
	return normalizeTags(fields)
}

// Snapshot 返回 by_uid 副本（调用方只读，不共享内部切片）。
func (s *tagStore) Snapshot() map[string][]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string][]string, len(s.byUID))
	for uid, tags := range s.byUID {
		out[uid] = append([]string(nil), tags...)
	}
	return out
}

// AllTags 返回全部标签及各自账号数（按标签名排序，供面板渲染筛选条）。
type tagCount struct {
	Tag   string `json:"tag"`
	Count int    `json:"count"`
}

func (s *tagStore) AllTags() []tagCount {
	s.mu.Lock()
	defer s.mu.Unlock()
	counts := map[string]int{}
	for _, tags := range s.byUID {
		for _, t := range tags {
			counts[t]++
		}
	}
	out := make([]tagCount, 0, len(counts))
	for t, c := range counts {
		out = append(out, tagCount{Tag: t, Count: c})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Tag < out[j].Tag })
	return out
}

// UIDsWithAnyTag 返回命中所给任一标签的账号（并集，按 uid 排序）。
func (s *tagStore) UIDsWithAnyTag(tags []string) []string {
	want := map[string]bool{}
	for _, t := range normalizeTags(tags) {
		want[t] = true
	}
	if len(want) == 0 {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for uid, own := range s.byUID {
		for _, t := range own {
			if want[t] {
				out = append(out, uid)
				break
			}
		}
	}
	sort.Strings(out)
	return out
}

// assign 批量打标签/去标签：
//   - mode "add"：并入（幂等）
//   - mode "remove"：移除指定标签（空标签列表 = 清空该账号全部标签）
//   - mode "replace"：整体替换为该列表
func (s *tagStore) assign(uids, tags []string, mode string) (int, error) {
	if mode != "add" && mode != "remove" && mode != "replace" {
		return 0, errors.New("mode 必须是 add / remove / replace")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	changed := 0
	for _, uid := range uids {
		uid = strings.TrimSpace(uid)
		if uid == "" {
			continue
		}
		cur := s.byUID[uid]
		var next []string
		switch mode {
		case "add":
			next = normalizeTags(append(append([]string(nil), cur...), tags...))
		case "remove":
			next = removeTags(cur, tags)
		case "replace":
			next = normalizeTags(tags)
		}
		if equalTags(cur, next) {
			continue
		}
		changed++
		if len(next) == 0 {
			delete(s.byUID, uid)
		} else {
			s.byUID[uid] = next
		}
	}
	if changed == 0 {
		return 0, nil
	}
	return changed, s.saveLocked()
}

// removeTags 从 own 中剔除 drop（drop 为空则清空全部）。
func removeTags(own, drop []string) []string {
	if len(drop) == 0 {
		return nil
	}
	del := map[string]bool{}
	for _, t := range normalizeTags(drop) {
		del[t] = true
	}
	out := make([]string, 0, len(own))
	for _, t := range own {
		if !del[t] {
			out = append(out, t)
		}
	}
	return out
}

func equalTags(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// forget 删除账号时清掉其标签（账号都没了，标签没有意义）。
func (s *tagStore) forget(uids []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	changed := false
	for _, uid := range uids {
		if _, ok := s.byUID[uid]; ok {
			delete(s.byUID, uid)
			changed = true
		}
	}
	if changed {
		_ = s.saveLocked()
	}
}

// prune 丢弃池内已不存在的 uid 的标签（防池重建后留下幽灵标签）。
func (s *tagStore) prune(alive map[string]bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	changed := false
	for uid := range s.byUID {
		if !alive[uid] {
			delete(s.byUID, uid)
			changed = true
		}
	}
	if changed {
		_ = s.saveLocked()
	}
}

// saveLocked 落盘（持锁调用）：tmp + rename 原子替换；失败只记日志不阻断。
//
// 注意 rename 语义：tags.json 在**目录挂载**（shared/data）里，不是单文件挂载，
// 因此 tmp+rename 可用（对比 config.json 的单文件挂载必须就地重写）。
func (s *tagStore) saveLocked() error {
	if s.path == "" {
		return nil
	}
	doc := map[string]any{"version": tagFileVersion, "by_uid": s.byUID}
	raw, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	if dir := filepath.Dir(s.path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("mkdir tags dir: %w", err)
		}
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}
