package app

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"huawei-cpe/internal/config"
	"huawei-cpe/internal/testutil"
)

// TestEndToEndDevicesAPI 是 P1.9 验收：完整启动 daemon（含 poller/cache/API），
// 对真实监听的 127.0.0.1:9090 发起 HTTP 请求，验证
// `curl 127.0.0.1:9090/api/v1/devices` 返回脱敏 JSON（等价于 curl 冒烟）。
func TestEndToEndDevicesAPI(t *testing.T) {
	// mock CPE（模拟真机握手 + 轮询端点）
	mock := testutil.NewMockCPE("admin")
	mock.SetEndpoint("monitoring/status", "response", map[string]string{
		"ConnectionStatus":   "901",
		"CurrentNetworkType": "LTE",
	})
	mock.SetEndpoint("device/information", "response", map[string]string{
		"DeviceName": "B315s-936",
	})
	srv := httptest.NewServer(mock)
	defer srv.Close()
	host := strings.TrimPrefix(srv.URL, "http://")

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	cfg := &config.Config{
		CPEs: []config.CPE{
			{ID: "main", Name: "Main", Host: host, Username: "admin",
				Password: "topsecret", Enabled: true, PollingInterval: 1},
		},
	}

	st := New(log, cfg)
	runErr := make(chan error, 1)
	go func() { runErr <- st.Run(nil) }()
	defer st.Quit()

	// 等待 API 就绪 + 首轮轮询落缓存（最多 10s）
	var devs map[string]any
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get("http://127.0.0.1:9090/api/v1/devices")
		if err != nil {
			time.Sleep(100 * time.Millisecond)
			continue
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			time.Sleep(100 * time.Millisecond)
			continue
		}
		var m map[string]any
		if err := json.Unmarshal(body, &m); err != nil {
			t.Fatalf("bad json: %v (%s)", err, body)
		}
		list, _ := m["devices"].([]any)
		if len(list) == 0 {
			time.Sleep(100 * time.Millisecond)
			continue
		}
		d := list[0].(map[string]any)
		if d["online"] == true {
			devs = m
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if devs == nil {
		t.Fatal("daemon did not come online within 10s")
	}

	// 脱敏校验：响应绝不包含凭据
	raw, _ := json.Marshal(devs)
	if strings.Contains(string(raw), "topsecret") {
		t.Fatal("API response leaks password")
	}

	// /devices/{id} 完整快照
	resp, err := http.Get("http://127.0.0.1:9090/api/v1/devices/main")
	if err != nil {
		t.Fatalf("get device: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("device status = %d (%s)", resp.StatusCode, body)
	}
	var one map[string]any
	if err := json.Unmarshal(body, &one); err != nil {
		t.Fatalf("bad device json: %v (%s)", err, body)
	}
	if one["online"] != true {
		t.Fatalf("online = %v, body=%s", one["online"], body)
	}
	if strings.Contains(string(body), "topsecret") {
		t.Fatal("device response leaks password")
	}

	st.Quit()
	select {
	case err := <-runErr:
		if err != nil {
			t.Fatalf("Run returned error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not exit after Quit")
	}
}
