// Package db 打开 SQLite（纯 Go 驱动 modernc.org/sqlite，无 CGO，
// OpenWrt 交叉编译友好），负责建库迁移与历史数据保留期清理。
//
// 设计约束（架构 §3/§存储）：
//   - 单连接串行（SetMaxOpenConns(1)）：写入量小（每设备 60s 一行），
//     串行彻底避免 SQLITE_BUSY，可靠性优先；
//   - WAL + synchronous=NORMAL：降低 flash 写放大；
//   - 表结构与架构文档 §3.2 一致；秘密字段（密码/token/短信内容）不入本库。
package db

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
)

// schema 是 v0.1 的表结构（信号历史 + 流量历史）。
// sms/events 表在 Phase 3/4 引入，避免无用空表。
const schema = `
CREATE TABLE IF NOT EXISTS signal_history (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    device_id TEXT NOT NULL,
    ts INTEGER NOT NULL,
    rsrp INTEGER, rsrq INTEGER, sinr INTEGER, rssi INTEGER,
    lte_band TEXT, nr_band TEXT, pci INTEGER,
    cell_id INTEGER, earfcn INTEGER, nrarfcn INTEGER
);
CREATE INDEX IF NOT EXISTS idx_sig_dev_ts ON signal_history(device_id, ts);

CREATE TABLE IF NOT EXISTS traffic_history (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    device_id TEXT NOT NULL, ts INTEGER NOT NULL,
    rx_bytes INTEGER, tx_bytes INTEGER, rx_rate REAL, tx_rate REAL
);
CREATE INDEX IF NOT EXISTS idx_traff_dev_ts ON traffic_history(device_id, ts);
`

// Open 打开（或创建）数据库文件并完成迁移。
// 父目录不存在时自动创建。失败时返回错误（调用方决定是否降级运行）。
func Open(path string) (*sql.DB, error) {
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("create db dir %s: %w", dir, err)
		}
	}
	dsn := fmt.Sprintf(
		"file:%s?_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_pragma=busy_timeout(5000)",
		path)
	d, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite %s: %w", path, err)
	}
	d.SetMaxOpenConns(1) // 串行访问，避免锁竞争
	if _, err := d.Exec(schema); err != nil {
		d.Close()
		return nil, fmt.Errorf("migrate %s: %w", path, err)
	}
	return d, nil
}

// PruneHistory 删除早于保留期（天）的历史行。
// retentionDays <= 0 时不做任何操作。
func PruneHistory(ctx context.Context, d *sql.DB, retentionDays int) error {
	if retentionDays <= 0 {
		return nil
	}
	cutoff := time.Now().AddDate(0, 0, -retentionDays).Unix()
	for _, table := range []string{"signal_history", "traffic_history"} {
		if _, err := d.ExecContext(ctx, "DELETE FROM "+table+" WHERE ts < ?", cutoff); err != nil {
			return fmt.Errorf("prune %s: %w", table, err)
		}
	}
	return nil
}
