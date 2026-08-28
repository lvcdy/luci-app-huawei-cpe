package poller

import (
	"context"
	"encoding/xml"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"huawei-cpe/internal/config"
	"huawei-cpe/internal/device"
)

// ---- mock CPE server（复用 device_test.go 的握手协议，扩充轮询端点）----

// endpointSnap 是一个端点返回的固定 XML 快照。
type endpointSnap struct {
	root string            // XML 根节点名（如 response/currentplmn）
	body map[string]string // 键值对
}

type mockCPE struct {
	t        *testing.T
	username string
	token    string

	loginCount atomic.Int32
	loggedIn   atomic.Bool

	mu    sync.Mutex
	snaps map[string]endpointSnap // 端点 → 快照
}

func newMockCPE(t *testing.T) *mockCPE {
	return &mockCPE{
		t:        t,
		username: "admin",
		token:    "test-csrf-token-abcdef123456",
		snaps:    map[string]endpointSnap{},
	}
}

// setEndpoint 设置某端点的固定 XML 快照（键值对）。
func (m *mockCPE) setEndpoint(ep string, root string, body map[string]string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.snaps[ep] = endpointSnap{root: root, body: body}
}

// xmlResp 输出固定 XML 快照。
func (m *mockCPE) xmlResp(w http.ResponseWriter, root string, body map[string]string) {
	var b strings.Builder
	b.WriteString("<?xml version=\"1.0\" encoding=\"UTF-8\"?>\n")
	b.WriteString("<" + root + ">")
	for k, v := range body {
		b.WriteString("<" + k + ">" + xmlEscape(v) + "</" + k + ">")
	}
	b.WriteString("</" + root + ">")
	w.Header().Set("Content-Type", "text/xml")
	w.WriteHeader(200)
	_, _ = w.Write([]byte(b.String()))
}

func xmlEscape(s string) string {
	r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;")
	return r.Replace(s)
}

// errResp 输出 error 响应（错误响应不回传新 token，与真机一致）。
func (m *mockCPE) errResp(w http.ResponseWriter, code int) {
	w.Header().Set("Content-Type", "text/xml")
	w.WriteHeader(200)
	body := fmt.Sprintf("<?xml version=\"1.0\" encoding=\"UTF-8\"?>\n<error><code>%d</code><message></message></error>", code)
	_, _ = w.Write([]byte(body))
}

// Handler 返回 mock 的 http.Handler。
func (m *mockCPE) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		if path == "" || path == "/" {
			w.Header().Set("Content-Type", "text/html")
			_, _ = fmt.Fprintf(w, `<html><head><meta name="csrf_token" content="%s"></head><body>Huawei CPE</body></html>`, m.token)
			return
		}
		if !strings.HasPrefix(path, "/api/") {
			m.errResp(w, 125001)
			return
		}
		ep := strings.TrimPrefix(path, "/api/")

		// 真机行为：每个 API 响应头都回传 CSRF token，SDK 凭此维持登录后队列。
		w.Header().Set("__RequestVerificationToken", m.token)

		reqToken := r.Header.Get("__RequestVerificationToken")
		if reqToken != m.token {
			m.errResp(w, 125003)
			return
		}

		switch ep {
		case "user/state-login":
			state := "-1"
			if m.loggedIn.Load() {
				state = "0"
			}
			m.xmlResp(w, "response", map[string]string{
				"State":         state,
				"password_type": "4",
			})
		case "user/login":
			m.loginCount.Add(1)
			var req struct {
				Username     string `xml:"Username"`
				Password     string `xml:"Password"`
				PasswordType string `xml:"password_type"`
			}
			_ = xml.NewDecoder(r.Body).Decode(&req)
			if req.Username != m.username || req.Password == "" {
				m.errResp(w, 108001)
				return
			}
			m.loggedIn.Store(true)
			w.Header().Set("Content-Type", "text/xml")
			w.WriteHeader(200)
			_, _ = w.Write([]byte(`<?xml version="1.0" encoding="UTF-8"?><response>OK</response>`))
		default:
			m.mu.Lock()
			snap, ok := m.snaps[ep]
			m.mu.Unlock()
			if ok {
				m.xmlResp(w, snap.root, snap.body)
				return
			}
			// 未配置的端点：返回不支持（100002），测试能力矩阵
			m.errResp(w, 100002)
		}
	})
}

// ---- test logger ----

type testLogger struct {
	t *testing.T
}

// formatArgs 将 slog 风格键值对格式化为可读文本（避免 Logf 的 %!(EXTRA) 噪音）。
func formatArgs(args ...any) string {
	var b strings.Builder
	for i := 0; i+1 < len(args); i += 2 {
		fmt.Fprintf(&b, " %v=%v", args[i], args[i+1])
	}
	return b.String()
}

func (l testLogger) Debug(msg string, args ...any) { l.t.Logf("[debug] %s%s", msg, formatArgs(args...)) }
func (l testLogger) Info(msg string, args ...any)  { l.t.Logf("[info] %s%s", msg, formatArgs(args...)) }
func (l testLogger) Warn(msg string, args ...any)  { l.t.Logf("[warn] %s%s", msg, formatArgs(args...)) }
func (l testLogger) Error(msg string, args ...any) { l.t.Logf("[error] %s%s", msg, formatArgs(args...)) }

// ---- helpers ----

// newTestDevice 构造连向 mock server 的 device。
func newTestDevice(t *testing.T, srv *httptest.Server) *device.Device {
	host := strings.TrimPrefix(srv.URL, "http://")
	return device.New(testLogger{t: t}, config.CPE{
		ID:              "main",
		Enabled:         true,
		Username:        "admin",
		Password:        "topsecret",
		Host:            host,
		PollingInterval: 60,
	})
}

// newTestPoll 构造连向 mock server 的 poller。
func newTestPoll(t *testing.T, mock *mockCPE) (*Poller, *httptest.Server) {
	srv := httptest.NewServer(mock.Handler())
	dev := newTestDevice(t, srv)
	return New(testLogger{t: t}, dev), srv
}

// ---- tests ----

// TestSnapshotFields 验证 mock 返回固定 XML 快照 → 快照字段正确（P1.7 验收）。
func TestSnapshotFields(t *testing.T) {
	mock := newMockCPE(t)
	p, srv := newTestPoll(t, mock)
	defer srv.Close()

	mock.setEndpoint("monitoring/status", "response", map[string]string{
		"ConnectionStatus":     "901",
		"CurrentNetworkType":   "LTE",
		"CurrentServiceDomain": "CS_PS",
		"Roaming":              "0",
	})
	mock.setEndpoint("device/information", "response", map[string]string{
		"DeviceName":      "B315s-936",
		"SerialNumber":    "S12345",
		"SoftwareVersion": "22.300.16.00.00",
		"HardwareVersion": "HG9A100X",
	})
	mock.setEndpoint("device/signal", "response", map[string]string{
		"rsrp": "-75",
		"rsrq": "-8",
		"sinr": "22",
		"rssi": "-65",
		"mode": "4G",
	})
	mock.setEndpoint("net/network", "response", map[string]string{
		"CurrentNetworkType":   "LTE",
		"CurrentServiceDomain": "CS_PS",
		"Roaming":              "0",
	})
	// 真实设备返回 <currentplmn> 根（保留为顶层键，poller 需嵌套解析）
	mock.setEndpoint("net/current-plmn", "currentplmn", map[string]string{
		"ProviderName": "CHINA MOBILE",
		"ShortName":    "CMCC",
	})
	mock.setEndpoint("monitoring/traffic-statistics", "response", map[string]string{
		"CurrentDownload": "123456",
		"CurrentUpload":   "654321",
		"TotalDownload":   "1000000000",
		"TotalUpload":     "200000000",
	})
	mock.setEndpoint("monitoring/month_statistics", "response", map[string]string{
		"MonthReceive":  "900000000",
		"MonthTransmit": "150000000",
	})

	ctx := context.Background()
	p.pollOnce(ctx)

	snap := p.Last()
	if !snap.Online {
		t.Fatalf("expected online, got %v", snap.Online)
	}
	if snap.Network.CurrentNetworkType != "LTE" {
		t.Errorf("CurrentNetworkType = %q, want LTE", snap.Network.CurrentNetworkType)
	}
	if snap.Network.CurrentServiceDomain != "CS_PS" {
		t.Errorf("CurrentServiceDomain = %q, want CS_PS", snap.Network.CurrentServiceDomain)
	}
	if snap.Network.Roaming != 0 {
		t.Errorf("Roaming = %d, want 0", snap.Network.Roaming)
	}
	if snap.Signal.RSRP != -75 {
		t.Errorf("RSRP = %d, want -75", snap.Signal.RSRP)
	}
	if snap.Signal.RSRQ != -8 {
		t.Errorf("RSRQ = %d, want -8", snap.Signal.RSRQ)
	}
	if snap.Signal.SINR != 22 {
		t.Errorf("SINR = %d, want 22", snap.Signal.SINR)
	}
	if snap.Signal.RSSI != -65 {
		t.Errorf("RSSI = %d, want -65", snap.Signal.RSSI)
	}
	if snap.Signal.Mode != "4G" {
		t.Errorf("Mode = %q, want 4G", snap.Signal.Mode)
	}
	if snap.Network.ProviderName != "CHINA MOBILE" {
		t.Errorf("ProviderName = %q, want CHINA MOBILE", snap.Network.ProviderName)
	}
	if snap.Network.ShortName != "CMCC" {
		t.Errorf("ShortName = %q, want CMCC", snap.Network.ShortName)
	}
	if snap.Traffic.TotalRxBytes != 1000000000 {
		t.Errorf("TotalRxBytes = %d, want 1000000000", snap.Traffic.TotalRxBytes)
	}
	if snap.Traffic.TotalTxBytes != 200000000 {
		t.Errorf("TotalTxBytes = %d, want 200000000", snap.Traffic.TotalTxBytes)
	}
	if snap.Traffic.CurrentRxBytes != 123456 {
		t.Errorf("CurrentRxBytes = %d, want 123456", snap.Traffic.CurrentRxBytes)
	}
	if snap.Traffic.CurrentTxBytes != 654321 {
		t.Errorf("CurrentTxBytes = %d, want 654321", snap.Traffic.CurrentTxBytes)
	}
	if snap.Traffic.MonthRxBytes != 900000000 {
		t.Errorf("MonthRxBytes = %d, want 900000000", snap.Traffic.MonthRxBytes)
	}
	if snap.Traffic.MonthTxBytes != 150000000 {
		t.Errorf("MonthTxBytes = %d, want 150000000", snap.Traffic.MonthTxBytes)
	}
	if snap.Info["SoftwareVersion"] != "22.300.16.00.00" {
		t.Errorf("Info[SoftwareVersion] = %v, want 22.300.16.00.00", snap.Info["SoftwareVersion"])
	}
	if snap.Info["DeviceName"] != "B315s-936" {
		t.Errorf("Info[DeviceName] = %v, want B315s-936", snap.Info["DeviceName"])
	}
}

// TestDiffRate 验证差分速率：Total 增长 → RxRate/TxRate > 0；首次为 0。
func TestDiffRate(t *testing.T) {
	mock := newMockCPE(t)
	p, srv := newTestPoll(t, mock)
	defer srv.Close()

	setTotals := func(down, up string) {
		mock.setEndpoint("monitoring/traffic-statistics", "response", map[string]string{
			"TotalDownload": down,
			"TotalUpload":   up,
		})
	}

	ctx := context.Background()
	setTotals("1000", "500")
	p.pollOnce(ctx)
	snap := p.Last()
	if snap.Traffic.RxRate != 0 || snap.Traffic.TxRate != 0 {
		t.Fatalf("first poll: rates must be 0, got rx=%v tx=%v", snap.Traffic.RxRate, snap.Traffic.TxRate)
	}

	// 第二次轮询 Total 增长；保证两次 pollOnce 时间差 > 0
	time.Sleep(50 * time.Millisecond)
	setTotals("3400", "1100")
	p.pollOnce(ctx)
	snap = p.Last()
	// Δrx=2400，Δt≥0.05s → rx rate ≤ 48000 bytes/s，且 > 0
	if snap.Traffic.RxRate <= 0 {
		t.Errorf("RxRate = %v, want > 0", snap.Traffic.RxRate)
	}
	if snap.Traffic.TxRate <= 0 {
		t.Errorf("TxRate = %v, want > 0", snap.Traffic.TxRate)
	}
	if snap.Traffic.RxRate > 48001 {
		t.Errorf("RxRate = %v, want <= 48000 (dt≥50ms)", snap.Traffic.RxRate)
	}
}

// TestTotalReset 验证 CPE 重启计数复位（Total 回退）→ 速率归零不产生负值。
func TestTotalReset(t *testing.T) {
	mock := newMockCPE(t)
	p, srv := newTestPoll(t, mock)
	defer srv.Close()

	setTotals := func(down, up string) {
		mock.setEndpoint("monitoring/traffic-statistics", "response", map[string]string{
			"TotalDownload": down,
			"TotalUpload":   up,
		})
	}

	ctx := context.Background()
	setTotals("5000", "3000")
	p.pollOnce(ctx)
	time.Sleep(20 * time.Millisecond)
	// Total 回退（换卡/重启）
	setTotals("100", "50")
	p.pollOnce(ctx)
	snap := p.Last()
	if snap.Traffic.RxRate < 0 {
		t.Errorf("RxRate = %v, want >= 0 after reset", snap.Traffic.RxRate)
	}
	if snap.Traffic.TxRate < 0 {
		t.Errorf("TxRate = %v, want >= 0 after reset", snap.Traffic.TxRate)
	}
}

// TestUnsupportedCached 验证能力矩阵：无快照端点返回 100002 → 能力置 false。
func TestUnsupportedCached(t *testing.T) {
	mock := newMockCPE(t)
	p, srv := newTestPoll(t, mock)
	defer srv.Close()

	// 只配置 status/information，其余全部 100002
	mock.setEndpoint("monitoring/status", "response", map[string]string{
		"ConnectionStatus": "901",
	})
	mock.setEndpoint("device/information", "response", map[string]string{
		"DeviceName": "B315s-936",
	})

	ctx := context.Background()
	p.pollOnce(ctx)

	snap := p.Last()
	if snap.Caps.Signal {
		t.Error("Caps.Signal should be false after NotSupported")
	}
	if snap.Caps.Traffic {
		t.Error("Caps.Traffic should be false after NotSupported")
	}
	if snap.Caps.Cellular {
		t.Error("Caps.Cellular should be false after NotSupported")
	}
}

// TestDisconnected 验证 ConnectionStatus=902 → Online=false。
func TestDisconnected(t *testing.T) {
	mock := newMockCPE(t)
	p, srv := newTestPoll(t, mock)
	defer srv.Close()

	mock.setEndpoint("monitoring/status", "response", map[string]string{
		"ConnectionStatus": "902",
	})

	ctx := context.Background()
	p.pollOnce(ctx)
	snap := p.Last()
	if snap.Online {
		t.Errorf("expected offline (902), got online")
	}
}

// TestPollerStartStop 验证 Start 循环首次采集后可在 ctx 取消时退出。
func TestPollerStartStop(t *testing.T) {
	mock := newMockCPE(t)
	p, srv := newTestPoll(t, mock)
	defer srv.Close()

	mock.setEndpoint("monitoring/status", "response", map[string]string{
		"ConnectionStatus": "901",
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		p.Start(ctx)
		close(done)
	}()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if !p.Last().At.IsZero() {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if p.Last().At.IsZero() {
		t.Fatal("poller did not run first poll")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("poller did not stop after cancel")
	}
}