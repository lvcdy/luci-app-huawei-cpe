//go:build !linux

package main

// setupReloadSignal 在非 Linux（开发机）上无 SIGUSR1，提供空实现保持可编译。
func setupReloadSignal(reloadCh chan struct{}) (stop func()) {
	return func() {}
}