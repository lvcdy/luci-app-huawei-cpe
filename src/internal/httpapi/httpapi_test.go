package httpapi

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"huawei-cpe/internal/cache"
	"huawei-cpe/internal/config"
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
