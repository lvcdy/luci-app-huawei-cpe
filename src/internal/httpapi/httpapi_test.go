package httpapi

import (
	"database/sql"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"huawei-cpe/internal/cache"
	"huawei-cpe/internal/config"
	"huawei-cpe/internal/db"
	"huawei-cpe/internal/device"
	"huawei-cpe/internal/poller"
	"huawei-cpe/internal/testutil"
)

func newTestServer(t *testing.T) (*Server, *cache.Store) {
	t.Helper()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	cfg := &config.Config{
		CPEs: []config.CPE{
			{ID: "cpe1", Name: "dev1", Host: "192.168.8.1", Username: "admin", Password: "secret", Enabled: true, PollingInterval: 3},
		},
	}
	store := cache.New()
	mgr := device.NewManager(log, cfg.CPEs)
	t.Cleanup(mgr.Close)
	return New(log, cfg, store, mgr), store
}

func getJSON(t *testing.T, s *Server, path string) (int, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	body, _ := io.ReadAll(rec.Body)
	if rec.Code != http.StatusOK {
		return rec.Code, nil
	}
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatalf("json decode %s: %v (%s)", path, err, body)
	}
	return rec.Code, m
}

func TestHealthEndpoint(t *testing.T) {
	s, _ := newTestServer(t)
	code, m := getJSON(t, s, "/api/v1/health")
	if code != 200 || m["status"] != "ok" {
		t.Fatalf("health = %v %v", code, m)
	}
}

func TestDevicesNoSnapshot(t *testing.T) {
	s, _ := newTestServer(t)
	code, m := getJSON(t, s, "/api/v1/devices")
	if code != 200 {
		t.Fatalf("code = %d", code)
	}
	devs := m["devices"].([]any)
	if len(devs) != 1 {
		t.Fatalf("devices = %v", devs)
	}
	d := devs[0].(map[string]any)
	if d["id"] != "cpe1" || d["host"] != "192.168.8.1" {
		t.Fatalf("device fields = %v", d)
	}
	// 脱敏：绝不返回凭据字段
	b, _ := json.Marshal(m)
	if want := "secret"; contains(b, want) {
		t.Fatal("response leaks password")
	}
	if want := "admin"; contains(b, want) {
		t.Fatal("response leaks username")
	}
}

func contains(b []byte, s string) bool {
	for i := 0; i+len(s) <= len(b); i++ {
		if string(b[i:i+len(s)]) == s {
			return true
		}
	}
	return false
}

func TestDeviceByIDWithSnapshot(t *testing.T) {
	s, store := newTestServer(t)
	store.PutSnapshot("cpe1", poller.Snapshot{
		At:      time.Now(),
		Online:  true,
		Signal:  poller.SignalState{RSRP: -85, SINR: 10, Mode: "LTE"},
		Traffic: poller.TrafficState{RxRate: 12.5},
		Network: poller.NetworkState{ProviderName: "TestCarrier"},
	})
	code, m := getJSON(t, s, "/api/v1/devices/cpe1")
	if code != 200 {
		t.Fatalf("code = %d", code)
	}
	if m["online"] != true {
		t.Fatalf("online = %v", m["online"])
	}
	sig := m["signal"].(map[string]any)
	if sig["rsrp"].(float64) != -85 {
		t.Fatalf("rsrp = %v", sig["rsrp"])
	}
}

func TestDeviceByIDUnknown(t *testing.T) {
	s, _ := newTestServer(t)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/devices/nope", nil)
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("code = %d", rec.Code)
	}
}

func TestRoutesNilPanicRecovers(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	cfg := &config.Config{}
	s := New(log, cfg, nil, nil)
	s.mux.HandleFunc("GET /api/v1/boom", func(w http.ResponseWriter, r *http.Request) {
		panic("kaboom")
	})
	req := httptest.NewRequest(http.MethodGet, "/api/v1/boom", nil)
	rec := httptest.NewRecorder()
	s.recoverMiddleware(s.mux).ServeHTTP(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("code = %d", rec.Code)
	}
}

// TestSignalHistoryNoDB 验证历史未启用时降级返回空点集（200 + empty）。
func TestSignalHistoryNoDB(t *testing.T) {
	s, _ := newTestServer(t) // db 为 nil
	code, m := getJSON(t, s, "/api/v1/devices/cpe1/signal/history?bucket=h1")
	if code != 200 {
		t.Fatalf("code = %d", code)
	}
	pts, _ := m["points"].([]any)
	if pts == nil || len(pts) != 0 {
		t.Fatalf("want empty points, got %v", m["points"])
	}
	if m["bucket"] != "h1" {
		t.Errorf("bucket = %v", m["bucket"])
	}
}

// TestSignalHistoryUnknownDevice 验证未知设备返回 404。
func TestSignalHistoryUnknownDevice(t *testing.T) {
	s, _ := newTestServer(t)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/devices/nope/signal/history", nil)
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("code = %d", rec.Code)
	}
}

// TestSignalHistoryWithData 验证真实库中插入数据后端点正确返回。
func TestSignalHistoryWithData(t *testing.T) {
	s, _ := newTestServer(t)
	d := openHistoryDB(t)
	s.SetDB(d)
	insertSignalPoint(t, d, "cpe1", time.Now().Unix()-60, -72)

	code, m := getJSON(t, s, "/api/v1/devices/cpe1/signal/history?bucket=h1")
	if code != 200 {
		t.Fatalf("code = %d", code)
	}
	pts, _ := m["points"].([]any)
	if len(pts) != 1 {
		t.Fatalf("want 1 point, got %d (%v)", len(pts), m["points"])
	}
	p := pts[0].(map[string]any)
	if p["rsrp"].(float64) != -72 {
		t.Errorf("rsrp = %v", p["rsrp"])
	}
}

// TestSignalHistoryBadBucket 验证非法 bucket 返回 400。
func TestSignalHistoryBadBucket(t *testing.T) {
	s, _ := newTestServer(t)
	s.SetDB(openHistoryDB(t))
	req := httptest.NewRequest(http.MethodGet, "/api/v1/devices/cpe1/signal/history?bucket=zzz", nil)
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code = %d", rec.Code)
	}
}

// TestTrafficHistory 验证流量趋势端点（空库返回空集）。
func TestTrafficHistory(t *testing.T) {
	s, _ := newTestServer(t)
	code, m := getJSON(t, s, "/api/v1/devices/cpe1/traffic/history") // 默认 d1
	if code != 200 {
		t.Fatalf("code = %d", code)
	}
	if m["bucket"] != "d1" {
		t.Errorf("default bucket = %v, want d1", m["bucket"])
	}
}

// openHistoryDB 打开一个临时历史库供测试用。
func openHistoryDB(t *testing.T) *sql.DB {
	t.Helper()
	d, err := db.Open(filepath.Join(t.TempDir(), "h.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { d.Close() })
	return d
}

func insertSignalPoint(t *testing.T, d *sql.DB, id string, ts int64, rsrp int) {
	t.Helper()
	if _, err := d.Exec(
		"INSERT INTO signal_history (device_id, ts, rsrp, rsrq, sinr, rssi) VALUES (?, ?, ?, ?, ?, ?)",
		id, ts, rsrp, -10, 8, -55); err != nil {
		t.Fatalf("insert: %v", err)
	}
}

// ---- SMS 端点测试（P3.3）----

// smsTestServer 构造连向 mock CPE 的 Server（带真实 SQLite 库）。
// 返回 server、db、mock、httptest server（需 t.Cleanup 自行关闭）。
func smsTestServer(t *testing.T, mock *testutil.MockCPE) (*Server, *sql.DB, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(mock)
	host := strings.TrimPrefix(srv.URL, "http://")

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	cfg := &config.Config{
		CPEs: []config.CPE{
			{ID: "cpe1", Name: "dev1", Host: host, Username: "admin", Password: "topsecret", Enabled: true, PollingInterval: 60},
		},
	}
	store := cache.New()
	mgr := device.NewManager(log, cfg.CPEs)
	t.Cleanup(mgr.Close)
	s := New(log, cfg, store, mgr)
	d := openHistoryDB(t)
	s.SetDB(d)
	t.Cleanup(srv.Close)
	return s, d, srv
}

// insertSms 直接插入一条短信到本地库，返回本地 id。
func insertSms(t *testing.T, d *sql.DB, deviceID string, idx int, phone, content string, status int, ts int64) int64 {
	t.Helper()
	res, err := d.Exec(
		"INSERT INTO sms (device_id, cpe_index, phone, content, status, received_at) VALUES (?, ?, ?, ?, ?, ?)",
		deviceID, idx, phone, content, status, ts)
	if err != nil {
		t.Fatalf("insert sms: %v", err)
	}
	id, _ := res.LastInsertId()
	return id
}

func doJSON(t *testing.T, s *Server, method, path string, body string) (int, map[string]any) {
	t.Helper()
	var r io.Reader
	if body != "" {
		r = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, r)
	if body != "" {
		req.Header.Set("Content-Type", "application/xml") // 不强依赖，仅示意
	}
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	b, _ := io.ReadAll(rec.Body)
	if len(strings.TrimSpace(string(b))) == 0 {
		return rec.Code, nil
	}

	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("json decode %s %s: %v (%s)", method, path, err, b)
	}
	return rec.Code, m
}

// setReadHandler 挂载 sms/set-read 动态响应；返回请求 Index 记录通道。
// body: "<response>OK</response>"（成功）或带 error 的 XML（失败）。
func setReadHandler(mock *testutil.MockCPE, resp string, got *[]int) {
	mock.SetEndpointHandler("sms/set-read", func(r *http.Request) string {
		var req struct {
			Index int `xml:"Index"`
		}
		_ = xml.NewDecoder(r.Body).Decode(&req)
		*got = append(*got, req.Index)
		return resp
	})
}

// deleteSmsHandler 挂载 sms/delete-sms 动态响应。
func deleteSmsHandler(mock *testutil.MockCPE, resp string, got *[]int) {
	mock.SetEndpointHandler("sms/delete-sms", func(r *http.Request) string {
		var req struct {
			Index int `xml:"Index"`
		}
		_ = xml.NewDecoder(r.Body).Decode(&req)
		*got = append(*got, req.Index)
		return resp
	})
}

func TestSmsListEmptyDB(t *testing.T) {
	s, _, _ := smsTestServer(t, testutil.NewMockCPE("admin"))
	code, m := getJSON(t, s, "/api/v1/devices/cpe1/sms")
	if code != 200 {
		t.Fatalf("code = %d", code)
	}
	msgs, _ := m["messages"].([]any)
	if len(msgs) != 0 {
		t.Fatalf("want empty list, got %v", msgs)
	}
	if m["unread_count"].(float64) != 0 {
		t.Fatalf("unread = %v", m["unread_count"])
	}
}

func TestSmsListNoHistoryDB(t *testing.T) {
	s, _ := newTestServer(t) // db nil
	code, m := getJSON(t, s, "/api/v1/devices/cpe1/sms")
	if code != 200 {
		t.Fatalf("code = %d", code)
	}
	msgs, _ := m["messages"].([]any)
	if len(msgs) != 0 {
		t.Fatalf("want empty list with nil db, got %v", msgs)
	}
	if m["unread_count"].(float64) != 0 {
		t.Fatalf("unread = %v", m["unread_count"])
	}
}

func TestSmsListFilterAndSearch(t *testing.T) {
	s, d, _ := smsTestServer(t, testutil.NewMockCPE("admin"))
	_ = d
	insertSms(t, d, "cpe1", 1, "+1000", "hello alpha", 0, 100)
	insertSms(t, d, "cpe1", 2, "+2000", "world beta", 1, 200)
	insertSms(t, d, "cpe1", 3, "+3000", "gamma", 0, 300)

	// 全部
	code, m := getJSON(t, s, "/api/v1/devices/cpe1/sms?filter=all")
	if code != 200 {
		t.Fatalf("code = %d", code)
	}
	msgs := m["messages"].([]any)
	if len(msgs) != 3 {
		t.Fatalf("all = %d, want 3", len(msgs))
	}
	if m["unread_count"].(float64) != 2 {
		t.Fatalf("unread_count = %v, want 2", m["unread_count"])
	}
	// 倒序（received_at desc）
	first := msgs[0].(map[string]any)
	if first["cpe_index"].(float64) != 3 {
		t.Fatalf("first = %v, want idx 3 (newest first)", first["cpe_index"])
	}

	// 未读筛选
	code, m = getJSON(t, s, "/api/v1/devices/cpe1/sms?filter=unread")
	if code != 200 {
		t.Fatalf("code = %d", code)
	}
	msgs = m["messages"].([]any)
	if len(msgs) != 2 {
		t.Fatalf("unread = %d, want 2", len(msgs))
	}

	// 搜索 phone
	code, m = getJSON(t, s, "/api/v1/devices/cpe1/sms?filter=all&search=2000")
	if code != 200 {
		t.Fatalf("code = %d", code)
	}
	msgs = m["messages"].([]any)
	if len(msgs) != 1 {
		t.Fatalf("search=2000 = %d, want 1", len(msgs))
	}

	// 搜索 content
	code, m = getJSON(t, s, "/api/v1/devices/cpe1/sms?filter=all&search=gamma")
	if code != 200 {
		t.Fatalf("code = %d", code)
	}
	msgs = m["messages"].([]any)
	if len(msgs) != 1 {
		t.Fatalf("search=gamma = %d, want 1", len(msgs))
	}

	// 未知设备
	code, _ = getJSON(t, s, "/api/v1/devices/nope/sms")
	if code != http.StatusNotFound {
		t.Fatalf("unknown device code = %d, want 404", code)
	}
}

func TestSmsMarkReadBidirectional(t *testing.T) {
	mock := testutil.NewMockCPE("admin")
	var got []int
	setReadHandler(mock, `<?xml version="1.0" encoding="UTF-8"?><response>OK</response>`, &got)

	s, d, _ := smsTestServer(t, mock)
	// 先入库一条未读（cpe_index=5）
	_ = insertSms(t, d, "cpe1", 5, "+1000", "some text", 0, 100)
	var unreadBefore int
	_ = d.QueryRow("SELECT COUNT(*) FROM sms WHERE device_id='cpe1' AND status=0 AND read_local=0").Scan(&unreadBefore)
	if unreadBefore != 1 {
		t.Fatalf("unread before = %d, want 1", unreadBefore)
	}

	code, m := doJSON(t, s, http.MethodPost, "/api/v1/devices/cpe1/sms/1/read", "")
	if code != 200 {
		t.Fatalf("code = %d, body %v", code, m)
	}
	if len(got) != 1 || got[0] != 5 {
		t.Fatalf("SetRead called with %v, want [5]", got)
	}
	// 本地 read_local=1
	var rl int
	_ = d.QueryRow("SELECT read_local FROM sms WHERE id=1").Scan(&rl)
	if rl != 1 {
		t.Fatalf("read_local = %d, want 1", rl)
	}
	// 未读计数降为 0
	var unreadAfter int
	_ = d.QueryRow("SELECT COUNT(*) FROM sms WHERE device_id='cpe1' AND status=0 AND read_local=0").Scan(&unreadAfter)
	if unreadAfter != 0 {
		t.Fatalf("unread after = %d, want 0", unreadAfter)
	}
}

func TestSmsMarkReadCpeFailure(t *testing.T) {
	mock := testutil.NewMockCPE("admin")
	var got []int
	// CPE 返回 error 100002（不支持）
	setReadHandler(mock, `<?xml version="1.0" encoding="UTF-8"?><error><code>100002</code><message></message></error>`, &got)

	s, d, _ := smsTestServer(t, mock)
	insertSms(t, d, "cpe1", 9, "+1000", "body", 0, 100)

	code, _ := doJSON(t, s, http.MethodPost, "/api/v1/devices/cpe1/sms/1/read", "")
	if code != http.StatusBadGateway {
		t.Fatalf("code = %d, want 502", code)
	}
	if len(got) != 1 || got[0] != 9 {
		t.Fatalf("SetRead called with %v, want [9]", got)
	}
	// 本地不可变（仍未读）
	var rl int
	_ = d.QueryRow("SELECT read_local FROM sms WHERE id=1").Scan(&rl)
	if rl != 0 {
		t.Fatalf("read_local = %d, want 0 (unchanged)", rl)
	}
}

func TestSmsMarkReadUnknownSms(t *testing.T) {
	s, _, _ := smsTestServer(t, testutil.NewMockCPE("admin"))
	code, _ := doJSON(t, s, http.MethodPost, "/api/v1/devices/cpe1/sms/999/read", "")
	if code != http.StatusNotFound {
		t.Fatalf("code = %d, want 404", code)
	}
}

func TestSmsDeleteBidirectional(t *testing.T) {
	mock := testutil.NewMockCPE("admin")
	var got []int
	deleteSmsHandler(mock, `<?xml version="1.0" encoding="UTF-8"?><response>OK</response>`, &got)

	s, d, _ := smsTestServer(t, mock)
	insertSms(t, d, "cpe1", 7, "+1000", "delete me", 0, 100)

	code, m := doJSON(t, s, http.MethodDelete, "/api/v1/devices/cpe1/sms/1", "")
	if code != 200 {
		t.Fatalf("code = %d, body %v", code, m)
	}
	if len(got) != 1 || got[0] != 7 {
		t.Fatalf("DeleteSms called with %v, want [7]", got)
	}
	var n int
	_ = d.QueryRow("SELECT COUNT(*) FROM sms WHERE id=1").Scan(&n)
	if n != 0 {
		t.Fatalf("local row after delete = %d, want 0", n)
	}
}

func TestSmsDeleteCpeFailure(t *testing.T) {
	mock := testutil.NewMockCPE("admin")
	var got []int
	deleteSmsHandler(mock, `<?xml version="1.0" encoding="UTF-8"?><error><code>100002</code><message></message></error>`, &got)

	s, d, _ := smsTestServer(t, mock)
	insertSms(t, d, "cpe1", 8, "+1000", "keep me", 0, 100)

	code, _ := doJSON(t, s, http.MethodDelete, "/api/v1/devices/cpe1/sms/1", "")
	if code != http.StatusBadGateway {
		t.Fatalf("code = %d, want 502", code)
	}
	if len(got) != 1 || got[0] != 8 {
		t.Fatalf("DeleteSms called with %v, want [8]", got)
	}
	var n int
	_ = d.QueryRow("SELECT COUNT(*) FROM sms WHERE id=1").Scan(&n)
	if n != 1 {
		t.Fatalf("local row after failed delete = %d, want 1 (unchanged)", n)
	}
}

func TestSmsBadSmsID(t *testing.T) {
	s, _, _ := smsTestServer(t, testutil.NewMockCPE("admin"))
	code, _ := doJSON(t, s, http.MethodPost, "/api/v1/devices/cpe1/sms/abc/read", "")
	if code != http.StatusBadRequest {
		t.Fatalf("code = %d, want 400", code)
	}
}

// 确保 fmt 被使用（前置 import 检查）
var _ = fmt.Sprintf
