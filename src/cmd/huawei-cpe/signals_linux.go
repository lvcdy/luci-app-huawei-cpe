//go:build linux

package main

import (
	"os"
	"os/signal"
	"syscall"
)

// setupReloadSignal 在 Linux（OpenWrt）上监听 SIGUSR1（procd reload 契约）。
func setupReloadSignal(reloadCh chan struct{}) (stop func()) {
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGUSR1)
	go func() {
		for range sigCh {
			select {
			case reloadCh <- struct{}{}:
			default: // 已有待处理 reload，合并
			}
		}
	}()
	return func() { signal.Stop(sigCh) }
}
