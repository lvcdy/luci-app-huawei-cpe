//go:build !linux

package slogx

import (
	"fmt"
	"log/slog"
)

// newPlatformHandler 在非 Linux（开发机）上不使用 syslog，返回错误以走 stderr 回退。
func newPlatformHandler(level slog.Level) (slog.Handler, error) {
	return nil, fmt.Errorf("syslog only supported on linux")
}
