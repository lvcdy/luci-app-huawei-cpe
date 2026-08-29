package httpapi

import (
	"encoding/xml"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"huawei-cpe/internal/cache"
	"huawei-cpe/internal/config"
	"huawei-cpe/internal/device"
	"huawei-cpe/internal/poller"
	"huawei-cpe/internal/testutil"
)

// ---- 功能 5/6/7：设备写操作端点（走真实 Manager + MockCPE）----

// writeOpsTestServer 构造连向 mock CPE 的 Server（设备经 manager 直接连 mock）。
func writeOpsTestServer(t *testing.T, mock *testutil.MockCPE) *Server {
	t.Helper()
	srv := httptest.NewServer(mock)
	t.Cleanup(srv.Close)
	host := strings.TrimPrefix(srv.URL, "http://")

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	cfg := &config.Config{
		CPEs: []config.CPE{
			{ID: "cpe1", Name: "dev1", Host: host, Username: "admin", Password: "topsecret", Enabled: true, PollingInterval: 3},
		},
	}
	store := cache.New()
	mgr := device.NewManager(log, cfg.CPEs)
	t.Cleanup(mgr.Close)
	return New(log, cfg, store, mgr)
}

// ---- 功能 10：暂停/恢复轮询（fake PauseController，不连 CPE）----

// fakePause is a stub PauseController for polling endpoint tests.
type fakePause struct {
	suspended atomic.Bool
}

func (f *fakePause) Suspend()          { f.suspended.Store(true) }
func (f *fakePause) Resume()           { f.suspended.Store(false) }
func (f *fakePause) IsSuspended() bool { return f.suspended.Load() }

// pollingTestServer 构造带 pollers 的 Server（恒挂接 fakePause）。
func pollingTestServer(t *testing.T) (*Server, *fakePause) {
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
	s := New(log, cfg, store, mgr)
	fp := &fakePause{}
	s.SetPollers(map[string]PauseController{"cpe1": fp})
	return s, fp
}

// ---- 功能 5：锁频 GET/POST ---

func TestCellLockGetNoSnapshot(t *testing.T) {
	s := writeOpsTestServer(t, testutil.NewMockCPE("admin"))
	code, _ := getJSON(t, s, "/api/v1/devices/cpe1/cell-lock")
	if code != http.StatusNotFound {
		t.Fatalf("code = %d, want 404 (no snapshot)", code)
	}
}

func TestCellLockGetWithSnapshot(t *testing.T) {
	s, store := newTestServer(t)
	store.PutSnapshot("cpe1", poller.Snapshot{
		Lock: poller.LockState{Lock: 1, Freq: "1825", PCI: 301, MaxFreq: "182500"},
	})
	code, m := getJSON(t, s, "/api/v1/devices/cpe1/cell-lock")
	if code != 200 {
		t.Fatalf("code = %d, body %v", code, m)
	}
	if m["lock"].(float64) != 1 || m["freq"] != "1825" ||
		m["pci"].(float64) != 301 || m["maxfreq"] != "182500" {
		t.Fatalf("lock fields = %v", m)
	}
}

func TestCellLockPostSuccess(t *testing.T) {
	mock := testutil.NewMockCPE("admin")
	var got []struct {
		LockCell int `xml:"LockCell"`
		Freq     int `xml:"Freq"`
		PCI      int `xml:"PCI"`
	}
	mock.SetEndpointHandler("net/lock-cell", func(r *http.Request) string {
		var req struct {
			LockCell int `xml:"LockCell"`
			Freq     int `xml:"Freq"`
			PCI      int `xml:"PCI"`
		}
		_ = xml.NewDecoder(r.Body).Decode(&req)
		got = append(got, req)
		return `<response>OK</response>`
	})
	s := writeOpsTestServer(t, mock)

	code, m := doJSON(t, s, http.MethodPost, "/api/v1/devices/cpe1/cell-lock", `{"lock":1,"freq":1825,"pci":0}`)
	if code != 200 || m["locked"] != true {
		t.Fatalf("lock = %d %v, want 200 locked=true", code, m)
	}
	if len(got) != 1 || got[0].LockCell != 1 || got[0].Freq != 1825 || got[0].PCI != 0 {
		t.Fatalf("lock request = %+v, want {1 1825 0}", got)
	}

	code, m = doJSON(t, s, http.MethodPost, "/api/v1/devices/cpe1/cell-lock", `{"lock":0,"freq":0,"pci":0}`)
	if code != 200 || m["locked"] != false {
		t.Fatalf("unlock = %d %v, want 200 locked=false", code, m)
	}
	if len(got) != 2 || got[1].LockCell != 0 {
		t.Fatalf("unlock request = %+v", got)
	}
}

func TestCellLockPostValidation(t *testing.T) {
	mock := testutil.NewMockCPE("admin")
	mock.SetEndpointHandler("net/lock-cell", func(r *http.Request) string {
		return `<response>OK</response>`
	})
	s := writeOpsTestServer(t, mock)

	// 非法 JSON
	code, _ := doJSON(t, s, http.MethodPost, "/api/v1/devices/cpe1/cell-lock", `{`)
	if code != http.StatusBadRequest {
		t.Fatalf("bad json = %d, want 400", code)
	}
	// lock=3（非法枚举）→ device 层校验 → 502
	code, _ = doJSON(t, s, http.MethodPost, "/api/v1/devices/cpe1/cell-lock", `{"lock":3,"freq":1825,"pci":0}`)
	if code != http.StatusBadGateway {
		t.Fatalf("lock=3 = %d, want 502", code)
	}
}

// ---- 功能 5b：网络模式 POST /net-mode ----

func TestNetModePostSuccess(t *testing.T) {
	mock := testutil.NewMockCPE("admin")
	var got []string
	mock.SetEndpointHandler("net/net-mode", func(r *http.Request) string {
		body, _ := io.ReadAll(r.Body)
		got = append(got, string(body))
		return `<response>OK</response>`
	})
	s := writeOpsTestServer(t, mock)

	code, m := doJSON(t, s, http.MethodPost, "/api/v1/devices/cpe1/net-mode", `{"network_mode":"03"}`)
	if code != 200 || m["applied"] != true {
		t.Fatalf("net-mode = %d %v, want 200 applied=true", code, m)
	}
	if len(got) != 1 || !strings.Contains(got[0], "<NetworkMode>03</NetworkMode>") {
		t.Fatalf("net-mode body = %q", got)
	}
}

func TestNetModePostNothingToSet(t *testing.T) {
	s := writeOpsTestServer(t, testutil.NewMockCPE("admin"))
	code, _ := doJSON(t, s, http.MethodPost, "/api/v1/devices/cpe1/net-mode", `{}`)
	if code != http.StatusBadRequest {
		t.Fatalf("empty body = %d, want 400", code)
	}
}

// ---- 功能 6：流量开关 POST /data-switch ----

func TestDataSwitchPost(t *testing.T) {
	mock := testutil.NewMockCPE("admin")
	var got []struct {
		DataSwitch int `xml:"dataswitch"`
	}
	mock.SetEndpointHandler("dialup/mobile-dataswitch", func(r *http.Request) string {
		var req struct {
			DataSwitch int `xml:"dataswitch"`
		}
		_ = xml.NewDecoder(r.Body).Decode(&req)
		got = append(got, req)
		return `<response>OK</response>`
	})
	s := writeOpsTestServer(t, mock)

	code, m := doJSON(t, s, http.MethodPost, "/api/v1/devices/cpe1/data-switch", `{"on":true}`)
	if code != 200 || m["on"] != true {
		t.Fatalf("on = %d %v", code, m)
	}
	code, m = doJSON(t, s, http.MethodPost, "/api/v1/devices/cpe1/data-switch", `{"on":false}`)
	if code != 200 || m["on"] != false {
		t.Fatalf("off = %d %v", code, m)
	}
	if len(got) != 2 || got[0].DataSwitch != 1 || got[1].DataSwitch != 0 {
		t.Fatalf("dataswitch requests = %+v, want [1 0]", got)
	}
}

func TestDataSwitchBadBody(t *testing.T) {
	s := writeOpsTestServer(t, testutil.NewMockCPE("admin"))
	code, _ := doJSON(t, s, http.MethodPost, "/api/v1/devices/cpe1/data-switch", `not-json`)
	if code != http.StatusBadRequest {
		t.Fatalf("bad json = %d, want 400", code)
	}
}

// ---- 功能 7：重启 POST /reboot ----

func TestRebootPost(t *testing.T) {
	mock := testutil.NewMockCPE("admin")
	var got []struct {
		Control int `xml:"Control"`
	}
	mock.SetEndpointHandler("device/control", func(r *http.Request) string {
		var req struct {
			Control int `xml:"Control"`
		}
		_ = xml.NewDecoder(r.Body).Decode(&req)
		got = append(got, req)
		return `<response>OK</response>`
	})
	s := writeOpsTestServer(t, mock)

	code, m := doJSON(t, s, http.MethodPost, "/api/v1/devices/cpe1/reboot", "")
	if code != 200 || m["rebooting"] != true {
		t.Fatalf("reboot = %d %v", code, m)
	}
	if len(got) != 1 || got[0].Control != 1 {
		t.Fatalf("control requests = %+v, want [{1}]", got)
	}
}

func TestWriteOpsUnknownDevice(t *testing.T) {
	s := writeOpsTestServer(t, testutil.NewMockCPE("admin"))
	for _, tc := range []struct{ method, path, body string }{
		{http.MethodPost, "/api/v1/devices/nope/cell-lock", `{"lock":0}`},
		{http.MethodPost, "/api/v1/devices/nope/net-mode", `{"network_mode":"00"}`},
		{http.MethodPost, "/api/v1/devices/nope/data-switch", `{"on":true}`},
		{http.MethodPost, "/api/v1/devices/nope/reboot", ""},
	} {
		code, _ := doJSON(t, s, tc.method, tc.path, tc.body)
		if code != http.StatusNotFound {
			t.Fatalf("%s %s = %d, want 404", tc.method, tc.path, code)
		}
	}
}

// ---- 功能 10：轮询控制端点 ----

func TestPollingSuspendResumeStatus(t *testing.T) {
	s, fp := pollingTestServer(t)

	// 初始：未暂停
	code, m := getJSON(t, s, "/api/v1/devices/cpe1/polling")
	if code != 200 || m["suspended"] != false {
		t.Fatalf("initial status = %d %v, want 200 suspended=false", code, m)
	}

	// 暂停
	code, m = doJSON(t, s, http.MethodPost, "/api/v1/devices/cpe1/polling/suspend", "")
	if code != 200 || m["suspended"] != true {
		t.Fatalf("suspend = %d %v, want 200 suspended=true", code, m)
	}
	if !fp.IsSuspended() {
		t.Fatal("fake poller not suspended")
	}

	// 状态确认
	code, m = getJSON(t, s, "/api/v1/devices/cpe1/polling")
	if code != 200 || m["suspended"] != true {
		t.Fatalf("status after suspend = %d %v", code, m)
	}

	// 恢复
	code, m = doJSON(t, s, http.MethodPost, "/api/v1/devices/cpe1/polling/resume", "")
	if code != 200 || m["suspended"] != false {
		t.Fatalf("resume = %d %v, want 200 suspended=false", code, m)
	}
	if fp.IsSuspended() {
		t.Fatal("fake poller still suspended after Resume")
	}
}

func TestPollingEndpointsGate(t *testing.T) {
	// 未装配 pollers（nil map）→ 503
	s, _ := newTestServer(t)
	code, _ := doJSON(t, s, http.MethodPost, "/api/v1/devices/cpe1/polling/suspend", "")
	if code != http.StatusServiceUnavailable {
		t.Fatalf("suspend without pollers = %d, want 503", code)
	}
	code, _ = getJSON(t, s, "/api/v1/devices/cpe1/polling")
	if code != http.StatusServiceUnavailable {
		t.Fatalf("status without pollers = %d, want 503", code)
	}
	// 未知设备 → 404
	s2, _ := pollingTestServer(t)
	code, _ = doJSON(t, s2, http.MethodPost, "/api/v1/devices/nope/polling/suspend", "")
	if code != http.StatusNotFound {
		t.Fatalf("unknown device suspend = %d, want 404", code)
	}
}