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

// Sink 接收每次轮询采集的快照（cache.Store 实现；nil = 仅保留在 Poller.Last）。
type Sink interface {
	PutSnapshot(id string, snap Snapshot)
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
	// 功能 1：小区详情增强字段（net/cell-info 并集）
	ARFCN     string // 服务小区 ARFCN（earfcn/nrarfcn 并集显示）
	EARFCN    string // LTE ARFCN（可空）
	NRARFCN   string // NR ARFCN（可空）
	Bandwidth int    // 小区带宽 MHz（0 = 未知）
	CQI       int    // 信道质量指示（0 = 未知）
	CellID    int64  // 小区标识（0 = 未知）
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
	CurrentRxBytes int64   // 当前会话累计下行字节
	CurrentTxBytes int64   // 当前会话累计上行字节
	TotalRxBytes   int64   // 设备总下行字节
	TotalTxBytes   int64   // 设备总上行字节
	MonthRxBytes   int64   // 本月下行字节（month_statistics）
	MonthTxBytes   int64   // 本月上行字节
	RxRate         float64 // bytes/s（差分）
	TxRate         float64 // bytes/s（差分）
}

// Capabilities 是设备能力矩阵（探测一次并缓存；NotSupported → false 后不再调用）。
// 零值陷阱：布尔零值 = false = “全部禁用”，初始化时必须显式全 true。
type Capabilities struct {
	SMS        bool
	Signal     bool
	Traffic    bool
	Cellular   bool
	CellInfo   bool
	Reboot     bool
	CarrierAgg bool // 载波聚合（seccellinfo）
	Neighbor   bool // 邻小区（nbrcellinfo）
	Lock       bool // 锁频（celllock/lock-cell）
	Log        bool // 系统操作日志（log/loginfo）
	DataSwitch bool // 流量开关（dialup/mobile-dataswitch）
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
	Cell     CellDetail  // 功能 1：服务小区详情
	Carrier  []CellState // 功能 3：载波聚合（辅小区）
	Neighbor []CellState // 功能 3：邻小区
	Lock     LockState   // 功能 5：当前锁频状态
	Data     DataState   // 功能 6：流量开关
	Log      []LogEntry  // 功能 4：系统操作日志（最近 N 条）
	QoS      QoSState    // 功能 2：AMBR / QCI / 速度限制状态
	HasError bool        // 本周期至少有一项采集失败
}

// QoSState 是功能 2：AMBR / QCI / 速度限制状态（monitoring/status + net/cell-info）。
// 0 值 = 未提供/未知（前端显示 “—” 而非误导性数值）。
type QoSState struct {
	QCI        int   // QoS 类别标识（mQos/QosPriority；0=未知）
	DlAmbr     int64 // 下行聚合最大比特率 kbps（0=未知）
	UlAmbr     int64 // 上行聚合最大比特率 kbps
	MaxDlSpeed int64 // 速度限制：下行上限 kbps（0=不限速/未知）
	MaxUlSpeed int64 // 速度限制：上行上限 kbps
	SpeedLimit bool  // 是否设置了速度限制（任一 Max 速度 > 0）
}

// LockState 是当前锁频参数（api/ntwk/celllock）。
type LockState struct {
	Lock    int    // 0=未锁定 1=已锁定（按频率） 2=按小区
	Freq    string // 锁定频率（ARFCN）
	PCI     int    // 锁定 PCI
	MaxFreq string // 锁定频率上限（部分固件）
}

// DataState 是移动数据开关状态（api/dialup/mobile-dataswitch）。
type DataState struct {
	DataSwitch int // 0=关 1=开（-1=未知/不支持）
}

// LogEntry 是系统操作日志的一条（api/log/loginfo）。
type LogEntry struct {
	Type  string // 日志类型（logtype）
	Level string // 级别（loglevel，如 INFO/WARN/ERROR）
	Time  string // 时间（固件格式，原样保留）
	Info  string // 内容（可能为 base64/编码，前端解码）
}

// Poller 是单设备的轮询采集器。
//
// 并发安全：Last 通过互斥锁读。轮询循环本身串行。
// 暂停机制（功能 10）：Suspend 置暂停标志并唤醒循环（跳过后续采集周期，
// 不向 CPE 发请求）；Resume 清标志并立即触发一次采集。
// 前端页面在后台（失焦）时暂停，前台恢复，降低对 CPE 的请求压力。
type Poller struct {
	log  Logger
	dev  *device.Device
	sink Sink // 快照订阅方（可空）
	stop chan struct{}
	done chan struct{} // Start 返回时关闭（等待旧循环退出，防止并发采集）

	mu        sync.RWMutex
	last      Snapshot // 最近一次采集快照
	suspended bool     // 暂停标志（Start 循环读取）

	notify chan struct{} // 暂停状态变化通知（Start 循环 select 监听）

	// 差分速率状态（仅轮询 goroutine 访问，无需加锁）
	prevRx int64
	prevTx int64
	prevAt time.Time
}

// New 构造 Poller（不启动）。设备轮询间隔取自 cfg.PollingInterval。
// 能力矩阵默认全部假设支持；遇 NotSupported 由 disableCap 逐项禁用（架构 §6）。
// sink 可为 nil（此时快照仅保留在 Last）。
func New(log Logger, dev *device.Device, sink Sink) *Poller {
	return &Poller{
		log:    log,
		dev:    dev,
		sink:   sink,
		stop:   make(chan struct{}),
		done:   make(chan struct{}),
		notify: make(chan struct{}, 1),
		last: Snapshot{
			Caps: Capabilities{
				SMS:        true,
				Signal:     true,
				Traffic:    true,
				Cellular:   true,
				CellInfo:   true,
				Reboot:     true,
				CarrierAgg: true,
				Neighbor:   true,
				Lock:       true,
				Log:        true,
				DataSwitch: true,
			},
		},
	}
}

// DeviceID 返回被采集设备的 ID。
func (p *Poller) DeviceID() string { return p.dev.ID() }

// Device 返回被采集的设备实例。
func (p *Poller) Device() *device.Device { return p.dev }

// Interval 返回该设备轮询间隔时长；未配置回落默认值。
func (p *Poller) Interval() time.Duration {
	if d := p.dev.PollingInterval(); d > 0 {
		return d
	}
	return defaultPollInterval * time.Second
}

// Start 启动轮询循环并阻塞直到 ctx 取消或 Stop 被调用。
// 首次采集立即执行，之后按 Interval 周期执行。返回时 done 被关闭。
// 暂停时（Suspend）不采集不请求 CPE；Resume 立即触发一次采集并恢复周期。
func (p *Poller) Start(ctx context.Context) {
	defer close(p.done)
	p.log.Info("poller: starting", "dev", p.dev.ID(), "interval", p.Interval().String())
	ticker := time.NewTicker(p.Interval())
	defer ticker.Stop()

	p.pollOnce(ctx)
	for {
		// 暂停期间：只监听 ctx/stop/notify，不触发采集
		if p.isSuspended() {
			select {
			case <-ctx.Done():
				return
			case <-p.stop:
				return
			case <-p.notify:
				// 被唤醒：若仍是暂停（并发 Suspend）继续循环，否则立即采集
				if !p.isSuspended() {
					p.pollOnce(ctx)
				}
			}
			continue
		}
		select {
		case <-ctx.Done():
			return
		case <-p.stop:
			return
		case <-p.notify:
			// 状态变化（可能刚被 Suspend/Resume）：回到循环头重新判断
		case <-ticker.C:
			p.pollOnce(ctx)
		}
	}
}

// Suspend 暂停轮询（功能 10：后台页面失焦时调用）。
// 幂等；暂停后循环不再采集也不向 CPE 发请求。
func (p *Poller) Suspend() {
	p.mu.Lock()
	if !p.suspended {
		p.suspended = true
		p.wake()
	}
	p.mu.Unlock()
}

// Resume 恢复轮询（功能 10：前台页面聚焦时调用）。
// 幂等；恢复后立即触发一次采集（唤醒循环）。
func (p *Poller) Resume() {
	p.mu.Lock()
	if p.suspended {
		p.suspended = false
		p.wake()
	}
	p.mu.Unlock()
}

// IsSuspended 返回是否处于暂停状态。
func (p *Poller) IsSuspended() bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.suspended
}

// wake 非阻塞发出状态变化通知（缓冲 1 足够：只有循环在消费）。
// 调用方必须持有 p.mu。
func (p *Poller) wake() {
	select {
	case p.notify <- struct{}{}:
	default:
	}
}

// isSuspended 不加锁读暂停标志（仅 Start 循环内调用）。
func (p *Poller) isSuspended() bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.suspended
}

// Stop 请求停止轮询循环（配合 Start 的 ctx 使用，二选一）。
func (p *Poller) Stop() {
	select {
	case <-p.stop:
	default:
		close(p.stop)
	}
}

// Done 返回轮询循环退出信号（Stop/cancel 生效且循环返回后关闭）。
func (p *Poller) Done() <-chan struct{} {
	return p.done
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
	// 8. 功能 1：net/cell-info —— 服务小区 RF 详情（ARFCN/带宽/CQI/CellID）
	if snap.Caps.CellInfo {
		if ci, err := client.Net.CellInfo(); err == nil {
			p.fillFromCellInfo(&snap, ci)
		} else if device.Classify(err) != device.KindUnsupported {
			p.handlePollErr("cell-info", err, &snap)
		}
	}
	// 9. 功能 3：device/seccellinfo —— 载波聚合辅小区（5G CPE 专有）
	if snap.Caps.CarrierAgg {
		if sc, err := client.Device.SecCellInfo(); err == nil {
			p.fillFromSecCell(&snap, sc)
		} else if device.Classify(err) != device.KindUnsupported {
			p.handlePollErr("seccellinfo", err, &snap)
		}
	}
	// 10. 功能 3：device/nbrcellinfo —— 邻小区（5G CPE 专有）
	if snap.Caps.Neighbor {
		if nb, err := client.Device.NbrCellInfo(); err == nil {
			p.fillFromNbrCell(&snap, nb)
		} else if device.Classify(err) != device.KindUnsupported {
			p.handlePollErr("nbrcellinfo", err, &snap)
		}
	}
	// 11. 功能 5：api/ntwk/celllock —— 当前锁频参数
	if snap.Caps.Lock {
		if lk, err := client.Ntwk.Celllock(); err == nil {
			p.fillFromCelllock(&snap, lk)
		} else if device.Classify(err) != device.KindUnsupported {
			p.handlePollErr("celllock", err, &snap)
		}
	}
	// 12. 功能 6：api/dialup/mobile-dataswitch —— 流量开关
	if snap.Caps.DataSwitch {
		if ds, err := client.DialUp.MobileDataswitch(); err == nil {
			p.fillFromDataSwitch(&snap, ds)
		} else if device.Classify(err) != device.KindUnsupported {
			p.handlePollErr("mobile-dataswitch", err, &snap)
		}
	}
	// 13. 功能 4：api/log/loginfo —— 系统操作日志尾部（有界）
	if snap.Caps.Log {
		if lg, err := client.Log.Loginfo(); err == nil {
			snap.Log = parseLogInfo(lg)
		} else if device.Classify(err) != device.KindUnsupported {
			p.handlePollErr("loginfo", err, &snap)
		}
	}

	p.calcRate(&snap)

	p.mu.Lock()
	p.last = snap
	p.mu.Unlock()

	if p.sink != nil {
		p.sink.PutSnapshot(p.dev.ID(), snap)
	}
	p.dev.SetOnline(snap.Online)
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
	// 功能 2：AMBR / QCI / 速度限制（monitoring/status 直出，键名见 docs/01-sdk-analysis.md）
	if v, ok := intp(m, "mQos"); ok {
		snap.QoS.QCI = v
	} else if v, ok := intp(m, "QosPriority"); ok {
		snap.QoS.QCI = v
	}
	if v, ok := int64p(m, "DownlinkAmbr"); ok {
		snap.QoS.DlAmbr = v
	} else if v, ok := int64p(m, "DlAmbr"); ok {
		snap.QoS.DlAmbr = v
	}
	if v, ok := int64p(m, "UplinkAmbr"); ok {
		snap.QoS.UlAmbr = v
	} else if v, ok := int64p(m, "UlAmbr"); ok {
		snap.QoS.UlAmbr = v
	}
	// 速度限制（部分固件 net/network 或 status 携带 MaxDownlinkSpeed/MaxUplinkSpeed）
	if v, ok := int64p(m, "MaxDownlinkSpeed"); ok {
		snap.QoS.MaxDlSpeed = v
	}
	if v, ok := int64p(m, "MaxUplinkSpeed"); ok {
		snap.QoS.MaxUlSpeed = v
	}
	snap.QoS.SpeedLimit = snap.QoS.MaxDlSpeed > 0 || snap.QoS.MaxUlSpeed > 0
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
	// 部分固件的速度限制键在 net/network 而非 monitoring/status
	if v, ok := int64p(m, "MaxDownlinkSpeed"); ok {
		snap.QoS.MaxDlSpeed = v
	}
	if v, ok := int64p(m, "MaxUplinkSpeed"); ok {
		snap.QoS.MaxUlSpeed = v
	}
	snap.QoS.SpeedLimit = snap.QoS.MaxDlSpeed > 0 || snap.QoS.MaxUlSpeed > 0
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

// fillFromCellInfo 解析 net/cell-info（功能 1：ARFCN/带宽/CQI/CellID + 功能 2 QCI）。
func (p *Poller) fillFromCellInfo(snap *Snapshot, m map[string]any) {
	fillCellFromInfo(&snap.Cell, m)
	// 若 cell-info 带 pci，回填到 SignalState（部分型号 signal 端点无 pci）
	if snap.Signal.PCI == 0 && snap.Cell.PCI != 0 {
		snap.Signal.PCI = snap.Cell.PCI
	}
	// 部分固件 cell-info 也带 QCI（qci 键）
	if v, ok := intp(m, "qci"); ok && snap.QoS.QCI == 0 {
		snap.QoS.QCI = v
	}
}

// fillFromSecCell 解析 device/seccellinfo（功能 3：载波聚合辅小区）。
func (p *Poller) fillFromSecCell(snap *Snapshot, m map[string]any) {
	// 优先 5G 列表；回落 4G 列表
	v := findNestedList(m, "nrseccell_list", "nrseccelllist")
	if v == "" {
		v = findNestedList(m, "lteseccell_list", "lteseccelllist")
	}
	snap.Carrier = parseCellList(v)
	if len(snap.Carrier) == 0 {
		snap.Carrier = nil
		return
	}
	// cell_id 可能作为独立键存在（部分固件在 seccellinfo 根上）
	if cid, ok := int64p(m, "cell_id"); ok && cid != 0 {
		for i := range snap.Carrier {
			if snap.Carrier[i].CellID == 0 {
				snap.Carrier[i].CellID = cid
			}
		}
	}
}

// fillFromNbrCell 解析 device/nbrcellinfo（功能 3：邻小区）。
func (p *Poller) fillFromNbrCell(snap *Snapshot, m map[string]any) {
	v := findNestedList(m, "nbrcell_nrlist", "nbrcellnrlist")
	if v == "" {
		v = findNestedList(m, "nbrcell_ltelist", "nbrcellltelist")
	}
	snap.Neighbor = parseCellList(v)
	if len(snap.Neighbor) == 0 {
		snap.Neighbor = nil
	}
}

// fillFromCelllock 解析 api/ntwk/celllock（功能 5：当前锁频参数）。
func (p *Poller) fillFromCelllock(snap *Snapshot, m map[string]any) {
	snap.Lock.Lock = intpOr(m, "Lock", 0)
	snap.Lock.Freq = strOr(m, "Freq", "")
	snap.Lock.PCI = intpOr(m, "PCI", 0)
	snap.Lock.MaxFreq = strOr(m, "MaxFreq", "")
}

// fillFromDataSwitch 解析 api/dialup/mobile-dataswitch（功能 6：流量开关）。
// 成功访问但字段缺失 → -1（未知），前端隐藏状态而非误显示为关。
func (p *Poller) fillFromDataSwitch(snap *Snapshot, m map[string]any) {
	if v, ok := intp(m, "dataswitch"); ok {
		snap.Data.DataSwitch = v
	} else {
		snap.Data.DataSwitch = -1
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
	case "cell-info":
		snap.Caps.CellInfo = false
	case "seccellinfo":
		snap.Caps.CarrierAgg = false
	case "nbrcellinfo":
		snap.Caps.Neighbor = false
	case "celllock":
		snap.Caps.Lock = false
	case "mobile-dataswitch":
		snap.Caps.DataSwitch = false
	case "loginfo":
		snap.Caps.Log = false
	}
}

// markError 在不可连接时把快照标记为离线（保留上次数据但 Online=false）。
func (p *Poller) markError() {
	p.mu.Lock()
	p.last.Online = false
	p.last.HasError = true
	p.last.At = time.Now()
	snap := p.last
	p.mu.Unlock()

	p.dev.SetOnline(false)
	if p.sink != nil {
		p.sink.PutSnapshot(p.dev.ID(), snap)
	}
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
