// Package history 将轮询快照写入 SQLite 历史表，并提供趋势聚合查询。
//
// 职责边界：
//   - 只写在线设备的有效采集（离线/能力缺失不写，避免脏点）；
//   - 写入失败仅记日志，绝不阻塞或影响轮询主循环（可靠性优先）；
//   - 聚合在服务端完成（GROUP BY 时间桶），前端只收精简点集。
package history

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"huawei-cpe/internal/poller"
)

// Logger 是 history 依赖的最小日志接口。
type Logger interface {
	Debug(msg string, args ...any)
	Warn(msg string, args ...any)
}

// Recorder 实现 poller.Sink：把每次在线快照追加到历史表。
// db 为 nil 时全部操作为 no-op（历史功能降级关闭）。
type Recorder struct {
	log Logger
	db  *sql.DB
}

// NewRecorder 构造写入器（db 可为 nil = 禁用）。
func NewRecorder(log Logger, db *sql.DB) *Recorder {
	return &Recorder{log: log, db: db}
}

// PutSnapshot 写入一条快照（仅在线且对应能力可用时）。
// 单连接串行执行；错误只降级记日志，不返回（Sink 语义）。
func (r *Recorder) PutSnapshot(id string, snap poller.Snapshot) {
	if r.db == nil || !snap.Online {
		return
	}
	ts := snap.At.Unix()

	if snap.Caps.Signal {
		lteBand, nrBand := "", ""
		if m := strings.ToUpper(snap.Signal.Mode); strings.Contains(m, "5G") || strings.Contains(m, "NR") {
			nrBand = snap.Signal.Band
		} else {
			lteBand = snap.Signal.Band
		}
		// 功能 1：小区详情持久化（cell-info 优先，回落到 signal 端点的并集）
		earfcn := snap.Cell.EARFCN
		if earfcn == "" {
			earfcn = snap.Signal.EARFCN
		}
		nrarfcn := snap.Cell.NRARFCN
		if nrarfcn == "" {
			nrarfcn = snap.Signal.NRARFCN
		}
		cellID := snap.Cell.CellID
		if cellID == 0 {
			cellID = snap.Signal.CellID
		}
		earfcnI, nrarfcnI := atoi64Safe(earfcn), atoi64Safe(nrarfcn)
		if _, err := r.db.Exec(
			`INSERT INTO signal_history
				(device_id, ts, rsrp, rsrq, sinr, rssi, lte_band, nr_band, pci, cell_id, earfcn, nrarfcn)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			id, ts, snap.Signal.RSRP, snap.Signal.RSRQ, snap.Signal.SINR,
			snap.Signal.RSSI, lteBand, nrBand, snap.Signal.PCI,
			cellID, earfcnI, nrarfcnI,
		); err != nil && r.log != nil {
			r.log.Warn("history: write signal", "dev", id, "err", err)
		}
	}

	if snap.Caps.Traffic {
		if _, err := r.db.Exec(
			`INSERT INTO traffic_history
				(device_id, ts, rx_bytes, tx_bytes, rx_rate, tx_rate)
			 VALUES (?, ?, ?, ?, ?, ?)`,
			id, ts, snap.Traffic.TotalRxBytes, snap.Traffic.TotalTxBytes,
			snap.Traffic.RxRate, snap.Traffic.TxRate,
		); err != nil && r.log != nil {
			r.log.Warn("history: write traffic", "dev", id, "err", err)
		}
	}
}

// bucket 描述一个时间桶：窗口长度（秒）与聚合粒度（秒；0 = 不聚合，返回原始点）。
type bucket struct {
	windowSec int64
	groupSec  int64
}

var signalBuckets = map[string]bucket{
	"h1":  {3600, 0},           // 最近 1 小时，原始点
	"d7":  {7 * 86400, 3600},   // 最近 7 天，每小时均值
	"d30": {30 * 86400, 86400}, // 最近 30 天，每天均值
}

var trafficBuckets = map[string]bucket{
	"d1":  {86400, 0},
	"d7":  {7 * 86400, 3600},
	"d30": {30 * 86400, 86400},
}

// atoi64Safe 容忍解析字符串为 int64；脏值返回 0（不 panic）。
// 用于把 ARFCN 字符串列（earfcn/nrarfcn）转换为 INTEGER 列写入。
func atoi64Safe(s string) int64 {
	var out int64
	for _, c := range strings.TrimSpace(s) {
		if c < '0' || c > '9' {
			return 0
		}
		out = out*10 + int64(c-'0')
	}
	return out
}

// SignalPoint 是信号趋势的一个聚合点。
type SignalPoint struct {
	Ts   int64   `json:"ts"`
	RSRP float64 `json:"rsrp"`
	RSRQ float64 `json:"rsrq"`
	SINR float64 `json:"sinr"`
	RSSI float64 `json:"rssi"`
}

// TrafficPoint 是流量趋势的一个聚合点（速率为桶内均值，bytes/s）。
type TrafficPoint struct {
	Ts     int64   `json:"ts"`
	RxRate float64 `json:"rx_rate"`
	TxRate float64 `json:"tx_rate"`
}

// SignalSeries 返回设备信号趋势（bucket: h1|d7|d30）。
func SignalSeries(ctx context.Context, d *sql.DB, deviceID, b string) ([]SignalPoint, error) {
	bk, ok := signalBuckets[b]
	if !ok {
		return nil, fmt.Errorf("invalid bucket %q", b)
	}
	cutoff := nowUnix() - bk.windowSec

	if bk.groupSec == 0 {
		return rawSignal(ctx, d, deviceID, cutoff)
	}
	rows, err := d.QueryContext(ctx, `
		SELECT (ts/?)*? AS t,
			COALESCE(AVG(rsrp), 0), COALESCE(AVG(rsrq), 0),
			COALESCE(AVG(sinr), 0), COALESCE(AVG(rssi), 0)
		FROM signal_history
		WHERE device_id = ? AND ts >= ?
		GROUP BY t ORDER BY t ASC`,
		bk.groupSec, bk.groupSec, deviceID, cutoff)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []SignalPoint{}
	for rows.Next() {
		var p SignalPoint
		if err := rows.Scan(&p.Ts, &p.RSRP, &p.RSRQ, &p.SINR, &p.RSSI); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// rawSignal 返回原始点（不聚合，上限 7200 点防内存放大）。
func rawSignal(ctx context.Context, d *sql.DB, deviceID string, cutoff int64) ([]SignalPoint, error) {
	rows, err := d.QueryContext(ctx, `
		SELECT ts, COALESCE(rsrp, 0), COALESCE(rsrq, 0),
			COALESCE(sinr, 0), COALESCE(rssi, 0)
		FROM signal_history
		WHERE device_id = ? AND ts >= ?
		ORDER BY ts ASC LIMIT 7200`, deviceID, cutoff)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []SignalPoint{}
	for rows.Next() {
		var p SignalPoint
		if err := rows.Scan(&p.Ts, &p.RSRP, &p.RSRQ, &p.SINR, &p.RSSI); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// TrafficSeries 返回设备流量速率趋势（bucket: d1|d7|d30）。
func TrafficSeries(ctx context.Context, d *sql.DB, deviceID, b string) ([]TrafficPoint, error) {
	bk, ok := trafficBuckets[b]
	if !ok {
		return nil, fmt.Errorf("invalid bucket %q", b)
	}
	cutoff := nowUnix() - bk.windowSec

	if bk.groupSec == 0 {
		return rawTraffic(ctx, d, deviceID, cutoff)
	}
	rows, err := d.QueryContext(ctx, `
		SELECT (ts/?)*? AS t, COALESCE(AVG(rx_rate), 0), COALESCE(AVG(tx_rate), 0)
		FROM traffic_history
		WHERE device_id = ? AND ts >= ?
		GROUP BY t ORDER BY t ASC`,
		bk.groupSec, bk.groupSec, deviceID, cutoff)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []TrafficPoint{}
	for rows.Next() {
		var p TrafficPoint
		if err := rows.Scan(&p.Ts, &p.RxRate, &p.TxRate); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func rawTraffic(ctx context.Context, d *sql.DB, deviceID string, cutoff int64) ([]TrafficPoint, error) {
	rows, err := d.QueryContext(ctx, `
		SELECT ts, COALESCE(rx_rate, 0), COALESCE(tx_rate, 0)
		FROM traffic_history
		WHERE device_id = ? AND ts >= ?
		ORDER BY ts ASC LIMIT 7200`, deviceID, cutoff)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []TrafficPoint{}
	for rows.Next() {
		var p TrafficPoint
		if err := rows.Scan(&p.Ts, &p.RxRate, &p.TxRate); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// nowUnix 当前 unix 秒。
func nowUnix() int64 { return time.Now().Unix() }
