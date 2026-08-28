// huawei-cpe daemon 入口。
//
// OpenWrt procd 契约：
//   - `huawei-cpe run`：前台运行（procd 托管，不 daemonize）
//   - SIGUSR1：procd_send_signal → 重读 UCI 配置（热 reload）
//   - SIGTERM：优雅退出
//   - 日志走 syslog（logread 查看）
package main

import (
	"flag"
	"fmt"
	"log/slog"
	"os"

	"huawei-cpe/internal/app"
	"huawei-cpe/internal/config"
	"huawei-cpe/internal/slogx"
)

const (
	defaultConfigPath = "/etc/config/huawei_cpe"
	version           = "0.1.0"
)

func main() {
	log := slogx.New(slog.LevelInfo)
	log.Info("huawei-cpe", slog.String("version", version),
		slog.String("pid", fmt.Sprintf("%d", os.Getpid())))

	if err := realMain(log); err != nil {
		log.Error("fatal", "err", err.Error())
		os.Exit(1)
	}
}

func realMain(log *slog.Logger) error {
	var configPath string
	flag.StringVar(&configPath, "config", defaultConfigPath, "path to UCI config file")
	flag.Parse()

	// 载入配置；文件缺失是在开发机上正常（用默认空配置），OpenWrt 上安装包自带。
	cfg, err := config.Load(configPath)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	st := app.New(log, cfg)

	// reload 通道：SIGUSR1 → procd reload（配置变更热加载）。
	reloadCh := make(chan struct{}, 1)
	stop := setupReloadSignal(reloadCh)
	defer stop()

	return st.Run(reloadCh)
}
