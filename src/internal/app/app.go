// Package app 管理 daemon 生命周期：启动、SIGUSR1 配置热重载、SIGTERM 优雅退出。
//
// OpenWrt procd 契约：
//   - 不做 daemonize（procd 已托管）
//   - SIGUSR1 触发 reload（procd_send_signal / procd_set_param file 变更）
//   - SIGTERM 优雅退出
package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"huawei-cpe/internal/config"
	"huawei-cpe/internal/httpapi"
)

// State 持有 daemon 运行期组件（后续里程碑挂载 poller/device/db 等）。
type State struct {
	log *slog.Logger
	cfg *config.Config
	api *httpapi.Server
}

// New 创建运行状态（仅持有配置与日志，组件在 Start 中装配）。
func New(log *slog.Logger, cfg *config.Config) *State {
	return &State{log: log, cfg: cfg}
}

// Run 启动全部组件并阻塞直到收到退出信号。
// reloadCh 由 main 传入：procd 发送 SIGUSR1 → main 触发 reload → 本方法重读配置。
func (s *State) Run(reloadCh <-chan struct{}) error {
	s.log.Info("huawei-cpe starting",
		slog.String("config", s.cfg.Redacted()))

	// 装配 HTTP API（仅回环 127.0.0.1）。
	s.api = httpapi.New(s.log, s.cfg)

	// 启动 API 服务（真实监听在 Start 中）。
	if err := s.api.Start(); err != nil {
		return fmt.Errorf("start http api: %w", err)
	}
	s.log.Info("http api listening on 127.0.0.1:9090")

	// Phase 1 仅骨架：poller / device manager 在 P1.6+ 挂载。

	sigCh := make(chan os.Signal, 2)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
	defer signal.Stop(sigCh)

	for {
		select {
		case sig := <-sigCh:
			s.log.Info("received signal, shutting down", slog.String("signal", sig.String()))
			s.shutdown()
			return nil
		case <-reloadCh:
			s.log.Info("config reload requested")
			s.reload()
		}
	}
}

// reload 重读 UCI 配置并应用（紧凑输出脱敏摘要）。
func (s *State) reload() {
	cfg, err := config.Load(s.cfg.Path)
	if err != nil {
		s.log.Error("config reload failed, keeping old config", "err", err)
		return
	}
	before := s.cfg
	s.cfg = cfg
	s.api.UpdateConfig(cfg)
	s.log.Info("config reloaded",
		slog.String("before", before.Redacted()),
		slog.String("after", cfg.Redacted()))
}

// shutdown 优雅停止所有组件。
func (s *State) shutdown() {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if s.api != nil {
		if err := s.api.Shutdown(ctx); err != nil && !errors.Is(err, context.DeadlineExceeded) {
			s.log.Error("http api shutdown", "err", err)
		}
	}
	s.log.Info("huawei-cpe stopped")
}
