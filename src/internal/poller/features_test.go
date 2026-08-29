package poller

import (
	"context"
	"testing"
	"time"
)

// ---- 功能 1：服务小区详情（net/cell-info）----

func TestPollCellInfo(t *testing.T) {
	mock := newMockCPE(t)
	p, srv := newTestPoll(t, mock)
	defer srv.Close()

	mock.setEndpoint("monitoring/status", "response", map[string]string{
		"ConnectionStatus": "901",
	})
	mock.setEndpoint("net/cell-info", "response", map[string]string{
		"earfcn": "1825", "nrarfcn": "156000",
		"bandwidth": "20", "cqi": "12", "pci": "301", "cell_id": "12345678",
	})

	p.pollOnce(context.Background())
	snap := p.Last()

	c := snap.Cell
	if c.ARFCN != "1825" || c.EARFCN != "1825" || c.NRARFCN != "156000" {
		t.Errorf("ARFCN/EARFCN/NRARFCN = %q/%q/%q", c.ARFCN, c.EARFCN, c.NRARFCN)
	}
	if c.Bandwidth != 20 || c.CQI != 12 || c.PCI != 301 || c.CellID != 12345678 {
		t.Errorf("BW/CQI/PCI/CellID = %d/%d/%d/%d", c.Bandwidth, c.CQI, c.PCI, c.CellID)
	}
	// PCI 回填到 Signal
	if snap.Signal.PCI != 301 {
		t.Errorf("Signal.PCI = %d, want 301 (backfill)", snap.Signal.PCI)
	}
	// 功能 2：cell-info 携带 qci 时回填 QoS
	if snap.QoS.QCI != 0 {
		t.Errorf("QoS.QCI = %d, want 0 (no qci in fixture)", snap.QoS.QCI)
	}
}

// 功能 2：AMBR / QCI / 速度限制（monitoring/status + net/network）
func TestPollQoS(t *testing.T) {
	mock := newMockCPE(t)
	p, srv := newTestPoll(t, mock)
	defer srv.Close()

	mock.setEndpoint("monitoring/status", "response", map[string]string{
		"ConnectionStatus": "901",
		"mQos":             "7",
		"DownlinkAmbr":     "100000000",
		"UplinkAmbr":       "50000000",
		"MaxDownlinkSpeed": "300000000",
	})
	mock.setEndpoint("net/network", "response", map[string]string{
		"MaxUplinkSpeed": "100000000",
	})

	p.pollOnce(context.Background())
	snap := p.Last()

	q := snap.QoS
	if q.QCI != 7 {
		t.Errorf("QCI = %d, want 7", q.QCI)
	}
	if q.DlAmbr != 100000000 || q.UlAmbr != 50000000 {
		t.Errorf("DL/UL AMBR = %d/%d", q.DlAmbr, q.UlAmbr)
	}
	if q.MaxDlSpeed != 300000000 || q.MaxUlSpeed != 100000000 {
		t.Errorf("Max DL/UL = %d/%d", q.MaxDlSpeed, q.MaxUlSpeed)
	}
	if !q.SpeedLimit {
		t.Error("SpeedLimit should be true when max speeds > 0")
	}
}

// ---- 功能 3：载波聚合辅小区 + 邻小区 ----

func TestPollSecAndNbrCell(t *testing.T) {
	mock := newMockCPE(t)
	p, srv := newTestPoll(t, mock)
	defer srv.Close()

	mock.setEndpoint("monitoring/status", "response", map[string]string{
		"ConnectionStatus": "901",
	})
	// seccellinfo：5G 列表优先（CSV 字符串，9 列带 CellID）
	mock.SetEndpointRaw("device/seccellinfo",
		`<?xml version="1.0" encoding="UTF-8"?><response><nrseccell_list>A1,n78,100,302,-80,-9,-70,18,87654321;A2,n78,20,303,-85,-10,-75,15,87654322</nrseccell_list></response>`)
	// nbrcellinfo：邻区列表（7 列）
	mock.SetEndpointRaw("device/nbrcellinfo",
		`<?xml version="1.0" encoding="UTF-8"?><response><nbrcell_nrlist>A3,n78,304,-90,-11,-80,12;A4,n28,305,-95,-12,-85,10</nbrcell_nrlist></response>`)

	p.pollOnce(context.Background())
	snap := p.Last()

	if len(snap.Carrier) != 2 {
		t.Fatalf("Carrier len = %d, want 2 (%+v)", len(snap.Carrier), snap.Carrier)
	}
	if snap.Carrier[0].Band != "n78" || snap.Carrier[0].BW != 100 ||
		snap.Carrier[0].PCI != 302 || snap.Carrier[0].RSRP != -80 {
		t.Errorf("Carrier[0] = %+v", snap.Carrier[0])
	}
	if snap.Carrier[0].CellID != 87654321 {
		t.Errorf("Carrier[0].CellID = %d, want 87654321", snap.Carrier[0].CellID)
	}

	if len(snap.Neighbor) != 2 {
		t.Fatalf("Neighbor len = %d, want 2 (%+v)", len(snap.Neighbor), snap.Neighbor)
	}
	if snap.Neighbor[1].ARFCN != "A4" || snap.Neighbor[1].PCI != 305 {
		t.Errorf("Neighbor[1] = %+v", snap.Neighbor[1])
	}
}

// ---- 功能 5：锁频状态读取 ----

func TestPollCellLock(t *testing.T) {
	mock := newMockCPE(t)
	p, srv := newTestPoll(t, mock)
	defer srv.Close()

	mock.setEndpoint("monitoring/status", "response", map[string]string{
		"ConnectionStatus": "901",
	})
	mock.setEndpoint("ntwk/celllock", "response", map[string]string{
		"Lock": "1", "Freq": "1825", "PCI": "301", "MaxFreq": "182500",
	})

	p.pollOnce(context.Background())
	snap := p.Last()

	if snap.Lock.Lock != 1 || snap.Lock.Freq != "1825" ||
		snap.Lock.PCI != 301 || snap.Lock.MaxFreq != "182500" {
		t.Errorf("Lock = %+v", snap.Lock)
	}
}

// ---- 功能 6：流量开关状态 ----

func TestPollDataSwitch(t *testing.T) {
	mock := newMockCPE(t)
	p, srv := newTestPoll(t, mock)
	defer srv.Close()

	mock.setEndpoint("monitoring/status", "response", map[string]string{
		"ConnectionStatus": "901",
	})
	mock.setEndpoint("dialup/mobile-dataswitch", "response", map[string]string{
		"dataswitch": "1",
	})

	p.pollOnce(context.Background())
	snap := p.Last()
	if snap.Data.DataSwitch != 1 {
		t.Errorf("DataSwitch = %d, want 1", snap.Data.DataSwitch)
	}
}

// ---- 功能 4：系统操作日志 ----

func TestPollLogInfo(t *testing.T) {
	mock := newMockCPE(t)
	p, srv := newTestPoll(t, mock)
	defer srv.Close()

	mock.setEndpoint("monitoring/status", "response", map[string]string{
		"ConnectionStatus": "901",
	})
	// loginfo：2 条日志（第一条带 BASE64 前缀）
	mock.SetEndpointRaw("log/loginfo",
		`<?xml version="1.0" encoding="UTF-8"?><response>`+
			`<loginfo><logtype>Syslog</logtype><loglevel>INFO</loglevel>`+
			`<logtime>2026-08-29 12:00:00</logtime>`+
			`<logcontent>BASE64:Q1BFIHJlYm9vdGVk</logcontent></loginfo>`+
			`<loginfo><logtype>Alarm</logtype><loglevel>WARN</loglevel>`+
			`<logtime>2026-08-29 12:01:00</logtime>`+
			`<logcontent>Weak signal</logcontent></loginfo>`+
			`</response>`)

	p.pollOnce(context.Background())
	snap := p.Last()

	if len(snap.Log) != 2 {
		t.Fatalf("Log len = %d, want 2 (%+v)", len(snap.Log), snap.Log)
	}
	// 倒序：最新（Weak signal）在前
	if snap.Log[0].Info != "Weak signal" || snap.Log[1].Info != "CPE rebooted" {
		t.Errorf("Log = %+v", snap.Log)
	}
}

// ---- 功能 10：暂停/恢复轮询 ----

// TestPollSuspendNoRequests 验证暂停期间不产生任何 /api 请求（保留最近快照）。
func TestPollSuspendNoRequests(t *testing.T) {
	mock := newMockCPE(t)
	p, srv := newTestPoll(t, mock)
	defer srv.Close()

	mock.setEndpoint("monitoring/status", "response", map[string]string{
		"ConnectionStatus": "901",
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { p.Start(ctx); close(done) }()

	// 等待首次采集完成
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

	p.Suspend()
	if !p.IsSuspended() {
		t.Fatal("expected IsSuspended() == true after Suspend")
	}
	before := mock.APIRequests()

	// 等一个周期以上，确认暂停期间无新增请求
	time.Sleep(120 * time.Millisecond)
	if got := mock.APIRequests() - before; got != 0 {
		t.Fatalf("requests during suspend = %d, want 0", got)
	}

	// Resume 恢复
	p.Resume()
	if p.IsSuspended() {
		t.Fatal("expected IsSuspended() == false after Resume")
	}
	// 恢复后立即触发一次采集（新请求出现）
	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if mock.APIRequests() > before {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if mock.APIRequests() == before {
		t.Fatal("no poll after Resume")
	}

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("poller did not stop after cancel")
	}
}
