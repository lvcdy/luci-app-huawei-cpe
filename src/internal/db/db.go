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

// schema 是 v0.1 的表结构（信号历史 + 流量历史 + 短信）。
// events 表在 Phase 4 引入，避免无用空表。
//
// sms 表：本地短信库。去重键 UNIQUE(device_id, cpe_index)——
// cpe_index 是 SDK Message.Index（CPE 内部索引）；重复同步冲突即忽略。
// 短信正文仅存本地（架构 §安全：不入日志、不进错误链、默认不外发）。
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

CREATE TABLE IF NOT EXISTS sms (
    id INTEGER PRIMARY KEY AUTOINCREMENT,        -- 本地自增
    device_id TEXT NOT NULL,
    cpe_index INTEGER NOT NULL,                  -- SDK Message.Index → 去重
    phone TEXT, content TEXT,
    status INTEGER,                              -- SmsStatus 0=new 1=read 2=pending 3=sent 4=failed
    received_at INTEGER NOT NULL,                -- unix 秒
    read_local INTEGER DEFAULT 0                 -- 本地已读标记（双向同步用）
);
CREATE UNIQUE INDEX IF NOT EXISTS uq_sms_dev_idx ON sms(device_id, cpe_index);
CREATE INDEX IF NOT EXISTS idx_sms_dev ON sms(device_id, received_at);
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

// SmsStatus 是 sms.status 的可读映射（与 enums.SmsStatus 数值一致）。
type SmsStatus int

const (
	SmsStatusNew    SmsStatus = 0
	SmsStatusRead   SmsStatus = 1
	SmsStatusSent   SmsStatus = 3
	SmsStatusFailed SmsStatus = 4
)

// SmsInserter 封装"插入一条短信，重复则忽略"（daemon 与测试共用）。
// 返回 (inserted bool, err)：inserted=false 表示该 (device_id, cpe_index)
// 已存在（重复同步），不属于错误。
// 注意：content 由调用方保证绝不进入日志 / 错误链。
type SmsInserter struct{}

// Insert 将一条从 CPE 拉到的短信写入本地库。
// 步骤：
//  1. UPDATE 镜像刷新：CPE 侧读状态等变化同步到已存在的本地行（幂等）；
//  2. INSERT OR IGNORE 去重插入：唯一键 (device_id, cpe_index) 冲突时静默忽略。
// 返回 (inserted bool, err)：inserted=true 表示本次为新增行；
// 已存在（重复同步，仅镜像刷新）时返回 false —— 不属于错误。
func (SmsInserter) Insert(ctx context.Context, d *sql.DB, deviceID string, sms SmsRow) (bool, error) {
	// 镜像刷新：CPE 侧状态（如已读 status）可能已在手机上变化，同步到本地行。
	// 仅刷新 status（read_local 是本地标读权威，不受 CPE 覆盖）。
	if _, err := d.ExecContext(ctx,
		`UPDATE sms SET status = ? WHERE device_id = ? AND cpe_index = ?`,
		sms.Status, deviceID, sms.CpeIndex); err != nil {
		return false, fmt.Errorf("update sms status: %w", err)
	}

	res, err := d.ExecContext(ctx,
		`INSERT OR IGNORE INTO sms (device_id, cpe_index, phone, content, status, received_at) VALUES (?, ?, ?, ?, ?, ?)`,
		deviceID, sms.CpeIndex, sms.Phone, sms.Content, sms.Status, sms.ReceivedAt)
	if err != nil {
		return false, fmt.Errorf("insert sms: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("insert sms rows: %w", err)
	}
	return n > 0, nil
}

// SmsRow 是一条短信的本地存储形态（与 sms 表列一一对应）。
type SmsRow struct {
	CpeIndex   int    // SDK Message.Index
	Phone      string
	Content    string
	Status     int    // enums.SmsStatus 数值
	ReceivedAt int64  // unix 秒
}

// SmsMessage 是 API 返回的一条短信（本地 id + 完整字段）。
// LocalID 是本地自增主键（API 路由用）；Unread 由 status 与 read_local 综合。
type SmsMessage struct {
	ID         int64  `json:"id"`
	CpeIndex   int    `json:"cpe_index"`
	Phone      string `json:"phone"`
	Content    string `json:"content"`
	Status     int    `json:"status"`
	ReceivedAt int64  `json:"received_at"`
	ReadLocal  int    `json:"read_local"`
	Unread     bool   `json:"unread"`
}

// ListSms 返回设备的短信列表（按 received_at 倒序）。
// filter: "unread" 仅未读；其它值（含空）返回全部。
// unread 判定：status=0（CPE 侧未读）**且** read_local=0（本地未读）才未读；
// 任一侧为已读（CPE 手机读过 → status=1；LuCI 标读 → read_local=1）即已读。
// search 非空时按 phone/content LIKE 过滤（% 转义由调用方处理）。
func ListSms(ctx context.Context, d *sql.DB, deviceID, filter, search string) ([]SmsMessage, error) {
	q := `SELECT id, cpe_index, phone, content, status, received_at, read_local, unread
	      FROM (
	        SELECT *, (status = 0 AND read_local = 0) AS unread
	        FROM sms WHERE device_id = ?
	      ) WHERE 1=1`
	args := []any{deviceID}
	if filter == "unread" {
		q += ` AND unread = 1`
	}
	if search != "" {
		q += ` AND (phone LIKE ? OR content LIKE ?)`
		like := "%" + search + "%"
		args = append(args, like, like)
	}
	q += ` ORDER BY received_at DESC, id DESC`

	rows, err := d.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("list sms: %w", err)
	}
	defer rows.Close()

	out := []SmsMessage{}
	for rows.Next() {
		var m SmsMessage
		if err := rows.Scan(&m.ID, &m.CpeIndex, &m.Phone, &m.Content,
			&m.Status, &m.ReceivedAt, &m.ReadLocal, &m.Unread); err != nil {
			return nil, fmt.Errorf("scan sms: %w", err)
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// CountUnreadSms 返回设备未读短信数（与 ListSms 相同的未读口径）。
func CountUnreadSms(ctx context.Context, d *sql.DB, deviceID string) (int, error) {
	var n int
	err := d.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM sms WHERE device_id = ? AND status = 0 AND read_local = 0`,
		deviceID).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("count unread sms: %w", err)
	}
	return n, nil
}

// GetSms 按本地 id 取单条短信（含 device_id 归属校验）。
// 不存在返回 (zero, sql.ErrNoRows)。
func GetSms(ctx context.Context, d *sql.DB, deviceID string, localID int64) (SmsMessage, error) {
	var m SmsMessage
	err := d.QueryRowContext(ctx,
		`SELECT id, cpe_index, phone, content, status, received_at, read_local,
		        (status = 0 AND read_local = 0) AS unread
		 FROM sms WHERE id = ? AND device_id = ?`,
		localID, deviceID).
		Scan(&m.ID, &m.CpeIndex, &m.Phone, &m.Content,
			&m.Status, &m.ReceivedAt, &m.ReadLocal, &m.Unread)
	if err != nil {
		return m, err // sql.ErrNoRows 原样返回
	}
	return m, nil
}

// MarkSmsRead 置已读标记（幂等；不存在返回 (false, nil)）。
// 同时置 status=1 与 read_local=1：status 维持 CPE 侧镜像一致（CPE SetRead 成功后
// CPE 自身也置 status=1），read_local 为本地即时标记（不等下次同步）。
func MarkSmsRead(ctx context.Context, d *sql.DB, deviceID string, localID int64) (bool, error) {
	res, err := d.ExecContext(ctx,
		`UPDATE sms SET status = 1, read_local = 1 WHERE id = ? AND device_id = ?`, localID, deviceID)
	if err != nil {
		return false, fmt.Errorf("mark sms read: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("mark sms read rows: %w", err)
	}
	return n > 0, nil
}

// DeleteSmsLocal 删除本地短信行（幂等；不存在返回 (false, nil)）。
func DeleteSmsLocal(ctx context.Context, d *sql.DB, deviceID string, localID int64) (bool, error) {
	res, err := d.ExecContext(ctx,
		`DELETE FROM sms WHERE id = ? AND device_id = ?`, localID, deviceID)
	if err != nil {
		return false, fmt.Errorf("delete sms: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("delete sms rows: %w", err)
	}
	return n > 0, nil
}
