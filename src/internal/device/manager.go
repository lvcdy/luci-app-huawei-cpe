package device

import (
	"errors"
	"fmt"
	"sort"
	"sync"

	"github.com/lvcdy/huawei-lte-api-go/session"

	"huawei-cpe/internal/config"
)

// 错误分类结果——供 poller / netmon / sms 使用，决定退避策略。
type ErrKind int

const (
	// KindPermanent 永久性错误：凭据无效等，不应再重试。
	KindPermanent ErrKind = iota
	// KindAuthExpired 会话过期：需要重新登录后重试一次。
	KindAuthExpired
	// KindUnsupported 设备不支持该 API：记入能力矩阵，不再调用。
	KindUnsupported
	// KindBusy 系统忙：短暂退避后重试。
	KindBusy
	// KindOffline 网络不可达/超时：设备离线，指数退避。
	KindOffline
	// KindUnknown 其它错误。
	KindUnknown
)

// ClassifiedError 是带分类的错误，错误信息绝不包含凭据。
type ClassifiedError struct {
	Kind   ErrKind
	Err    error
	Device string
}

func (e *ClassifiedError) Error() string { return e.Err.Error() }
func (e *ClassifiedError) Unwrap() error { return e.Err }

// Classify 将任意 SDK 错误映射为 ErrKind。
// 永不把敏感细节带进错误文本 —— 只在日志按 Kind 记录。
func Classify(err error) ErrKind {
	switch {
	case err == nil:
		return 0
	case isPermanentAuth(err):
		return KindPermanent
	case isAuthExpired(err):
		return KindAuthExpired
	case isUnsupported(err):
		return KindUnsupported
	case isBusy(err):
		return KindBusy
	default:
		return KindOffline
	}
}

// classifyConnectErr 是 connect 阶段错误的分类（登录失败也是 connect 的一部分）。
func classifyConnectErr(err error) error {
	return &ClassifiedError{Kind: Classify(err), Err: err, Device: "connect"}
}

// isPermanentAuth 判断是否永久凭据错误（登录用户名/密码错误）。
func isPermanentAuth(err error) bool {
	var le *session.LoginInvalidCredentialsError
	return errors.As(err, &le)
}

// isAuthExpired 判断是否会话过期需重登。
func isAuthExpired(err error) bool {
	var le *session.LoginRequiredError
	if errors.As(err, &le) {
		return true
	}
	var wt *session.WrongSessionTokenError
	return errors.As(err, &wt)
}

// isUnsupported 判断设备是否不支持某 API。
func isUnsupported(err error) bool {
	var ne *session.NotSupportedError
	return errors.As(err, &ne)
}

// isBusy 判断是否系统繁忙（临时）。
func isBusy(err error) bool {
	var be *session.SystemBusyError
	return errors.As(err, &be)
}

// ---- Manager 多设备生命周期 ----

// Manager 持有全部已配置设备，提供统一的连接/关闭/事件分发生命周期。
// 每设备独立管理，失败互不影响。
type Manager struct {
	mu      sync.Mutex
	log     Logger
	devices map[string]*Device
	cfg     []config.CPE
}

// NewManager 按配置构造设备管理器。不发起网络连接。
func NewManager(log Logger, cfgs []config.CPE) *Manager {
	m := &Manager{log: log, devices: map[string]*Device{}, cfg: cfgs}
	for i := range cfgs {
		c := cfgs[i]
		if c.ID == "" {
			c.ID = fmt.Sprintf("cpe-%d", i)
		}
		d := New(log, c)
		m.devices[c.ID] = d
	}
	return m
}

// Get 返回指定 ID 的设备；不存在返回 nil。
func (m *Manager) Get(id string) *Device {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.devices[id]
}

// All 返回全部设备（有序）。
func (m *Manager) All() []*Device {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]*Device, 0, len(m.devices))
	for _, d := range m.devices {
		out = append(out, d)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID() < out[j].ID() })
	return out
}

// Enabled 返回已启用设备列表。
func (m *Manager) Enabled() []*Device {
	out := m.All()
	var enabled []*Device
	for _, d := range out {
		if d.cfg.Enabled {
			enabled = append(enabled, d)
		}
	}
	return enabled
}

// Close 关闭全部设备并清空列表（幂等）。
func (m *Manager) Close() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, d := range m.devices {
		_ = d.Close()
	}
	m.devices = map[string]*Device{}
}

// Update 用新配置重建设备集（reload 时调用）。已存在的设备保留在线状态与连接。
// 返回变化简述（不含凭据）。
func (m *Manager) Update(cfgs []config.CPE) []string {
	m.mu.Lock()
	defer m.mu.Unlock()

	var changes []string
	seen := map[string]bool{}

	// 更新/新增
	for i := range cfgs {
		c := cfgs[i]
		if exist, ok := m.devices[c.ID]; ok {
			// 已有 —— 仅 Host/Enabled 变化判定；凭据不比较、不打印。
			if exist.cfg.Host != c.Host {
				changes = append(changes, fmt.Sprintf("cpe[%s] host changed", c.ID))
				_ = exist.Close()
				exist.cfg = c
				m.devices[c.ID] = New(m.log, c)
			} else {
				exist.cfg = c
			}
			seen[c.ID] = true
			continue
		}
		// 新增
		d := New(m.log, c)
		m.devices[c.ID] = d
		changes = append(changes, fmt.Sprintf("cpe[%s] added", c.ID))
		seen[c.ID] = true
	}

	// 删除
	for id := range m.devices {
		if !seen[id] {
			_ = m.devices[id].Close()
			delete(m.devices, id)
			changes = append(changes, fmt.Sprintf("cpe[%s] removed", id))
		}
	}
	return changes
}

// ErrPermanent 表示设备处于永久错误状态（凭据无效），不应再重试。
var ErrPermanent = errors.New("device: permanent credential error")

// PermanentError 检查错误是否为永久性凭据错误。
func PermanentError(err error) bool {
	var ce *ClassifiedError
	return errors.As(err, &ce) && ce.Kind == KindPermanent
}