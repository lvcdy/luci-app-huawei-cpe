// 设备写操作（功能 5 锁频 / 功能 6 流量开关 / 功能 7 重启 / 网络模式）。
//
// 全部操作走同一个租约模式：Lease 拿连接 → SDK 调用 → release。
// 重启会断开当前会话（CPE 重启期间轮询短暂失败，属预期）。
package device

import (
	"context"
	"errors"
	"fmt"

	"github.com/lvcdy/huawei-lte-api-go/enums"
)

// CellLockReq 是锁频参数（api/net/lock-cell 语义）。
// 解锁 = Lock=0 且 Freq=0 且 PCI=0（华为约定："0,0,0"）。
type CellLockReq struct {
	Lock int // 1=锁定
	Freq int // 锁定频率（ARFCN；解锁=0）
	PCI  int // 锁定 PCI（解锁=0）
}

// SetCellLock 设置小区锁定（锁频/锁小区/解锁）。
//
//   - 按频率锁定：Lock=1, Freq=目标ARFCN, PCI=0
//   - 按小区锁定：Lock=2, Freq=ARFCN, PCI=目标PCI
//   - 解锁：Lock=0, Freq=0, PCI=0
//
// 参数校验：Lock∈{0,1,2}；Lock!=0 时 Freq 必须 >0。
func (d *Device) SetCellLock(ctx context.Context, req CellLockReq) error {
	if req.Lock < 0 || req.Lock > 2 {
		return errors.New("lock must be 0 (unlock), 1 (freq) or 2 (cell)")
	}
	if req.Lock != 0 && req.Freq <= 0 {
		return errors.New("freq (ARFCN) is required when locking")
	}
	if req.PCI < 0 {
		return errors.New("pci must be >= 0")
	}

	client, release, err := d.Lease(ctx)
	if err != nil {
		return fmt.Errorf("device unreachable: %w", err)
	}
	defer release()

	if _, err := client.Ntwk.LockCell(req.Lock, req.Freq, req.PCI); err != nil {
		return fmt.Errorf("lock-cell failed: %w", err)
	}
	return nil
}

// NetModeReq 是网络模式设置（api/net/net-mode 语义）。
// 任一字段为 nil 时保持该维度不变（SDK 用 interface{} 传值）。
type NetModeReq struct {
	LTEBand     interface{} // 4G band 位掩码（enums.LTEBand/LTEBandAll）
	NetworkBand interface{} // 频段位掩码（enums.NetworkBand/NetworkBandAll）
	NetworkMode interface{} // 网络模式（enums.NetworkMode 字符串）
}

// SetNetMode 设置网络模式与频段（2G/3G/4G/5G 选择 + 频段锁定）。
// 例：4G-only → SetNetMode(nil, nil, enums.NetworkMode4GOnly)。
func (d *Device) SetNetMode(ctx context.Context, req NetModeReq) error {
	client, release, err := d.Lease(ctx)
	if err != nil {
		return fmt.Errorf("device unreachable: %w", err)
	}
	defer release()

	if _, err := client.Net.SetNetMode(req.LTEBand, req.NetworkBand, req.NetworkMode); err != nil {
		return fmt.Errorf("set-net-mode failed: %w", err)
	}
	return nil
}

// DataSwitch 开关移动数据（api/dialup/mobile-dataswitch 语义）。
// on=true 开启流量，on=false 关闭流量。
func (d *Device) SetDataSwitch(ctx context.Context, on bool) error {
	v := 0
	if on {
		v = 1
	}
	client, release, err := d.Lease(ctx)
	if err != nil {
		return fmt.Errorf("device unreachable: %w", err)
	}
	defer release()

	if _, err := client.DialUp.SetMobileDataswitch(v); err != nil {
		return fmt.Errorf("mobile-dataswitch failed: %w", err)
	}
	return nil
}

// Reboot 重启 CPE（api/device/control, Control=1）。
// 注意：重启后会话失效，调用方应等待重新上线（轮询自动恢复）。
func (d *Device) Reboot(ctx context.Context) error {
	client, release, err := d.Lease(ctx)
	if err != nil {
		return fmt.Errorf("device unreachable: %w", err)
	}
	defer release()

	if _, err := client.Device.SetControl(enums.ControlModeReboot); err != nil {
		return fmt.Errorf("reboot failed: %w", err)
	}
	return nil
}
