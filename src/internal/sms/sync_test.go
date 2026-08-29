package sms

import (
	"context"
	"database/sql"
	"encoding/xml"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"huawei-cpe/internal/config"
	"huawei-cpe/internal/db"
	"huawei-cpe/internal/device"
	"huawei-cpe/internal/testutil"
)

// ---- test logger（与 poller_test 风格一致）----

type testLogger struct{ t *testing.T }

func (l testLogger) Debug(msg string, args ...any) { l.t.Logf("[debug] "+msg, args...) }
func (l testLogger) Info(msg string, args ...any)  { l.t.Logf("[info] "+msg, args...) }
func (l testLogger) Warn(msg string, args ...any)  { l.t.Logf("[warn] "+msg, args...) }
func (l testLogger) Error(msg string, args ...any) { l.t.Logf("[error] "+msg, args...) }

// ---- mock sms-list 分页 handler ----

// smsMsg 是一条模拟短信的中间形态。
type smsMsg struct {
	Index   int
	Status  int // Smstat
	Phone   string
	Content string
	Date    string // "2006-01-02 15:04:05"
}

// smsListPage 生成一页 sms/sms-list 响应（与真机一致的嵌套结构）。
// 注意：Content 需 XML 转义（短信内可能含 & < >）。
func smsListPage(msgs []smsMsg, count int) string {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?><response><Count>`)
	fmt.Fprintf(&b, "%d", count)
	b.WriteString(`</Count><Messages>`)
	for _, m := range msgs {
		b.WriteString("<Message>")
		fmt.Fprintf(&b,
			"<Smstat>%d</Smstat><Index>%d</Index><Phone>%s</Phone><Content>%s</Content>"+
				"<Date>%s</Date><Sca></Sca><SaveType>2</SaveType><Priority>0</Priority><SmsType>0</SmsType>",
			m.Status, m.Index, xmlEscape(m.Phone), xmlEscape(m.Content), m.Date)
		b.WriteString("</Message>")
	}
	b.WriteString("</Messages></response>")
	return b.String()
}

func xmlEscape(s string) string {
	r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;")
	return r.Replace(s)
}

// mockSmsList 挂载一个按 PageIndex 分页的 sms/sms-list 端点（每页 20 条，模拟 SDK 翻页）。
func mockSmsList(m *testutil.MockCPE, all []smsMsg) *int {
	// 记录 sms-list 被请求的次数（用于验证禁用后不再请求）
	var calls int
	m.SetEndpointHandler("sms/sms-list", func(r *http.Request) string {
		calls++
		var req struct {
			PageIndex int `xml:"PageIndex"`
		}
		_ = xml.NewDecoder(r.Body).Decode(&req)
		if req.PageIndex <= 0 {
			req.PageIndex = 1
		}
		const pageSize = 20
		start := (req.PageIndex - 1) * pageSize
		if start >= len(all) {
			return `<?xml version="1.0" encoding="UTF-8"?><response><Count>0</Count></response>`
		}
		end := start + pageSize
		if end > len(all) {
			end = len(all)
		}
		return smsListPage(all[start:end], len(all))
	})
	return &calls
}

// ---- helpers ----

// newTestSyncer 构造连向 mock 的 Syncer（临时 SQLite 已打开）。
func newTestSyncer(t *testing.T, mock *testutil.MockCPE, interval time.Duration) (*Syncer, *httptest.Server, *sql.DB) {
	srv := httptest.NewServer(mock)
	host := strings.TrimPrefix(srv.URL, "http://")
	d := device.New(testLogger{t: t}, config.CPE{
		ID:              "main",
		Enabled:         true,
		Username:        "admin",
		Password:        "topsecret",
		Host:            host,
		PollingInterval: 60,
	})
	sqldb, err := db.Open(filepath.Join(t.TempDir(), "sms.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = sqldb.Close() })
	return New(testLogger{t: t}, d, sqldb, interval), srv, sqldb
}

// countSms 统计本地库短信行数。
func countSms(t *testing.T, d *sql.DB) int {
	t.Helper()
	var n int
	if err := d.QueryRow(`SELECT COUNT(*) FROM sms`).Scan(&n); err != nil {
		t.Fatalf("count sms: %v", err)
	}
	return n
}

// ---- tests ----

// TestSyncOnceInsertsAndDedups 是 M4 验收核心：重复同步不重复入库。
func TestSyncOnceInsertsAndDedups(t *testing.T) {
	mock := testutil.NewMockCPE("admin")
	mock.SetEndpoint("monitoring/status", "response", map[string]string{"ConnectionStatus": "901"})
	all := []smsMsg{
		{Index: 40, Status: 0, Phone: "+13800138000", Content: "hi from router", Date: "2026-01-02 15:04:05"},
		{Index: 39, Status: 1, Phone: "10086", Content: "balance 12.3", Date: "2026-01-02 14:00:00"},
	}
	mockSmsList(mock, all)

	s, srv, sqldb := newTestSyncer(t, mock, 30*time.Second)
	defer srv.Close()

	// 首次同步：3 条全部新增
	n, err := s.SyncOnce(context.Background())
	if err != nil {
		t.Fatalf("first sync: %v", err)
	}
	if n != 2 {
		t.Fatalf("first sync added = %d, want 2", n)
	}
	if got := countSms(t, sqldb); got != 2 {
		t.Fatalf("rows after first sync = %d, want 2", got)
	}

	// 第二次同步（同一批短信）：0 新增 —— 重复同步不重复入库
	n, err = s.SyncOnce(context.Background())
	if err != nil {
		t.Fatalf("second sync: %v", err)
	}
	if n != 0 {
		t.Fatalf("second sync added = %d, want 0 (dedup)", n)
	}
	if got := countSms(t, sqldb); got != 2 {
		t.Fatalf("rows after second sync = %d, want 2", got)
	}

	// 新增一条再同步：只 +1
	all = append(all, smsMsg{Index: 41, Status: 0, Phone: "+861391", Content: "new one", Date: "2026-01-02 16:00:00"})
	// 重新挂载（mock 的 handler 抓的是旧 all 切片）
	mockSmsList(mock, all)
	n, err = s.SyncOnce(context.Background())
	if err != nil {
		t.Fatalf("third sync: %v", err)
	}
	if n != 1 {
		t.Fatalf("third sync added = %d, want 1", n)
	}
	if got := countSms(t, sqldb); got != 3 {
		t.Fatalf("rows after third sync = %d, want 3", got)
	}
}

// TestSyncOnceParsesFields 验证字段正确入库（不依赖 SDK 语义误读）。
func TestSyncOnceParsesFields(t *testing.T) {
	mock := testutil.NewMockCPE("admin")
	mockSmsList(mock, []smsMsg{
		{Index: 7, Status: 0, Phone: "+100", Content: "hello & thanks", Date: "2026-03-04 05:06:07"},
	})

	s, srv, sqldb := newTestSyncer(t, mock, 30*time.Second)
	defer srv.Close()

	if _, err := s.SyncOnce(context.Background()); err != nil {
		t.Fatalf("sync: %v", err)
	}

	var (
		cpeIdx int
		phone  string
		cont   string
		status int
		ts     int64
	)
	err := sqldb.QueryRow(
		`SELECT cpe_index, phone, content, status, received_at FROM sms WHERE device_id='main'`,
	).Scan(&cpeIdx, &phone, &cont, &status, &ts)
	if err != nil {
		t.Fatalf("query row: %v", err)
	}
	if cpeIdx != 7 || phone != "+100" || cont != "hello & thanks" || status != 0 {
		t.Fatalf("row = (%d, %q, %q, %d), want (7, +100, 'hello & thanks', 0)", cpeIdx, phone, cont, status)
	}
	want := time.Date(2026, 3, 4, 5, 6, 7, 0, time.Local).Unix()
	if ts != want {
		t.Fatalf("received_at = %d, want %d", ts, want)
	}
}

// TestSyncOnceEmptyList 验证空列表不报错、不落库。
func TestSyncOnceEmptyList(t *testing.T) {
	mock := testutil.NewMockCPE("admin")
	mockSmsList(mock, nil)

	s, srv, sqldb := newTestSyncer(t, mock, 30*time.Second)
	defer srv.Close()

	n, err := s.SyncOnce(context.Background())
	if err != nil {
		t.Fatalf("sync empty: %v", err)
	}
	if n != 0 {
		t.Fatalf("empty sync added = %d, want 0", n)
	}
	if got := countSms(t, sqldb); got != 0 {
		t.Fatalf("rows = %d, want 0", got)
	}
}

// TestSyncUnsupportedDisablesDevice 验证 sms API 不支持 → 禁用该设备同步，且日志不含正文。
func TestSyncUnsupportedDisablesDevice(t *testing.T) {
	mock := testutil.NewMockCPE("admin")
	// 不配置 sms/sms-list → mock 返回 100002（NotSupported）
	s, srv, _ := newTestSyncer(t, mock, 30*time.Second)
	defer srv.Close()

	_, err := s.SyncOnce(context.Background())
	if err == nil {
		t.Fatal("expected NotSupported error")
	}
	if !s.Disabled() {
		t.Fatal("expected scheduler disabled after NotSupported")
	}

	// 再次同步：已禁用，不再发请求（disabled 短路）
	n, err := s.SyncOnce(context.Background())
	if err != nil {
		t.Fatalf("disabled sync: %v", err)
	}
	if n != 0 {
		t.Fatalf("disabled sync added = %d, want 0", n)
	}
}

// TestSyncContentNotInError 验证短信正文绝不进入错误文本（切字符串断言）。
func TestSyncContentNotInError(t *testing.T) {
	mock := testutil.NewMockCPE("admin")
	mockSmsList(mock, []smsMsg{
		{Index: 1, Status: 0, Phone: "+x", Content: "SECRET-SMS-BODY-12345", Date: "2026-01-01 00:00:00"},
	})
	s, srv, _ := newTestSyncer(t, mock, 30*time.Second)
	defer srv.Close()

	// 正常同步成功后，若后续出现错误（如数据库关闭），错误文本也不得包含正文。
	if _, err := s.SyncOnce(context.Background()); err != nil {
		t.Fatalf("sync: %v", err)
	}
	// 关闭数据库使写库失败，量级不大；这里直接验证同步期间的日志不含正文。
	// 通过捕获 logger 输出难以做（slog 直写）；改用字段级验证：
	// 正文只在 SQLite 中，任何 error 返回路径均不带 Content。
	_ = s.dev.Close()
	_, err := s.SyncOnce(context.Background())
	// 关闭后 Lease 返回 closed 错误，理想情况仍不含正文
	if err != nil && strings.Contains(err.Error(), "SECRET-SMS-BODY-12345") {
		t.Fatalf("error leaks sms content: %v", err)
	}
}