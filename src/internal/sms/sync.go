// Package sms 实现短信同步循环：以固定间隔从 CPE 拉取短信并增量入库
//（本地 SQLite，去重键 device_id+cpe_index）。新短信不主动外发——
// 仅日志记录数量，正文绝不进入日志 / 错误链（架构 §安全）。
//
// 职责边界：
//   - 只依赖 device.Lease 借出的 SDK Client，永不直接创建 SDK 连接；
//   - 单设备单 goroutine，串行请求，不与其他循环并发打 CPE（架构 §6）；
//   - 不支持（NotSupported）或永久凭据错误 → 禁用该设备短信同步；
//   - 历史 SQLite 未打开（sqldb == nil）时上层不创建本循环。
package sms

import (
	"context"
	"database/sql"
	"sync/atomic"
	"time"

	"github.com/lvcdy/huawei-lte-api-go/enums"

	"huawei-cpe/internal/device"
	"huawei-cpe/internal/db"
)

// Logger 是 sms 依赖的最小日志接口（与 device/poller.Logger 对齐）。
type Logger interface {
	Debug(msg string, args ...any)
	Info(msg string, args ...any)
	Warn(msg string, args ...any)
	Error(msg string, args ...any)
}

// defaultSyncInterval 是未配置同步间隔时的缺省值（秒）。
const defaultSyncInterval = 30 * time.Second

// Syncer 是单设备的短信同步器。
//
// 并发安全：disabled 用原子布尔（仅同步器自身与 app.Stop 交互）。
// 同步循环使用 device.Lease 的独占租约，与 poller 互斥访问 SDK。
type Syncer struct {
	log      Logger
	dev      *device.Device
	db       *sql.DB
	interval time.Duration
	stop     chan struct{}
	done     chan struct{}
	disabled atomic.Bool // 能力禁用（NotSupported / 永久凭据错误）后不再请求
}

// New 构造 Syncer（不启动）。interval <= 0 回落默认 30s。
// db 不允许为 nil（上层在历史存储不可用时不得创建本对象）。
func New(log Logger, dev *device.Device, db *sql.DB, interval time.Duration) *Syncer {
	if interval <= 0 {
		interval = defaultSyncInterval
	}
	return &Syncer{
		log:      log,
		dev:      dev,
		db:       db,
		interval: interval,
		stop:     make(chan struct{}),
		done:     make(chan struct{}),
	}
}

// DeviceID 返回被同步设备的 ID。
func (s *Syncer) DeviceID() string { return s.dev.ID() }

// Interval 返回同步间隔时长。
func (s *Syncer) Interval() time.Duration { return s.interval }

// Disabled 报告该设备短信同步是否已被禁用。
func (s *Syncer) Disabled() bool { return s.disabled.Load() }

// Start 启动同步循环并阻塞直到 ctx 取消、Stop 或能力禁用。
// 首次同步立即执行，之后按 interval 周期执行。返回时 done 被关闭。
func (s *Syncer) Start(ctx context.Context) {
	defer close(s.done)
	s.log.Info("sms: starting", "dev", s.dev.ID(), "interval", s.interval.String())

	ticker := time.NewTicker(s.interval)
	defer ticker.Stop()

	for {
		if s.disabled.Load() {
			return
		}
		_, err := s.SyncOnce(ctx)
		if err != nil && !s.Disabled() {
			// 非禁用错误（连接失败/系统忙等）：告警后下轮重试。
			s.log.Warn("sms: sync failed, will retry", "dev", s.dev.ID(), "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-s.stop:
			return
		case <-ticker.C:
		}
	}
}

// Stop 请求停止同步循环（幂等）。
func (s *Syncer) Stop() {
	select {
	case <-s.stop:
	default:
		close(s.stop)
	}
}

// Done 返回同步循环退出信号。
func (s *Syncer) Done() <-chan struct{} { return s.done }

// SyncOnce 执行一次同步：拉取全部短信并增量入库。
// 返回本次新增条数（去重后）。错误不包含短信正文。
// 已禁用时不做网络请求，返回 0, nil。
func (s *Syncer) SyncOnce(ctx context.Context) (int, error) {
	if s.disabled.Load() {
		return 0, nil
	}
	client, release, err := s.dev.Lease(ctx)
	if err != nil {
		if device.PermanentError(err) {
			s.disable("permanent credential error")
		}
		return 0, err
	}
	defer release()

	msgs, err := client.Sms.GetMessages(1, enums.BoxTypeMixInbox, 0,
		enums.SortTypeDate, false, false)
	if err != nil {
		switch device.Classify(err) {
		case device.KindUnsupported:
			s.disable("sms API not supported")
		case device.KindPermanent:
			s.disable("permanent credential error")
		}
		return 0, err
	}

	// 增量入库：唯一键 (device_id, cpe_index) 冲突静默忽略（重复同步无重复）。
	ins := db.SmsInserter{}
	added := 0
	for _, m := range msgs {
		ok, err := ins.Insert(ctx, s.db, s.dev.ID(), db.SmsRow{
			CpeIndex:   m.Index,
			Phone:      m.Phone,
			Content:    m.Content,
			Status:     int(m.Status),
			ReceivedAt: m.DateTime.Unix(),
		})
		if err != nil {
			// 写库失败不影响本轮其它短信；下轮重试补齐。
			s.log.Warn("sms: write failed", "dev", s.dev.ID(), "err", err)
			continue
		}
		if ok {
			added++
		}
	}
	// 日志只记数量，绝不记录短信正文 / 号码。
	s.log.Info("sms: synced", "dev", s.dev.ID(), "total", len(msgs), "new", added)
	return added, nil
}

// disable 将本设备短信同步置为禁用（只触发一次日志；reason 不含短信内容）。
func (s *Syncer) disable(reason string) {
	if s.disabled.CompareAndSwap(false, true) {
		s.log.Warn("sms: disabled for device", "dev", s.dev.ID(), "reason", reason)
	}
}