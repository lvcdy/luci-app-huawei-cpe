// Package poller 实现单设备的轮询采集循环：
// 以固定间隔串行调用 SDK 采集 status/signal/information/net/traffic，
// 计算流量差分速率，并按需探测设备能力（NotSupported 记入能力矩阵后不再调用）。
//
// 职责边界：
//   - 只依赖 device.Lease 借出的 SDK Client，永不直接创建 SDK 连接；
//   - 单设备单 goroutine，串行请求，永不并发打 CPE（架构 §6）；
//   - 采集结果写入内存快照（由 cache 层消费）；错误按 device.Classify 分类处理。
package poller

import (
	"context"
	"sync"
	"time"

	"huawei-cpe/internal/device"
)

// Logger 是 poller 依赖的最小日志接口（与 device.Logger 对齐）。
type Logger interface {
	Debug(msg string, args ...any)
	Info(msg string, args ...any)
	Warn(msg string, args ...any)
	Error(msg string, args ...any)
}

// defaultPollInterval 是未配置轮询间隔时的缺省值（秒）。
const defaultPollInterval = 60

// SignalState 是 device/signal 的类型化解（字段缺失时保持零值）。
type SignalState struct {
	RSRP int    // dBm（华为存的值；负值如 -75）
	RSRQ int    // dB（华为存的值 = 实测 x2，如 -16）
	SINR int    // dB（正负均可）
	RSSI int    // dBm（2G/4G）
	Mode string // "4G"/"5G"/"5G NSA" 等
	PLMN string // 运营商 PLMN（如 46000）
	Band string // 服务小区 band（如 "B3"）
	PCI  int    // 物理小区 ID
}

// NetworkState 是 net/network + net/current-plmn 的类型化解。
type NetworkState struct {
	CurrentNetworkType   string // "GSM"/"WCDMA"/"LTE"/"NR" 等
	CurrentServiceDomain string // 服务域（如 "CS_PS"）
	Roaming              int    // 0=非漫游 1=漫游
	RegisteredPlmn       string // 注册 PLMN
	ProviderName         string // 运营商显示名（current-plmn）
	ShortName            string // 运营商短名（current-plmn）
}

// TrafficState 是 monitoring/traffic-statistics + month_statistics 的类型化解。
// RxRate/TxRate 是差分速率（bytes/s），由 poller 根据时间差计算。
type TrafficState struct {
	CurrentRxBytes int64 // 当前会话累计下行字节
	CurrentTxBytes int64 // 当前会话累计上行字节
	TotalRxBytes   int64 // 设备总下行字节
	TotalTxBytes   int64 // 设备总上行字节
	MonthRxBytes   int64 // 本月下行字节（month_statistics）
	MonthTxBytes   int64 // 本月上行字节
	RxRate         float64 // bytes/s（差分）
	TxRate         float64 // bytes/s（差分）
}

// Capabilities 是设备能力矩阵（探测一次并缓存；NotSupported → false 后不再调用）。
type Capabilities struct {
	SMS      bool
	Signal   bool
	Traffic  bool
	Cellular bool
	CellInfo bool
	Reboot   bool
}

// Snapshot 是一次轮询采集的完整内存快照（不含凭据）。
type Snapshot struct {
	At       time.Time
	Online   bool           // CPE 在线（API 可达且认证成功）
	Info     map[string]any // device/information 原始字段（模型/固件/序列号等）
	Signal   SignalState
	Network  NetworkState
	Traffic  TrafficState
	Caps     Capabilities
	HasError bool // 本周期至少有一项采集失败
}

// Poller 是单设备的轮询采集器。
//
// 并发安全：Last 通过互斥锁读。轮询循环本身串行。
type Poller struct {
	log  Logger
	dev  *device.Device
	stop chan struct{}

	mu   sync.RWMutex
	last Snapshot // 最近一次采集快照

	// 差分速率状态（仅轮询 goroutine 访问，无需加锁）
	prevRx int64
	prevTx int64
	prevAt time.Time
}

// New 构造 Poller（不启动）。设备轮询间隔取自 cfg.PollingInterval。
// 能力矩阵默认全部假设支持；遇 NotSupported 由 disableCap 逐项禁用（架构 §6）。
func New(log Logger, dev *device.Device) *Poller {
	return &Poller{
		log:  log,
		dev:  dev,
		stop: make(chan struct{}),
		last: Snapshot{
			Caps: Capabilities{
				SMS:      true,
				Signal:   true,
				Traffic:  true,
				Cellular: true,
				CellInfo: true,
				Reboot:   true,
			},
		},
	}
}

// Interval 返回该设备轮询间隔时长；未配置回落默认值。
func (p *Poller) Interval() time.Duration {
	if d := p.dev.PollingInterval(); d > 0 {
		return d
	}
	return defaultPollInterval * time.Second
}

// Start 启动轮询循环并阻塞直到 ctx 取消或设备停止。
// 首次采集立即执行，之后按 Interval 周期执行。
func (p *Poller) Start(ctx context.Context) {
	p.log.Info("poller: starting", "dev", p.dev.ID(), "interval", p.Interval().String())
	ticker := time.NewTicker(p.Interval())
	defer ticker.Stop()

	p.pollOnce(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-p.stop:
			return
		case <-ticker.C:
			p.pollOnce(ctx)
		}
	}
}

// Stop 请求停止轮询循环（配合 Start 的 ctx 使用，二选一）。
func (p *Poller) Stop() {
	select {
	case <-p.stop:
	default:
		close(p.stop)
	}
}

// Last 返回最近一次采集快照。
func (p *Poller) Last() Snapshot {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.last
}

// pollOnce 执行一次完整采集：connect → status → info → signal → net → traffic。
// 任何一步失败按错误分类处理，不影响后续步骤（能力矩阵跳过不支持的端点）。
func (p *Poller) pollOnce(ctx context.Context) {
	client, release, err := p.dev.Lease(ctx)
	if err != nil {
		if device.PermanentError(err) {
			p.log.Error("poller: permanent credential error, stopping", "dev", p.dev.ID())
			// 永久错误：不再继续该设备采集（由上层事件上报 cpe.auth_failed）
			p.markError()
			return
		}
		p.log.Warn("poller: connect failed", "dev", p.dev.ID(), "err", err)
		p.markError()
		return
	}
	defer release()

	// 继承上一轮的能力矩阵（禁用状态跨周期持久）；
	// 本轮内 NotSupported 通过 disableCap 更新到 snap.Caps。
	snap := Snapshot{At: time.Now(), Online: true, Caps: p.deviceCaps()}

	// 1. monitoring/status —— 核心状态（连接状态 + 当前网络类型）
	if status, err := client.Monitoring.Status(); err == nil {
		snap.Info = mergeInfo(snap.Info, status)
		p.fillFromStatus(&snap, status)
	} else {
		p.handlePollErr("status", err, &snap)
	}
	// 2. device/information —— 型号/固件/序列号
	if info, err := client.Device.Information(); err == nil {
		snap.Info = mergeInfo(snap.Info, info)
	} else {
		p.handlePollErr("information", err, &snap)
	}
	// 3. device/signal —— 信号（能力矩阵：不支持则跳过）
	if snap.Caps.Signal {
		if sig, err := client.Device.Signal(); err == nil {
			p.fillFromSignal(&snap, sig)
		} else {
			p.handlePollErr("signal", err, &snap)
		}
	}
	// 4. net/network —— 当前网络
	if snap.Caps.Cellular {
		if net, err := client.Net.Network(); err == nil {
			p.fillFromNetwork(&snap, net)
		} else {
			p.handlePollErr("network", err, &snap)
		}
	}
	// 5. net/current-plmn —— 运营商名（不支持可容忍）
	if snap.Caps.Cellular {
		if plmn, err := client.Net.CurrentPlmn(); err == nil {
			p.fillFromPlmn(&snap, plmn)
		} else {
			p.handlePollErr("current-plmn", err, &snap)
		}
	}
	// 6. monitoring/traffic-statistics —— 流量 + 差分速率
	if snap.Caps.Traffic {
		if tr, err := client.Monitoring.TrafficStatistics(); err == nil {
			p.fillFromTraffic(&snap, tr)
		} else {
			p.handlePollErr("traffic-statistics", err, &snap)
		}
	}
	// 7. monitoring/month_statistics —— 本月流量（可选，不支持不报错）
	if snap.Caps.Traffic {
		if mo, err := client.Monitoring.MonthStatistics(); err == nil {
			if v, ok := int64p(mo, "MonthReceive"); ok {
				snap.Traffic.MonthRxBytes = v
			}
			if v, ok := int64p(mo, "MonthTransmit"); ok {
				snap.Traffic.MonthTxBytes = v
			}
		} else if device.Classify(err) != device.KindUnsupported {
			p.handlePollErr("month_statistics", err, &snap)
		}
	}

	p.calcRate(&snap)

	p.mu.Lock()
	p.last = snap
	p.mu.Unlock()
}

// fillFromStatus 解析 monitoring/status。
func (p *Poller) fillFromStatus(snap *Snapshot, m map[string]any) {
	// ConnectionStatus: 901=Connected（enums.ConnectionStatusConnected）。
	// 不直接引入 SDK 枚举：保持 poller 只依赖 map 数据，型号差异不写死。
	if v, ok := intp(m, "ConnectionStatus"); ok {
		snap.Online = v == 901 // ConnectionStatusConnected
	}
	snap.Network.CurrentNetworkType = strOr(m, "CurrentNetworkType", snap.Network.CurrentNetworkType)
	snap.Network.CurrentServiceDomain = strOr(m, "CurrentServiceDomain", snap.Network.CurrentServiceDomain)
	snap.Network.Roaming = intpOr(m, "Roaming", snap.Network.Roaming)
}

// fillFromSignal 解析 device/signal。
func (p *Poller) fillFromSignal(snap *Snapshot, m map[string]any) {
	// 华为 device/signal 键名：rsrp/rsrq/sinr/rssi
	snap.Signal.RSRP = intpOr(m, "rsrp", 0)
	snap.Signal.RSRQ = intpOr(m, "rsrq", 0)
	snap.Signal.SINR = intpOr(m, "sinr", 0)
	snap.Signal.RSSI = intpOr(m, "rssi", 0)
	snap.Signal.Mode = strOr(m, "mode", "")
	snap.Signal.PCI = intpOr(m, "pci", 0)
	if band, ok := str(m, "band"); ok {
		snap.Signal.Band = band
	} else if lte, ok := str(m, "lte_band"); ok {
		snap.Signal.Band = lte
	}
	snap.Signal.PLMN = strOr(m, "plmn", snap.Signal.PLMN)
}

// fillFromNetwork 解析 net/network（根是 <response> 被剥离，键直接可见）。
func (p *Poller) fillFromNetwork(snap *Snapshot, m map[string]any) {
	snap.Network.CurrentNetworkType = strOr(m, "CurrentNetworkType", snap.Network.CurrentNetworkType)
	snap.Network.CurrentServiceDomain = strOr(m, "CurrentServiceDomain", snap.Network.CurrentServiceDomain)
	snap.Network.Roaming = intpOr(m, "Roaming", snap.Network.Roaming)
}

// fillFromPlmn 解析 net/current-plmn（根是 <currentplmn> 保留为顶层键，也可能嵌套）。
func (p *Poller) fillFromPlmn(snap *Snapshot, m map[string]any) {
	// 直接键（某些型号）
	if name, ok := str(m, "ProviderName"); ok {
		snap.Network.ProviderName = name
	}
	if short, ok := str(m, "ShortName"); ok {
		snap.Network.ShortName = short
	}
	// 嵌套键（某些型号套 <currentplmn> 子树）
	if sub, ok := nested(m, "currentplmn"); ok {
		if name, ok := str(sub, "ProviderName"); ok {
			snap.Network.ProviderName = name
		}
		if short, ok := str(sub, "ShortName"); ok {
			snap.Network.ShortName = short
		}
	}
}

// fillFromTraffic 解析 monitoring/traffic-statistics。
func (p *Poller) fillFromTraffic(snap *Snapshot, m map[string]any) {
	t := &snap.Traffic
	// 键名：CurrentDownload/CurrentUpload/TotalDownload/TotalUpload
	if v, ok := int64p(m, "CurrentDownload"); ok {
		t.CurrentRxBytes = v
	}
	if v, ok := int64p(m, "CurrentUpload"); ok {
		t.CurrentTxBytes = v
	}
	if v, ok := int64p(m, "TotalDownload"); ok {
		t.TotalRxBytes = v
	}
	if v, ok := int64p(m, "TotalUpload"); ok {
		t.TotalTxBytes = v
	}
	// 备用键：TotalBytes 形态（某些型号）
	if v, ok := int64p(m, "TotalBytes"); ok && t.TotalRxBytes == 0 {
		t.TotalRxBytes = v
	}
}

// calcRate 计算差分速率（bytes/s），并保存本周期基准。
// 解决 CPE 重启/换卡导致的计数复位：差值非负才采用。
func (p *Poller) calcRate(snap *Snapshot) {
	now := snap.At
	if !p.prevAt.IsZero() {
		dt := now.Sub(p.prevAt).Seconds()
		if dt > 0 {
			drx := snap.Traffic.TotalRxBytes - p.prevRx
			dtx := snap.Traffic.TotalTxBytes - p.prevTx
			if drx >= 0 {
				snap.Traffic.RxRate = float64(drx) / dt
			}
			if dtx >= 0 {
				snap.Traffic.TxRate = float64(dtx) / dt
			}
		}
	}
	p.prevAt = now
	p.prevRx = snap.Traffic.TotalRxBytes
	p.prevTx = snap.Traffic.TotalTxBytes
}

// handlePollErr 按错误分类处理单步采集失败。
func (p *Poller) handlePollErr(step string, err error, snap *Snapshot) {
	switch device.Classify(err) {
	case device.KindUnsupported:
		p.log.Debug("poller: unsupported, caching capability", "dev", p.dev.ID(), "step", step)
		disableCap(snap, step)
	case device.KindPermanent:
		p.log.Error("poller: permanent error", "dev", p.dev.ID(), "step", step)
		snap.Online = false
	default:
		p.log.Warn("poller: poll failed", "dev", p.dev.ID(), "step", step, "err", err)
		snap.HasError = true
	}
}

// deviceCaps 返回能力矩阵（读副本）。
func (p *Poller) deviceCaps() Capabilities {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.last.Caps
}

// disableCap 将某一步对应的能力标记为 false（本轮快照内缓存；结尾写回后持久）。
func disableCap(snap *Snapshot, step string) {
	switch step {
	case "signal":
		snap.Caps.Signal = false
	case "traffic-statistics", "month_statistics":
		snap.Caps.Traffic = false
	case "network", "current-plmn":
		snap.Caps.Cellular = false
	}
}

// markError 在不可连接时把快照标记为离线（保留上次数据但 Online=false）。
func (p *Poller) markError() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.last.Online = false
	p.last.HasError = true
	p.last.At = time.Now()
}

// mergeInfo 合并原始信息字段（status 与 information 都可能带模型/固件）。
func mergeInfo(dst, src map[string]any) map[string]any {
	if dst == nil {
		dst = map[string]any{}
	}
	for k, v := range src {
		if _, exists := dst[k]; !exists {
			dst[k] = v
		}
	}
	return dst
}