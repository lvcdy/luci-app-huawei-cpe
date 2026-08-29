package device

import (
	"context"
	"encoding/xml"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"huawei-cpe/internal/config"
)

// newTestDeviceWithMock 构造连向 mock 的 device。
func newTestDeviceWithMock(t *testing.T, username string) (*Device, *mockCPE) {
	t.Helper()
	m := newMockCPE(t, username)
	srv := httptest.NewServer(m.Handler())
	t.Cleanup(srv.Close)
	host := strings.TrimPrefix(srv.URL, "http://")
	d := New(testLogger{t}, config.CPE{
		ID:       "r1",
		Host:     host,
		Username: username,
		Password: "secret",
	})
	return d, m
}

// ---- 功能 5：锁频 ----

func TestSetCellLockRequests(t *testing.T) {
	d, m := newTestDeviceWithMock(t, "admin")
	// 记录收到的 Lock/Freq/PCI 请求体
	var got []struct {
		Lock int `xml:"LockCell"`
		Freq int `xml:"Freq"`
		PCI  int `xml:"PCI"`
	}
	m.SetEndpointHandler("net/lock-cell", func(r *http.Request) string {
		var req struct {
			Lock int `xml:"LockCell"`
			Freq int `xml:"Freq"`
			PCI  int `xml:"PCI"`
		}
		_ = xml.NewDecoder(r.Body).Decode(&req)
		got = append(got, req)
		return `<response>OK</response>`
	})

	ctx := context.Background()
	// 按频率锁定
	if err := d.SetCellLock(ctx, CellLockReq{Lock: 1, Freq: 1825, PCI: 0}); err != nil {
		t.Fatalf("lock by freq: %v", err)
	}
	// 按小区锁定
	if err := d.SetCellLock(ctx, CellLockReq{Lock: 2, Freq: 1825, PCI: 301}); err != nil {
		t.Fatalf("lock by cell: %v", err)
	}
	// 解锁
	if err := d.SetCellLock(ctx, CellLockReq{Lock: 0, Freq: 0, PCI: 0}); err != nil {
		t.Fatalf("unlock: %v", err)
	}

	if len(got) != 3 {
		t.Fatalf("requests = %d, want 3 (%+v)", len(got), got)
	}
	if got[0] != (struct {
		Lock int `xml:"LockCell"`
		Freq int `xml:"Freq"`
		PCI  int `xml:"PCI"`
	}{1, 1825, 0}) {
		t.Errorf("req[0] = %+v, want {1 1825 0}", got[0])
	}
	if got[1] != (struct {
		Lock int `xml:"LockCell"`
		Freq int `xml:"Freq"`
		PCI  int `xml:"PCI"`
	}{2, 1825, 301}) {
		t.Errorf("req[1] = %+v, want {2 1825 301}", got[1])
	}
	if got[2].Lock != 0 {
		t.Errorf("req[2] = %+v, want unlock {0 0 0}", got[2])
	}
}

func TestSetCellLockValidation(t *testing.T) {
	d, m := newTestDeviceWithMock(t, "admin")
	// unlock 分支会真实发起请求，挂默认 OK 响应
	m.SetEndpointHandler("net/lock-cell", func(r *http.Request) string {
		return `<response>OK</response>`
	})
	ctx := context.Background()
	if err := d.SetCellLock(ctx, CellLockReq{Lock: 3}); err == nil {
		t.Error("lock=3 should fail")
	}
	if err := d.SetCellLock(ctx, CellLockReq{Lock: 1, Freq: 0}); err == nil {
		t.Error("lock without freq should fail")
	}
	if err := d.SetCellLock(ctx, CellLockReq{Lock: 0, Freq: 0}); err != nil {
		t.Errorf("unlock should succeed: %v", err)
	}
}

// ---- 功能 5b：网络模式 ----

func TestSetNetModeRequests(t *testing.T) {
	d, m := newTestDeviceWithMock(t, "admin")
	var got []struct {
		NetworkMode string `xml:"NetworkMode"`
		NetworkBand string `xml:"NetworkBand"`
		LTEBand     string `xml:"LTEBand"`
	}
	m.SetEndpointHandler("net/net-mode", func(r *http.Request) string {
		var req struct {
			NetworkMode string `xml:"NetworkMode"`
			NetworkBand string `xml:"NetworkBand"`
			LTEBand     string `xml:"LTEBand"`
		}
		_ = xml.NewDecoder(r.Body).Decode(&req)
		got = append(got, req)
		return `<response>OK</response>`
	})

	ctx := context.Background()
	// 4G-only（networkmode="03"）—— lte/network band nil → 空元素（不变维度）
	if err := d.SetNetMode(ctx, NetModeReq{LTEBand: nil, NetworkBand: nil, NetworkMode: "03"}); err != nil {
		t.Fatalf("set net mode: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("requests = %d, want 1", len(got))
	}
	if got[0].NetworkMode != "03" {
		t.Errorf("NetworkMode = %q, want 03", got[0].NetworkMode)
	}
	// band int → 小写 hex（enums.NetworkBandAll=0x3FFFFFFF → "3fffffff"）
	if err := d.SetNetMode(ctx, NetModeReq{LTEBand: 0x7FFFFFFFFFFFFFFF, NetworkBand: 0x3FFFFFFF, NetworkMode: nil}); err != nil {
		t.Fatalf("set bands: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("requests = %d, want 2", len(got))
	}
	if got[1].LTEBand != "7fffffffffffffff" || got[1].NetworkBand != "3fffffff" {
		t.Errorf("bands = lte:%q net:%q, want 7fffffffffffffff/3fffffff",
			got[1].LTEBand, got[1].NetworkBand)
	}
}

// ---- 功能 6：流量开关 ----

func TestSetDataSwitchRequests(t *testing.T) {
	d, m := newTestDeviceWithMock(t, "admin")
	var got []struct {
		DataSwitch int `xml:"dataswitch"`
	}
	m.SetEndpointHandler("dialup/mobile-dataswitch", func(r *http.Request) string {
		var req struct {
			DataSwitch int `xml:"dataswitch"`
		}
		_ = xml.NewDecoder(r.Body).Decode(&req)
		got = append(got, req)
		return `<response>OK</response>`
	})

	ctx := context.Background()
	if err := d.SetDataSwitch(ctx, true); err != nil {
		t.Fatalf("on: %v", err)
	}
	if err := d.SetDataSwitch(ctx, false); err != nil {
		t.Fatalf("off: %v", err)
	}
	if len(got) != 2 || got[0].DataSwitch != 1 || got[1].DataSwitch != 0 {
		t.Fatalf("dataswitch requests = %+v, want [1 0]", got)
	}
}

// ---- 功能 7：重启 ----

func TestRebootRequests(t *testing.T) {
	d, m := newTestDeviceWithMock(t, "admin")
	var got []struct {
		Control int `xml:"Control"`
	}
	m.SetEndpointHandler("device/control", func(r *http.Request) string {
		var req struct {
			Control int `xml:"Control"`
		}
		_ = xml.NewDecoder(r.Body).Decode(&req)
		got = append(got, req)
		return `<response>OK</response>`
	})

	ctx := context.Background()
	if err := d.Reboot(ctx); err != nil {
		t.Fatalf("reboot: %v", err)
	}
	if len(got) != 1 || got[0].Control != 1 {
		t.Fatalf("control requests = %+v, want [1]", got)
	}
}
