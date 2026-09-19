package panel

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

func newExportPanel(t *testing.T) (*Panel, string) {
	t.Helper()
	dir := t.TempDir()
	p := New(Config{Version: "test", APIKey: "test-key", AuthDir: dir, Pool: pool.New("")})
	return p, dir
}

// seedAuth 按「面板导入」的同一路径写入凭证文件并进池，供导出测试取源。
func seedAuth(t *testing.T, p *Panel, dir, uid, nickname, realm, domain string, token string) {
	t.Helper()
	a := &auth.Auth{
		UID: uid, Nickname: nickname, AccessToken: token, RefreshToken: "RT-" + uid,
		ExpiresAt: 1794484878, Domain: domain,
		FilePath: filepath.Join(dir, "workbuddy-"+uid+".json"),
	}
	if _, err := auth.BackfillRealmFor(a, realm); err != nil {
		t.Fatal(err)
	}
	if err := a.SaveAtomic(); err != nil {
		t.Fatal(err)
	}
	p.cfg.Pool.Add(a)
}

func doExport(t *testing.T, p *Panel, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("POST", "/panel/api/accounts/export", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer test-key")
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	return rec
}

// 导出格式必须能原样回导：把导出条目逐条喂回导入解析器，核对凭证与域不丢。
func TestAccountsExportRoundTripsThroughImport(t *testing.T) {
	p, dir := newExportPanel(t)
	seedAuth(t, p, dir, "uid-cn", "国内号", "cn", "www.codebuddy.cn", "AT-cn")
	seedAuth(t, p, dir, "uid-global", "国际号", "global", "www.workbuddy.ai", "AT-global")

	rec := doExport(t, p, `{}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control=%q want no-store（导出含明文凭证，不能进缓存）", got)
	}
	if cd := rec.Header().Get("Content-Disposition"); !strings.Contains(cd, "codebuddy_accounts_") {
		t.Errorf("Content-Disposition=%q 应带 codebuddy_accounts_ 文件名（混域）", cd)
	}

	var items []json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &items); err != nil {
		t.Fatal(err)
	}
	if len(items) != 2 {
		t.Fatalf("导出条数=%d want 2", len(items))
	}
	seen := map[string]string{}
	for _, raw := range items {
		var probe struct {
			UID       string `json:"uid"`
			Realm     string `json:"realm"`
			ExpiresAt int64  `json:"expires_at"`
			ID        string `json:"id"`
		}
		if err := json.Unmarshal(raw, &probe); err != nil {
			t.Fatal(err)
		}
		// 毫秒形态（与外部导出 codebuddy_cn_accounts_*.json 同形）
		if probe.ExpiresAt != 1794484878000 {
			t.Errorf("%s expires_at=%d 应为毫秒", probe.UID, probe.ExpiresAt)
		}
		// 回导：同一份 raw 交给导入解析器，字段与域必须一致
		a, err := parseImportAccount(raw, "")
		if err != nil {
			t.Fatalf("%s 回导解析失败: %v", probe.UID, err)
		}
		if a.UID != probe.UID || a.Realm() != probe.Realm || a.ExpiresAt != 1794484878 {
			t.Errorf("%s 回导不一致: uid=%s realm=%s expires=%d", probe.UID, a.UID, a.Realm(), a.ExpiresAt)
		}
		if got := a.AccessTokenValue(); got != "AT-"+strings.TrimPrefix(probe.UID, "uid-") {
			t.Errorf("%s 回导 token=%q", probe.UID, got)
		}
		seen[probe.Realm] = probe.ID
	}
	if !strings.HasPrefix(seen["cn"], "codebuddy_cn_") || !strings.HasPrefix(seen["global"], "codebuddy_global_") {
		t.Errorf("id 前缀应随域：%v", seen)
	}
}

// 所选导出只含指定账号；指定了不存在的 uid 回 404（避免静默导出成空文件）。
func TestAccountsExportSelectedUIDs(t *testing.T) {
	p, dir := newExportPanel(t)
	seedAuth(t, p, dir, "uid-a", "A", "cn", "www.codebuddy.cn", "AT-a")
	seedAuth(t, p, dir, "uid-b", "B", "cn", "www.codebuddy.cn", "AT-b")

	rec := doExport(t, p, `{"uids":["uid-a"]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d", rec.Code)
	}
	var items []struct {
		UID string `json:"uid"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &items); err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].UID != "uid-a" {
		t.Fatalf("所选导出=%+v want 仅 uid-a", items)
	}
	if cd := rec.Header().Get("Content-Disposition"); !strings.Contains(cd, "codebuddy_cn_accounts_") {
		t.Errorf("单域文件名应为 codebuddy_cn_accounts_*：%q", cd)
	}

	rec = doExport(t, p, `{"uids":["nope"]}`)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("不存在 uid: code=%d want 404", rec.Code)
	}
	// 空池导出：400（没有可导出的账号），不是 200 空数组
	p2, _ := newExportPanel(t)
	if rec := doExport(t, p2, `{}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("空池导出 code=%d want 400", rec.Code)
	}
}

// 导出含明文凭证：未带管理面密钥必须 401。
func TestAccountsExportRequiresAuth(t *testing.T) {
	p, dir := newExportPanel(t)
	seedAuth(t, p, dir, "uid-a", "A", "cn", "www.codebuddy.cn", "AT-a")
	req := httptest.NewRequest("POST", "/panel/api/accounts/export", strings.NewReader(`{}`))
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("code=%d want 401", rec.Code)
	}
}

// 一键全部移除：无 confirm 拒绝；带 confirm 清空池并删掉凭证文件。
func TestAccountsRemoveAll(t *testing.T) {
	p, dir := newExportPanel(t)
	seedAuth(t, p, dir, "uid-a", "A", "cn", "www.codebuddy.cn", "AT-a")
	seedAuth(t, p, dir, "uid-b", "B", "cn", "www.codebuddy.cn", "AT-b")

	// 无 confirm：拒绝且不动数据
	req := httptest.NewRequest("POST", "/panel/api/accounts/remove_all", strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer test-key")
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("无 confirm: code=%d want 400", rec.Code)
	}
	if total, _, _, _, _ := p.cfg.Pool.CountsDetailed(); total != 2 {
		t.Fatalf("无 confirm 不应改动账号池: total=%d", total)
	}

	req = httptest.NewRequest("POST", "/panel/api/accounts/remove_all", strings.NewReader(`{"confirm":true}`))
	req.Header.Set("Authorization", "Bearer test-key")
	rec = httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Removed int `json:"removed"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Removed != 2 {
		t.Errorf("removed=%d want 2", resp.Removed)
	}
	if total, _, _, _, _ := p.cfg.Pool.CountsDetailed(); total != 0 {
		t.Errorf("移除后池内仍有 %d 个账号", total)
	}
	for _, uid := range []string{"uid-a", "uid-b"} {
		if _, err := os.Stat(filepath.Join(dir, "workbuddy-"+uid+".json")); !os.IsNotExist(err) {
			t.Errorf("%s 凭证文件未删除", uid)
		}
	}
}

// xlsx 结构：最小必需部件齐全、表头与数据写入、XML 转义与控制字符处理正确。
func TestWriteXLSXStructure(t *testing.T) {
	var buf bytes.Buffer
	rows := [][]string{
		{"肯德基冰淇淋", "2026-10-24", "12<3&4"},
		{"含控制字符\x01的名字", "—", "000012345678"},
	}
	if err := writeXLSX(&buf, "券码", []string{"商品名", "有效期", "券码"}, rows, []float64{24, 16, 30}); err != nil {
		t.Fatal(err)
	}
	zr, err := zip.NewReader(bytes.NewReader(buf.Bytes()), int64(buf.Len()))
	if err != nil {
		t.Fatalf("产物不是合法 zip: %v", err)
	}
	parts := map[string]string{}
	for _, f := range zr.File {
		rc, err := f.Open()
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(rc)
		rc.Close()
		parts[f.Name] = string(body)
	}
	for _, want := range []string{"[Content_Types].xml", "_rels/.rels", "xl/workbook.xml",
		"xl/_rels/workbook.xml.rels", "xl/worksheets/sheet1.xml"} {
		if _, ok := parts[want]; !ok {
			t.Errorf("缺少部件 %s", want)
		}
	}
	sheet := parts["xl/worksheets/sheet1.xml"]
	for _, want := range []string{"商品名", "有效期", "券码", "肯德基冰淇淋", "2026-10-24"} {
		if !strings.Contains(sheet, want) {
			t.Errorf("sheet 缺少 %q", want)
		}
	}
	if !strings.Contains(sheet, "12&lt;3&amp;4") {
		t.Error("XML 特殊字符未转义")
	}
	if strings.Contains(sheet, "\x01") {
		t.Error("控制字符应被剔除（否则 Excel 报文件损坏）")
	}
	if !strings.Contains(sheet, "000012345678") {
		t.Error("券码应按文本写入（保留前导 0）")
	}
	if !strings.Contains(sheet, `t="inlineStr"`) {
		t.Error("应为内联字符串单元格")
	}
}

// 商品名 / 有效期兜底规则（上游字段可能缺失）。
func TestVoucherCellFallbacks(t *testing.T) {
	if got := voucherPrize(upstream.SchoolVoucher{PrizeName: "  ", SKUCode: "voucher_kugou"}); got != "voucher_kugou" {
		t.Errorf("prize_name 空应回落 sku_code，得到 %q", got)
	}
	if got := voucherPrize(upstream.SchoolVoucher{}); got == "" {
		t.Error("两者都空要有占位名")
	}
	if got := voucherValidTo(upstream.SchoolVoucher{ValidTo: "2026-10-24"}); got != "2026-10-24" {
		t.Errorf("valid_to=%q", got)
	}
	if got := voucherValidTo(upstream.SchoolVoucher{ValidFrom: "2026-09-01", ValidTo: "2026-10-24"}); got != "2026-09-01 ~ 2026-10-24" {
		t.Errorf("区间拼接=%q", got)
	}
	if got := voucherValidTo(upstream.SchoolVoucher{}); got != "—" {
		t.Errorf("都空应为占位符，得到 %q", got)
	}
}

// 券码导出端点：未带密钥 401（xlsx 走同一管理面鉴权）。
func TestVouchersXLSXRequiresAuth(t *testing.T) {
	p, _ := newExportPanel(t)
	req := httptest.NewRequest("GET", "/panel/api/school/vouchers.xlsx", nil)
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("code=%d want 401", rec.Code)
	}
}
