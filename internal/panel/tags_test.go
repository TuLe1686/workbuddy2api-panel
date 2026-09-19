package panel

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
)

func newTaggedPanel(t *testing.T) (*Panel, string) {
	t.Helper()
	dir := t.TempDir()
	tagFile := filepath.Join(dir, "tags.json")
	authDir := filepath.Join(dir, "auths")
	if err := os.MkdirAll(authDir, 0o755); err != nil { // SaveAtomic 需要目录已存在
		t.Fatal(err)
	}
	p := New(Config{
		Version: "test", APIKey: "test-key",
		AuthDir: authDir,
		TagFile: tagFile,
		Pool:    pool.New(""),
	})
	return p, tagFile
}

// callTagAPI 走真实路由 + 鉴权（验证挂载点确实在管理面之后）。
func callTagAPI(t *testing.T, p *Panel, method, path, body string, withKey bool) *httptest.ResponseRecorder {
	t.Helper()
	var reader *strings.Reader
	if body == "" {
		reader = strings.NewReader("")
	} else {
		reader = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, reader)
	if withKey {
		req.Header.Set("Authorization", "Bearer test-key")
	}
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	return rec
}

// 标签归一化：去空白/去重/去空项/超长截断，中英文逗号都当分隔符。
func TestNormalizeAndParseTags(t *testing.T) {
	got := parseTagList("主力, 备用,主力，测试  主力")
	want := []string{"主力", "备用", "测试"}
	if len(got) != len(want) {
		t.Fatalf("parseTagList=%v want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("parseTagList=%v want %v", got, want)
		}
	}
	long := strings.Repeat("很", tagMaxLen+10)
	over := normalizeTags([]string{long, long})
	if len([]rune(over[0])) != tagMaxLen {
		t.Fatalf("超长标签应截断到 %d 字符，实际 %d", tagMaxLen, len([]rune(over[0])))
	}
	if len(over) != 1 {
		t.Fatalf("截断后重复项应去重: %v", over)
	}
}

// 打标签三种模式 + 落盘 + 重新载入（标签必须跨重启存活）。
func TestTagStoreAssignAndPersist(t *testing.T) {
	p, tagFile := newTaggedPanel(t)
	s := p.tags

	if changed, err := s.assign([]string{"u1", "u2"}, []string{"主力"}, "add"); err != nil || changed != 2 {
		t.Fatalf("add: changed=%d err=%v", changed, err)
	}
	if changed, _ := s.assign([]string{"u1"}, []string{"主力"}, "add"); changed != 0 {
		t.Errorf("重复 add 同一标签应无变更，changed=%d", changed)
	}
	if changed, _ := s.assign([]string{"u1"}, []string{"测试"}, "add"); changed != 1 {
		t.Errorf("追加标签应变更 1 个账号")
	}
	if got := s.Snapshot()["u1"]; len(got) != 2 {
		t.Errorf("u1 标签=%v want 2 个", got)
	}
	if changed, _ := s.assign([]string{"u1"}, []string{"测试"}, "remove"); changed != 1 {
		t.Errorf("remove 应变更 1 个")
	}
	if got := s.Snapshot()["u1"]; len(got) != 1 || got[0] != "主力" {
		t.Errorf("remove 后 u1 标签=%v", got)
	}
	if changed, _ := s.assign([]string{"u2"}, []string{"甲", "乙"}, "replace"); changed != 1 {
		t.Errorf("replace 应变更 1 个")
	}
	if got := s.Snapshot()["u2"]; len(got) != 2 {
		t.Errorf("replace 后 u2 标签=%v want 2 个", got)
	}
	if changed, _ := s.assign([]string{"u2"}, nil, "remove"); changed != 1 {
		t.Errorf("空标签 remove 应清空标签")
	}
	if _, ok := s.Snapshot()["u2"]; ok {
		t.Error("清空后应删除该 uid 条目")
	}
	if _, err := s.assign([]string{"u1"}, []string{"x"}, "bogus"); err == nil {
		t.Error("非法 mode 应报错")
	}

	reloaded := newTagStore(tagFile)
	if got := reloaded.Snapshot()["u1"]; len(got) != 1 || got[0] != "主力" {
		t.Fatalf("重新载入后 u1 标签=%v want [主力]", got)
	}
	if fi, err := os.Stat(tagFile); err != nil {
		t.Fatalf("标签文件未落盘: %v", err)
	} else if perm := fi.Mode().Perm(); runtimeIsLinux() && perm != 0o600 {
		t.Errorf("标签文件权限=%o want 600", perm)
	}
}

// 多标签并集选择 + forget/prune 清理。
func TestTagStoreSelectionAndCleanup(t *testing.T) {
	p, _ := newTaggedPanel(t)
	s := p.tags
	_, _ = s.assign([]string{"u1"}, []string{"主力"}, "add")
	_, _ = s.assign([]string{"u2"}, []string{"备用"}, "add")
	_, _ = s.assign([]string{"u3"}, []string{"备用", "测试"}, "add")

	if got := s.UIDsWithAnyTag([]string{"主力", "备用"}); len(got) != 3 {
		t.Fatalf("并集选择=%v want 3 个", got)
	}
	if only := s.UIDsWithAnyTag([]string{"测试"}); len(only) != 1 || only[0] != "u3" {
		t.Fatalf("单标签选择=%v want [u3]", only)
	}
	if none := s.UIDsWithAnyTag(nil); none != nil {
		t.Errorf("空标签应返回 nil，得到 %v", none)
	}
	cnts := s.AllTags()
	if len(cnts) != 3 {
		t.Fatalf("AllTags=%+v want 3 个标签", cnts)
	}
	for _, c := range cnts {
		if c.Tag == "备用" && c.Count != 2 {
			t.Errorf("备用计数=%d want 2", c.Count)
		}
	}

	s.forget([]string{"u3"})
	if _, ok := s.Snapshot()["u3"]; ok {
		t.Error("forget 后 u3 不应有标签")
	}
	s.prune(map[string]bool{"u1": true})
	snap := s.Snapshot()
	if _, ok := snap["u2"]; ok {
		t.Error("prune 应清掉不在池内的 u2")
	}
	if _, ok := snap["u1"]; !ok {
		t.Error("prune 不应动池内存活账号")
	}
}

// 损坏的标签文件不应让面板起不来（改名留证 + 空表继续）。
func TestTagStoreCorruptFileRecovers(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "tags.json")
	if err := os.WriteFile(f, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	s := newTagStore(f)
	if len(s.Snapshot()) != 0 {
		t.Fatalf("损坏文件应回退空表: %v", s.Snapshot())
	}
	if _, err := os.Stat(f + ".corrupt"); err != nil {
		t.Errorf("损坏文件应改名留证: %v", err)
	}
}

// 导入时打标签：请求级 tags 应用到这一批；条目自带 tags 优先。
func TestImportAppliesTags(t *testing.T) {
	p, _ := newTaggedPanel(t)

	rec := postImport(t, p, `{"accounts":[{"uid":"imp-1","access_token":"AT1"},{"uid":"imp-2","access_token":"AT2"}],"tags":["主力","新导入"]}`)
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	snap := p.tags.Snapshot()
	for _, uid := range []string{"imp-1", "imp-2"} {
		if len(snap[uid]) != 2 {
			t.Errorf("%s 标签=%v want 2 个（请求级 tags）", uid, snap[uid])
		}
	}

	rec = postImport(t, p, `{"accounts":[{"uid":"imp-3","access_token":"AT3","tags":["条目级"]}],"tags":["请求级"]}`)
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	if got := p.tags.Snapshot()["imp-3"]; len(got) != 1 || got[0] != "条目级" {
		t.Errorf("imp-3 标签=%v want [条目级]", got)
	}
}

// 导出带标签，且导出文件回导后标签仍在（导出→导入闭环不丢标签）。
func TestExportIncludesTagsAndRoundTrips(t *testing.T) {
	p, _ := newTaggedPanel(t)
	seedAuth(t, p, p.cfg.AuthDir, "uid-a", "A", "cn", "www.codebuddy.cn", "AT-a")
	if _, err := p.tags.assign([]string{"uid-a"}, []string{"主力", "备用"}, "add"); err != nil {
		t.Fatal(err)
	}

	rec := doExport(t, p, `{}`)
	var items []json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &items); err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 {
		t.Fatalf("导出条数=%d want 1", len(items))
	}
	var item struct {
		Tags []string `json:"tags"`
	}
	if err := json.Unmarshal(items[0], &item); err != nil {
		t.Fatal(err)
	}
	if len(item.Tags) != 2 {
		t.Fatalf("导出未带标签: %+v", item.Tags)
	}

	p2, _ := newTaggedPanel(t)
	if rec := postImport(t, p2, string(rec.Body.Bytes())); rec.Code != 200 {
		t.Fatalf("回导失败 code=%d body=%s", rec.Code, rec.Body.String())
	}
	if got := p2.tags.Snapshot()["uid-a"]; len(got) != 2 {
		t.Errorf("回导后标签=%v want 2 个", got)
	}
}

// 标签接口：全量读 + 批量赋值；空 uids 拒绝；不在池内的 uid 记 skipped；未鉴权 401。
func TestTagsAPI(t *testing.T) {
	p, _ := newTaggedPanel(t)
	seedAuth(t, p, p.cfg.AuthDir, "uid-a", "A", "cn", "www.codebuddy.cn", "AT-a")

	rec := callTagAPI(t, p, "POST", "/panel/api/tags/assign", `{"uids":["uid-a"],"tags":["主力"],"mode":"add"}`, true)
	if rec.Code != 200 {
		t.Fatalf("assign code=%d body=%s", rec.Code, rec.Body.String())
	}
	var out struct {
		Changed int                 `json:"changed"`
		Skipped int                 `json:"skipped"`
		ByUID   map[string][]string `json:"by_uid"`
		All     []tagCount          `json:"all"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Changed != 1 || len(out.ByUID["uid-a"]) != 1 || len(out.All) != 1 {
		t.Fatalf("assign 返回异常: %+v", out)
	}

	if rec := callTagAPI(t, p, "POST", "/panel/api/tags/assign", `{"uids":[],"tags":["x"]}`, true); rec.Code != 400 {
		t.Errorf("空 uids code=%d want 400", rec.Code)
	}
	if rec := callTagAPI(t, p, "POST", "/panel/api/tags/assign", `{"uids":["ghost"],"tags":["x"]}`, true); rec.Code != 404 {
		t.Errorf("幽灵 uid code=%d want 404", rec.Code)
	}
	if rec := callTagAPI(t, p, "POST", "/panel/api/tags/assign", `{"uids":["uid-a"],"tags":["x"],"mode":"bogus"}`, true); rec.Code != 400 {
		t.Errorf("非法 mode code=%d want 400", rec.Code)
	}

	rec = callTagAPI(t, p, "GET", "/panel/api/tags", "", true)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "主力") {
		t.Errorf("tagsList code=%d body=%s", rec.Code, rec.Body.String())
	}
	if rec := callTagAPI(t, p, "GET", "/panel/api/tags", "", false); rec.Code != http.StatusUnauthorized {
		t.Errorf("未鉴权 code=%d want 401", rec.Code)
	}
}

// 全部移除后标签一起清空（账号没了标签无意义）。
func TestRemoveAllClearsTags(t *testing.T) {
	p, _ := newTaggedPanel(t)
	seedAuth(t, p, p.cfg.AuthDir, "uid-a", "A", "cn", "www.codebuddy.cn", "AT-a")
	_, _ = p.tags.assign([]string{"uid-a"}, []string{"主力"}, "add")

	req := httptest.NewRequest("POST", "/panel/api/accounts/remove_all", strings.NewReader(`{"confirm":true}`))
	req.Header.Set("Authorization", "Bearer test-key")
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("remove_all code=%d body=%s", rec.Code, rec.Body.String())
	}
	if len(p.tags.Snapshot()) != 0 {
		t.Errorf("移除全部后标签未清空: %v", p.tags.Snapshot())
	}
}

// runtimeIsLinux 让权限断言只在 Linux/CI 生效（Windows 不保留 POSIX 位）。
func runtimeIsLinux() bool {
	return os.PathSeparator == '/'
}
