package httpapi

import (
	"database/sql"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"huawei-cpe/internal/cache"
	"huawei-cpe/internal/config"
	"huawei-cpe/internal/db"
	"huawei-cpe/internal/device"
	"huawei-cpe/internal/poller"
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
