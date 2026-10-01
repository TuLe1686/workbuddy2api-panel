package apikeys

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestVerifyRoundtrip(t *testing.T) {
	s := New("")
	k, err := s.Create("测试A", 100, 0)
	if err != nil {
		t.Fatal(err)
	}
	if id, ok := s.Verify(k.Secret); !ok || id != k.ID {
		t.Errorf("Verify 命中失败: id=%q ok=%v", id, ok)
	}
	if _, ok := s.Verify("sk-wrong"); ok {
		t.Error("错误 token 不应命中")
	}
	if _, ok := s.Verify(""); ok {
		t.Error("空 token 不应命中")
	}
}

func TestVerifyDisabledRejected(t *testing.T) {
	s := New("")
	k, _ := s.Create("禁用测试", 0, 0)
	dis := true
	if _, _, err := s.Update(k.ID, nil, nil, nil, &dis); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.Verify(k.Secret); ok {
		t.Error("禁用的 key 不应通过校验")
	}
	// 重新启用后恢复
	dis = false
	s.Update(k.ID, nil, nil, nil, &dis)
	if _, ok := s.Verify(k.Secret); !ok {
		t.Error("重新启用后应通过校验")
	}
}

func TestCheckQuotaCredits(t *testing.T) {
	s := New("")
	k, _ := s.Create("积分限额", 10, 0)
	if r := s.CheckQuota(k.ID); r != "" {
		t.Errorf("未消耗不应触发限额: %q", r)
	}
	s.NoteUsage(k.ID, 100, true, 10.5, true)
	if r := s.CheckQuota(k.ID); r == "" {
		t.Error("消耗 10.5 >= 上限 10 应触发限额")
	}
	// 静态 api_key（keyID=""）恒不限额
	if r := s.CheckQuota(""); r != "" {
		t.Errorf("空 keyID 不应触发限额: %q", r)
	}
}

func TestCheckQuotaTokens(t *testing.T) {
	s := New("")
	k, _ := s.Create("token 限额", 0, 1000)
	s.NoteUsage(k.ID, 1000, true, 0, false)
	if r := s.CheckQuota(k.ID); r == "" {
		t.Error("token 达上限应触发限额")
	}
	// 无 usage 的失败尝试只计请求数，不推进 token 台账
	k2, _ := s.Create("只失败", 0, 100)
	s.NoteRequest(k2.ID)
	if r := s.CheckQuota(k2.ID); r != "" {
		t.Errorf("无 usage 尝试不应触发 token 限额: %q", r)
	}
}

func TestNoteUsageAccumulates(t *testing.T) {
	s := New("")
	k, _ := s.Create("记账", 0, 0)
	s.NoteUsage(k.ID, 100, true, 1.5, true)
	s.NoteUsage(k.ID, 200, true, 2.25, true)
	s.NoteRequest(k.ID) // 失败尝试：只加请求
	s.NoteUsage("kid-nope", 1, true, 1, true)
	s.NoteUsage("", 1, true, 1, true)
	info, ok := s.Quota(k.ID)
	if !ok {
		t.Fatal("Quota 应命中")
	}
	if info.Credits != 3.75 {
		t.Errorf("credits = %v, want 3.75", info.Credits)
	}
	if info.Tokens != 300 {
		t.Errorf("tokens = %v, want 300", info.Tokens)
	}
	if info.ID != k.ID || info.Name != "记账" {
		t.Errorf("快照字段不符: %+v", info)
	}
}

func TestResetStatsAndRemove(t *testing.T) {
	s := New("")
	k, _ := s.Create("重置", 5, 0)
	s.NoteUsage(k.ID, 10, true, 5, true)
	if r := s.CheckQuota(k.ID); r == "" {
		t.Fatal("应先触发限额")
	}
	if !s.ResetStats(k.ID) {
		t.Fatal("ResetStats 应成功")
	}
	if r := s.CheckQuota(k.ID); r != "" {
		t.Errorf("清零后不应触发限额: %q", r)
	}
	got, ok := s.List()[0], true
	if !ok {
		t.Fatal("List 应返回条目")
	}
	if got.Requests != 0 || got.Credits != 0 || got.Tokens != 0 {
		t.Errorf("清零后台账应为零: %+v", got)
	}
	if !s.Remove(k.ID) {
		t.Fatal("Remove 应成功")
	}
	if _, ok := s.Verify(k.Secret); ok {
		t.Error("删除后 key 不应再通过校验")
	}
	if s.Remove(k.ID) {
		t.Error("重复 Remove 应返回 false")
	}
}

func TestPersistenceRoundtrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "api_keys.json")
	s := New(path)
	k1, err := s.Create("持久A", 100, 5000)
	if err != nil {
		t.Fatal(err)
	}
	s.NoteUsage(k1.ID, 123, true, 4.5, true)
	k2, _ := s.Create("持久B", 0, 0)

	// 重新载入：key、台账都应恢复。
	s2 := New(path)
	if _, ok := s2.Verify(k1.Secret); !ok {
		t.Error("重载后 key1 应命中")
	}
	if _, ok := s2.Verify(k2.Secret); !ok {
		t.Error("重载后 key2 应命中")
	}
	info, ok := s2.Quota(k1.ID)
	if !ok || info.Credits != 4.5 || info.Tokens != 123 || info.MaxCredits != 100 || info.MaxTokens != 5000 {
		t.Errorf("重载后台账不符: %+v ok=%v", info, ok)
	}
}

func TestCorruptFileQuarantined(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "api_keys.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	s := New(path)
	if s.Count() != 0 {
		t.Errorf("损坏文件应按空表继续, got %d", s.Count())
	}
	if _, err := os.Stat(path + ".corrupt"); err != nil {
		t.Errorf("损坏文件应改名 .corrupt 留证: %v", err)
	}
	// 空表可继续创建（保存路径应正常写新文件）。
	if _, err := s.Create("灾后新建", 0, 0); err != nil {
		t.Fatalf("灾后创建失败: %v", err)
	}
}

func TestConcurrentNoteUsage(t *testing.T) {
	s := New("")
	k, _ := s.Create("并发", 0, 0)
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.NoteUsage(k.ID, 10, true, 0.5, true)
		}()
	}
	wg.Wait()
	var got Key
	for _, kk := range s.List() {
		if kk.ID == k.ID {
			got = kk
		}
	}
	if got.Requests != 32 || got.Tokens != 320 || got.Credits != 16 {
		t.Errorf("并发记账不一致: %+v", got)
	}
}

func TestUpdateSemantics(t *testing.T) {
	s := New("")
	k, _ := s.Create("原名", 1, 1)
	// 只改额度，不动名字
	mc, mt := 99.0, int64(999)
	got, ok, err := s.Update(k.ID, nil, &mc, &mt, nil)
	if err != nil || !ok {
		t.Fatalf("Update 失败: %v ok=%v", err, ok)
	}
	if got.Name != "原名" || got.MaxCredits != 99 || got.MaxTokens != 999 {
		t.Errorf("更新结果不符: %+v", got)
	}
	// 名字传空白 = 不改（保持原值）
	blank := "  "
	if _, _, err := s.Update(k.ID, &blank, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.Quota(k.ID); got.Name != "原名" {
		t.Errorf("空白名不应覆盖: %q", got.Name)
	}
	// 不存在的 id
	if _, ok, _ := s.Update("kid-nope", nil, nil, nil, nil); ok {
		t.Error("不存在的 id 不应更新成功")
	}
}
