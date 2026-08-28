package history

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"huawei-cpe/internal/db"
	"huawei-cpe/internal/poller"
)

// TestRecorderWritesOnlineSnapshots 验证在线快照写入两张表，离线快照跳过。
func TestRecorderWritesOnlineSnapshots(t *testing.T) {
	sqldb, err := db.Open(t.TempDir() + "/h.db")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer sqldb.Close()

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	r := NewRecorder(log, sqldb)

	online := poller.Snapshot{
		At:     time.Now(),
		Online: true,
		Signal: poller.SignalState{RSRP: -75, RSRQ: -16, SINR: 12, Band: "B3", Mode: "4G"},
		Traffic: poller.TrafficState{
			TotalRxBytes: 1000, TotalTxBytes: 500, RxRate: 10, TxRate: 5,
		},
		Caps: poller.Capabilities{Signal: true, Traffic: true},
	}
	r.PutSnapshot("main", online)

	var sigN, trafN int
	sqldb.QueryRow("SELECT COUNT(*) FROM signal_history").Scan(&sigN)
	sqldb.QueryRow("SELECT COUNT(*) FROM traffic_history").Scan(&trafN)
	if sigN != 1 || trafN != 1 {
		t.Fatalf("want 1 signal + 1 traffic row, got %d/%d", sigN, trafN)
	}

	// band 按 mode 分类到 lte_band
	var lteBand string
	sqldb.QueryRow("SELECT lte_band FROM signal_history").Scan(&lteBand)
	if lteBand != "B3" {
		t.Errorf("lte_band = %q, want B3", lteBand)
	}

	// 离线快照不写
	offline := online
	offline.Online = false
	r.PutSnapshot("main", offline)
	sqldb.QueryRow("SELECT COUNT(*) FROM signal_history").Scan(&sigN)
	if sigN != 1 {
		t.Errorf("offline snapshot written: %d rows", sigN)
	}
}

// TestRecorderNilDB 验证 db=nil 时不 panic。
func TestRecorderNilDB(t *testing.T) {
	r := NewRecorder(nil, nil)
	r.PutSnapshot("main", poller.Snapshot{Online: true}) // 不应 panic
}

// TestSignalSeriesBuckets 验证聚合查询的三种桶。
func TestSignalSeriesBuckets(t *testing.T) {
	sqldb, err := db.Open(t.TempDir() + "/h.db")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer sqldb.Close()

	now := time.Now().Unix()
	// 插入：30 分钟前（h1/d7/d30 都覆盖）、5 天前（仅 d7/d30）、40 天前（都不覆盖）
	insert := func(ts int64, rsrp int) {
		if _, err := sqldb.Exec(
			"INSERT INTO signal_history (device_id, ts, rsrp, rsrq, sinr, rssi) VALUES ('main', ?, ?, ?, ?, ?)",
			ts, rsrp, -10, 5, -60); err != nil {
			t.Fatalf("insert: %v", err)
		}
	}
	insert(now-1800, -70)
	insert(now-5*86400, -80)
	insert(now-40*86400, -90)

	ctx := context.Background()
	for _, tc := range []struct {
		bucket string
		want   int
	}{
		{"h1", 1},
		{"d7", 2},
		{"d30", 2},
		{"bogus", -1},
	} {
		pts, err := SignalSeries(ctx, sqldb, "main", tc.bucket)
		if tc.want < 0 {
			if err == nil {
				t.Errorf("bucket %s: expected error", tc.bucket)
			}
			continue
		}
		if err != nil {
			t.Fatalf("bucket %s: %v", tc.bucket, err)
		}
		if len(pts) != tc.want {
			t.Errorf("bucket %s: want %d points, got %d", tc.bucket, tc.want, len(pts))
		}
	}
}

// TestTrafficSeries 验证流量趋势查询。
func TestTrafficSeries(t *testing.T) {
	sqldb, err := db.Open(t.TempDir() + "/h.db")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer sqldb.Close()

	now := time.Now().Unix()
	for i := 0; i < 3; i++ {
		if _, err := sqldb.Exec(
			"INSERT INTO traffic_history (device_id, ts, rx_bytes, tx_bytes, rx_rate, tx_rate) VALUES ('main', ?, 0, 0, ?, ?)",
			now-int64(i)*60, 100.0, 50.0); err != nil {
			t.Fatal(err)
		}
	}

	pts, err := TrafficSeries(context.Background(), sqldb, "main", "d1")
	if err != nil {
		t.Fatalf("TrafficSeries: %v", err)
	}
	if len(pts) != 3 {
		t.Fatalf("want 3 points, got %d", len(pts))
	}
	// 按时间升序
	for i := 1; i < len(pts); i++ {
		if pts[i].Ts <= pts[i-1].Ts {
			t.Errorf("points not ascending at %d", i)
		}
	}
	if _, err := TrafficSeries(context.Background(), sqldb, "main", "nope"); err == nil {
		t.Error("expected error for bad bucket")
	}
}

// TestDeviceIsolation 验证历史按 device_id 隔离。
func TestDeviceIsolation(t *testing.T) {
	sqldb, err := db.Open(t.TempDir() + "/h.db")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer sqldb.Close()

	now := time.Now().Unix()
	for _, id := range []string{"a", "b"} {
		sqldb.Exec("INSERT INTO signal_history (device_id, ts, rsrp) VALUES (?, ?, -70)", id, now-60)
	}
	pts, err := SignalSeries(context.Background(), sqldb, "a", "h1")
	if err != nil {
		t.Fatal(err)
	}
	if len(pts) != 1 {
		t.Errorf("device isolation broken: got %d points", len(pts))
	}
}
