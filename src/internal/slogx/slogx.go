// Package slogx 提供跨平台日志：OpenWrt(Linux) 上输出到 syslog（logread 查看），
// 本机开发时回退到 stderr。所有条目绝不包含密钥字段。
package slogx

import (
	"log/slog"
	"os"
)

// New 返回一个输出到 syslog（OpenWrt）或 stderr（开发）的 logger。
func New(level slog.Level) *slog.Logger {
	return slog.New(NewHandler(level))
}

// NewHandler 返回经平台适配的 slog.Handler：
//   - linux 且可用 → syslog（OpenWrt logread 可查）
//   - 其它 → stderr 文本
func NewHandler(level slog.Level) slog.Handler {
	if h, err := newPlatformHandler(level); err == nil {
		return h
	}
	return slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level})
}
