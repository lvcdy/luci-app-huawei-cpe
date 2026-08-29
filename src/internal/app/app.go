// Package app 管理 daemon 生命周期：启动、SIGUSR1 配置热重载、SIGTERM 优雅退出。
//
// OpenWrt procd 契约：
//   - 不做 daemonize（procd 已托管）
//   - SIGUSR1 触发 reload（procd_send_signal / procd_set_param file 变更）
//   - SIGTERM 优雅退出
package app

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"huawei-cpe/internal/cache"
	"huawei-cpe/internal/config"
	"huawei-cpe/internal/db"
	"huawei-cpe/internal/device"
	"huawei-cpe/internal/history"
	"huawei-cpe/internal/httpapi"
	"huawei-cpe/internal/poller"
	"huawei-cpe/internal/sms"
)

// State 持有 daemon 运行期组件。
type State struct {
	log     *slog.Logger
	cfg     *config.Config
	api     *httpapi.Server
	store   *cache.Store
	mgr     *device.Manager
	sqldb   *sql.DB // 历史存储（nil = 历史禁用）
	pollers []*poller.Poller
	syncers []*sms.Syncer
	cancel  context.CancelFunc // 轮询循环的取消函数

	quitOnce sync.Once
	quit     chan struct{} // 可编程退出（测试用；关闭后 Run 返回）
}

// New 创建运行状态（仅持有配置与日志，组件在 Start 中装配）。
func New(log *slog.Logger, cfg *config.Config) *State {
	return &State{log: log, cfg: cfg, quit: make(chan struct{})}
}

// Quit 请求 Run 返回（幂等）。生产路径靠信号；测试/内嵌场景可主动调用。
func (s *State) Quit() { s.quitOnce.Do(func() { close(s.quit) }) }

// Run 启动全部组件并阻塞直到收到退出信号。
// reloadCh 由 main 传入：procd 发送 SIGUSR1 → main 触发 reload → 本方法重读配置。
func (s *State) Run(reloadCh <-chan struct{}) error {
	s.log.Info("huawei-cpe starting",
		slog.String("config", s.cfg.Redacted()))

	ctx, cancel := context.WithCancel(context.Background())
	s.cancel = cancel

	// 装配：设备管理器 + 快照缓存 + 轮询器。
	s.store = cache.New()
	s.mgr = device.NewManager(s.log, s.cfg.CPEs)
	s.openHistory(ctx)
	s.startPollers(ctx)
	s.startSMSSyncers(ctx)

	// 装配 HTTP API（仅回环 127.0.0.1），读缓存不触发 CPE。
	s.api = httpapi.New(s.log, s.cfg, s.store, s.mgr)
	s.api.SetDB(s.sqldb)
	s.api.SetPollers(s.pollersByID())

	// 启动 API 服务（真实监听在 Start 中）。
	if err := s.api.Start(); err != nil {
		cancel()
		return fmt.Errorf("start http api: %w", err)
	}
	s.log.Info("http api listening on 127.0.0.1:9090")

	sigCh := make(chan os.Signal, 2)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
	defer signal.Stop(sigCh)

	for {
		select {
		case sig := <-sigCh:
			s.log.Info("received signal, shutting down", slog.String("signal", sig.String()))
			s.shutdown()
			return nil
		case <-s.quit:
			s.log.Info("quit requested, shutting down")
			s.shutdown()
			return nil
		case <-reloadCh:
			s.log.Info("config reload requested")
			s.reload(ctx)
		}
	}
}

// startPollers 为全部启用设备创建并启动轮询 goroutine（单设备单循环，串行采集）。
// 快照订阅方：内存缓存（API 读）+ 历史写入器（SQLite，可禁用）。
func (s *State) startPollers(ctx context.Context) {
	var sink poller.Sink = s.store
	if s.sqldb != nil {
		sink = fanoutSink{s.store, history.NewRecorder(s.log, s.sqldb)}
	}
	s.pollers = nil
	for _, d := range s.mgr.Enabled() {
		p := poller.New(s.log, d, sink)
		s.pollers = append(s.pollers, p)
		go p.Start(ctx)
	}
	s.log.Info("pollers started", slog.Int("count", len(s.pollers)))
}

// fanoutSink 把快照同时分发给多个订阅方（串行、同协程，无竞争）。
type fanoutSink []poller.Sink

func (f fanoutSink) PutSnapshot(id string, snap poller.Snapshot) {
	for _, s := range f {
		s.PutSnapshot(id, snap)
	}
}

// openHistory 打开历史 SQLite；失败仅告警降级（历史功能关闭，核心采集不受影响）。
// db_path 为空 = 显式禁用历史。
func (s *State) openHistory(ctx context.Context) {
	path := s.cfg.History.DBPath
	if path == "" {
		s.log.Info("history storage disabled (db_path empty)")
		return
	}
	d, err := db.Open(path)
	if err != nil {
		s.log.Warn("history storage unavailable, continuing without it", "path", path, "err", err)
		return
	}
	s.sqldb = d
	go s.pruneLoop(ctx)
	s.log.Info("history storage ready", "path", path, "retention_days", s.cfg.History.RetentionDays)
}

// pruneLoop 启动时清一次过期历史，之后每天清理一次（架构 §3.2）。
func (s *State) pruneLoop(ctx context.Context) {
	days := s.cfg.History.RetentionDays
	if err := db.PruneHistory(ctx, s.sqldb, days); err != nil {
		s.log.Warn("history prune", "err", err)
	}
	t := time.NewTicker(24 * time.Hour)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := db.PruneHistory(ctx, s.sqldb, days); err != nil {
				s.log.Warn("history prune", "err", err)
			}
		}
	}
}

// stopPollers 停止全部轮询循环并等待退出（有界等待，防止采集中的请求卡住关机）。
func (s *State) stopPollers() {
	for _, p := range s.pollers {
		p.Stop()
	}
	for _, p := range s.pollers {
		select {
		case <-p.Done():
		case <-time.After(6 * time.Second):
			s.log.Warn("poller stop timed out", slog.String("dev", p.DeviceID()))
		}
	}
	s.pollers = nil
}

// pollersByID 返回 device id → poller 的映射（功能 10：httpapi 控制暂停/恢复）。
func (s *State) pollersByID() map[string]httpapi.PauseController {
	m := make(map[string]httpapi.PauseController, len(s.pollers))
	for _, p := range s.pollers {
		m[p.DeviceID()] = p
	}
	return m
}

// startSMSSyncers 为全部启用设备创建并启动短信同步 goroutine。
// 数据库未打开（sqldb == nil = 历史/短信存储不可用）时跳过——同步器依赖 SQLite 去重入库。
func (s *State) startSMSSyncers(ctx context.Context) {
	if s.sqldb == nil {
		s.log.Info("sms sync disabled (no history db)")
		return
	}
	s.syncers = nil
	for _, d := range s.mgr.Enabled() {
		syn := sms.New(s.log, d, s.sqldb, d.SMSSyncInterval())
		s.syncers = append(s.syncers, syn)
		go syn.Start(ctx)
	}
	s.log.Info("sms syncers started", slog.Int("count", len(s.syncers)))
}

// stopSMSSyncers 停止全部短信同步循环并等待退出（有界等待，防止卡住关机）。
func (s *State) stopSMSSyncers() {
	for _, syn := range s.syncers {
		syn.Stop()
	}
	for _, syn := range s.syncers {
		select {
		case <-syn.Done():
		case <-time.After(6 * time.Second):
			s.log.Warn("sms syncer stop timed out", slog.String("dev", syn.DeviceID()))
		}
	}
	s.syncers = nil
}

// reload 重读 UCI 配置并应用（紧凑输出脱敏摘要）。
// 设备集变化时重建轮询循环（简单可靠优先，架构 §6）。
func (s *State) reload(ctx context.Context) {
	cfg, err := config.Load(s.cfg.Path)
	if err != nil {
		s.log.Error("config reload failed, keeping old config", "err", err)
		return
	}
	before := s.cfg
	s.cfg = cfg
	s.api.UpdateConfig(cfg)

	changes := s.mgr.Update(cfg.CPEs)
	if len(changes) > 0 {
		s.log.Info("device set changed", slog.Any("changes", changes))
		s.stopPollers()
		s.startPollers(ctx)
		s.stopSMSSyncers()
		s.startSMSSyncers(ctx)
		s.api.SetPollers(s.pollersByID())
	}

	s.log.Info("config reloaded",
		slog.String("before", before.Redacted()),
		slog.String("after", cfg.Redacted()))
}

// shutdown 优雅停止所有组件（幂等）。
func (s *State) shutdown() {
	if s.cancel != nil {
		s.cancel()
	}
	s.stopPollers()
	s.stopSMSSyncers() // 必须在 sqldb.Close 之前：同步器依赖数据库句柄
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if s.api != nil {
		if err := s.api.Shutdown(ctx); err != nil && !errors.Is(err, context.DeadlineExceeded) {
			s.log.Error("http api shutdown", "err", err)
		}
	}
	if s.sqldb != nil {
		if err := s.sqldb.Close(); err != nil {
			s.log.Error("history db close", "err", err)
		}
		s.sqldb = nil
	}
	if s.mgr != nil {
		s.mgr.Close()
	}
	s.log.Info("huawei-cpe stopped")
}
