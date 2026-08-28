//go:build linux

package slogx

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"os"
	"strings"
	"sync"
)

// newPlatformHandler 在 Linux 上通过 /dev/log 写 syslog。
func newPlatformHandler(level slog.Level) (slog.Handler, error) {
	conn, err := net.Dial("unixgram", "/dev/log")
	if err != nil {
		return nil, err
	}
	return &syslogHandler{conn: conn, level: level}, nil
}

// syslogHandler 把 slog 记录转为 BSD syslog 报文发往 /dev/log（facility=daemon）。
type syslogHandler struct {
	mu    sync.Mutex
	conn  net.Conn
	level slog.Level
}

// Enabled 按等级过滤。
func (h *syslogHandler) Enabled(_ context.Context, l slog.Level) bool {
	return l >= h.level
}

// Handle 序列化为 BSD syslog 消息并写入 /dev/log。
func (h *syslogHandler) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()

	var b strings.Builder
	fmt.Fprintf(&b, "<%d>", priority(r.Level))
	tag := appName
	if tag == "" {
		tag = "huawei-cpe"
	}
	fmt.Fprintf(&b, "%s[%d]: %s", tag, os.Getpid(), r.Message)
	for _, a := range attrsOf(r) {
		fmt.Fprintf(&b, " %s=%s", a.Key, a.Value)
	}
	_, err := h.conn.Write([]byte(b.String()))
	return err
}

func (h *syslogHandler) WithAttrs(attrs []slog.Attr) slog.Handler { return h }
func (h *syslogHandler) WithGroup(name string) slog.Handler       { return h }

// attrsOf 提取 Record 的 attrs。
func attrsOf(r slog.Record) []slog.Attr {
	var out []slog.Attr
	r.Attrs(func(a slog.Attr) bool {
		out = append(out, a)
		return true
	})
	return out
}

// priority 映射 slog 等级到 syslog facility=daemon(3)*8 + severity。
func priority(l slog.Level) int {
	sev := 6 // info
	switch {
	case l >= slog.LevelError:
		sev = 3 // err
	case l >= slog.LevelWarn:
		sev = 4 // warning
	case l >= slog.LevelInfo:
		sev = 6 // info
	default:
		sev = 7 // debug
	}
	return 3<<3 | sev
}

// appName 可被 main 设置（显示在 logread 中的 tag）。
var appName = "huawei-cpe"

// SetAppName 设置 syslog tag。
func SetAppName(name string) { appName = name }
