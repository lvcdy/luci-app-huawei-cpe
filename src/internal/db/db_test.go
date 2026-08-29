package db

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestOpenCreatesSchema 验证打开新库时建表与索引成功。
func TestOpenCreatesSchema(t *testing.T) {
	p := filepath.Join(t.TempDir(), "sub", "history.db")
	d, err := Open(p)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer d.Close()

	if _, err := os.Stat(p); err != nil {
		t.Fatalf("db file not created: %v", err)
	}
	// 确认表存在（插入一行验证可写）。
	if _, err := d.Exec(`INSERT INTO signal_history (device_id, ts) VALUES ('x', 1)`); err != nil {
		t.Fatalf("insert signal: %v", err)
	}
	if _, err := d.Exec(`INSERT INTO traffic_history (device_id, ts) VALUES ('x', 1)`); err != nil {
		t.Fatalf("insert traffic: %v", err)
	}
	if _, err := d.Exec(`INSERT INTO sms (device_id, cpe_index, phone, content, status, received_at) VALUES ('x', 1, '+1', 'hi', 0, 1)`); err != nil {
		t.Fatalf("insert sms: %v", err)
	}
}

// TestSmsUnique 验证 (device_id, cpe_index) 唯一约束：
// 重复同步同一条短信（相同 cpe_index）必须被拒绝，这是"重复同步不重复入库"的底层保证。
func TestSmsUnique(t *testing.T) {
	d, err := Open(filepath.Join(t.TempDir(), "h.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer d.Close()

	const ins = `INSERT INTO sms (device_id, cpe_index, phone, content, status, received_at) VALUES (?, ?, ?, ?, ?, ?)`
	if _, err := d.Exec(ins, "cpe1", 40, "+86138", "hello", 0, 1700000000); err != nil {
		t.Fatalf("first insert: %v", err)
	}
	// 相同设备相同 cpe_index → 违反唯一约束
	if _, err := d.Exec(ins, "cpe1", 40, "+86138", "hello again", 1, 1700000001); err == nil {
		t.Fatal("duplicate (device_id, cpe_index) insert must fail")
	}
	// 不同设备相同 cpe_index → 允许（不同 CPE 的索引空间相互独立）
	if _, err := d.Exec(ins, "cpe2", 40, "+86138", "hello", 0, 1700000000); err != nil {
		t.Fatalf("same cpe_index on other device must succeed: %v", err)
	}
	// 相同设备不同 cpe_index → 允许
	if _, err := d.Exec(ins, "cpe1", 41, "+86138", "second", 0, 1700000002); err != nil {
		t.Fatalf("distinct cpe_index must succeed: %v", err)
	}

	var n int
	if err := d.QueryRow("SELECT COUNT(*) FROM sms").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 3 {
		t.Errorf("want 3 rows, got %d", n)
	}
}

// TestOpenIdempotent 验证重复打开（迁移幂等）不报错。
func TestOpenIdempotent(t *testing.T) {
	p := filepath.Join(t.TempDir(), "h.db")
	d1, err := Open(p)
	if err != nil {
		t.Fatalf("first Open: %v", err)
	}
	d1.Close()

	d2, err := Open(p)
	if err != nil {
		t.Fatalf("second Open: %v", err)
	}
	d2.Close()
}

// TestPruneHistory 验证保留期清理只删过期行。
func TestPruneHistory(t *testing.T) {
	d, err := Open(filepath.Join(t.TempDir(), "h.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer d.Close()

	now := time.Now().Unix()
	old := now - 40*86400  // 40 天前（超出 30 天保留）
	fresh := now - 86400   // 1 天前
	for _, table := range []string{"signal_history", "traffic_history"} {
		for _, ts := range []int64{old, fresh} {
			if _, err := d.Exec("INSERT INTO "+table+" (device_id, ts) VALUES ('x', ?)", ts); err != nil {
				t.Fatalf("insert: %v", err)
			}
		}
	}

	if err := PruneHistory(context.Background(), d, 30); err != nil {
		t.Fatalf("Prune: %v", err)
	}

	for _, table := range []string{"signal_history", "traffic_history"} {
		var n int
		if err := d.QueryRow("SELECT COUNT(*) FROM " + table).Scan(&n); err != nil {
			t.Fatalf("count: %v", err)
		}
		if n != 1 {
			t.Errorf("%s: want 1 row after prune, got %d", table, n)
		}
	}
}

// TestPruneZeroRetention 验证保留天数<=0 时不清理。
func TestPruneZeroRetention(t *testing.T) {
	d, err := Open(filepath.Join(t.TempDir(), "h.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer d.Close()

	if _, err := d.Exec("INSERT INTO signal_history (device_id, ts) VALUES ('x', 1)"); err != nil {
		t.Fatal(err)
	}
	if err := PruneHistory(context.Background(), d, 0); err != nil {
		t.Fatalf("Prune(0): %v", err)
	}
	var n int
	if err := d.QueryRow("SELECT COUNT(*) FROM signal_history").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("row removed by zero-retention prune")
	}
}
