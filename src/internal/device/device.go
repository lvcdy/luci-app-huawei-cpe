// Package device 管理 Huawei CPE 设备实例：SDK Connection 的创建、持有与重连，
// 以及设备凭据、在线状态的内存态维护。
//
// 职责边界：
//   - 凭据（Username/Password）仅存在于进程内存，绝不进入日志 / API 返回 / 错误链；
//   - 依赖 github.com/lvcdy/huawei-lte-api-go 作为唯一通信层，这里不重实现 HiLink 协议；
//   - 登录失效（LoginRequired）触发重登，永久凭据错误（LoginInvalidCredentials）停止重试。
package device

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"

	huaweilteapi "github.com/lvcdy/huawei-lte-api-go"
	"github.com/lvcdy/huawei-lte-api-go/session"

	"huawei-cpe/internal/config"
)

// DefaultHost 是 UCI 未配置 host 时的缺省值（华为 CPE 通常 192.168.8.1）。
const DefaultHost = "192.168.8.1"

// requestTimeout 是每个 API 请求层的超时上限，防止慢 CPE 挂死轮询循环。
const requestTimeout = 5 * time.Second

// Device 是一个 CPE 设备实例。
//
// 并发安全：所有字段通过 Device 上的方法读写。
// 凭据（Username/Password）只允许在设备初始化时设置一次，之后不被任何路径复制出去。
type Device struct {
	mu       sync.RWMutex
	log      Logger
	cfg      config.CPE          // 快照；凭据只写一次
	host     string              // 规范化 host（去 scheme/端口）
	conn     *session.Connection // SDK 连接（nil = 未连接/已关闭）
	client   *huaweilteapi.Client
	online   bool      // 最近一次 CPE 可达且认证成功
	lastSeen time.Time // 最近成功交互时间
	closed   bool

	// leaseMu 串行化租约期内对 SDK client 的访问。
	// SDK session 无内置并发保护；poller（状态采集）与 SMS 同步器等
	// 多循环共享同一连接时依赖此锁互斥（架构 §6：单设备串行请求）。
	leaseMu sync.Mutex
}

// Logger 是 device 包依赖的最小日志接口，由 app 包注入实现。
type Logger interface {
	Debug(msg string, args ...any)
	Info(msg string, args ...any)
	Warn(msg string, args ...any)
	Error(msg string, args ...any)
}

// New 构造一个设备实例。不发起网络连接（连接在第一次 Lease 时惰性建立）。
func New(log Logger, cfg config.CPE) *Device {
	if cfg.Host == "" {
		cfg.Host = DefaultHost
	}
	if cfg.Username == "" {
		cfg.Username = "admin"
	}
	return &Device{
		log:  log,
		cfg:  cfg,
		host: normalizeHost(cfg.Host),
	}
}

// normalizeHost 规范化 host：剥去 scheme、路径与空白。仅用于展示/去重。
func normalizeHost(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.Index(s, "://"); i >= 0 {
		s = s[i+3:]
	}
	if i := strings.IndexByte(s, '/'); i >= 0 {
		s = s[:i]
	}
	return strings.TrimSuffix(s, "/")
}

// ID 返回设备唯一标识（UCI section 名）。
func (d *Device) ID() string { return d.cfg.ID }

// Name 返回显示名（缺省回落为 ID）。
func (d *Device) Name() string {
	if d.cfg.Name != "" {
		return d.cfg.Name
	}
	return d.cfg.ID
}

// Host 返回规范化 host。
func (d *Device) Host() string { return d.host }

// CredentialsAreSet 报告是否配置了用户名/密码（用于 UI 提示缺凭据）。
func (d *Device) CredentialsAreSet() bool {
	return d.cfg.Username != "" && d.cfg.Password != ""
}

// PollingInterval 返回配置的轮询间隔时长；未配置返回 0（上层回落默认值）。
// 注意：只暴露时长，不暴露含凭据的完整配置。
func (d *Device) PollingInterval() time.Duration {
	if d.cfg.PollingInterval <= 0 {
		return 0
	}
	return time.Duration(d.cfg.PollingInterval) * time.Second
}

// SMSSyncInterval 返回配置的短信同步间隔时长；未配置返回 0（上层回落默认值，
// 见 internal/sms 的 deFaultSyncInterval）。
func (d *Device) SMSSyncInterval() time.Duration {
	if d.cfg.SMSSyncInterval <= 0 {
		return 0
	}
	return time.Duration(d.cfg.SMSSyncInterval) * time.Second
}

// Snapshot 返回设备状态的内存快照（不含凭据，供 API/页面展示）。
func (d *Device) Snapshot() Snapshot {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return Snapshot{
		ID:       d.cfg.ID,
		Name:     d.Name(),
		Host:     d.host,
		Enabled:  d.cfg.Enabled,
		Online:   d.online,
		HasCreds: d.CredentialsAreSet(),
		LastSeen: d.lastSeen,
	}
}

// Snapshot 是设备展示状态（绝不包含凭据字段）。
type Snapshot struct {
	ID       string    `json:"id"`
	Name     string    `json:"name"`
	Host     string    `json:"host"`
	Enabled  bool      `json:"enabled"`
	Online   bool      `json:"online"`
	HasCreds bool      `json:"has_creds"`
	LastSeen time.Time `json:"last_seen,omitempty"`
}

// Lease 建立（或复用）SDK 连接，返回可用客户端与释放函数。
// 释放后连接保持打开，供后续租约复用；设备 Close 时统一关闭。
//
// 并发语义：租约期（Lease 返回 → release 调用）内独占该设备连接——
// 多采集循环（poller / SMS 同步器）不得并发调用同一 SDK client
// （SDK session 无内置锁）。release 既释放租约也解锁连接。
// 返回的错误可能携带 Kind 分类（凭据错误 = KindPermanent）。
func (d *Device) Lease(ctx context.Context) (*huaweilteapi.Client, func(), error) {
	d.leaseMu.Lock()
	client, err := d.connect(ctx)

	// 加锁失败（closed）或连接失败：不独占锁，立即解锁返回。
	if err != nil {
		d.leaseMu.Unlock()
		return nil, nil, classifyConnectErr(err)
	}
	return client, d.leaseMu.Unlock, nil
}

// connect 建立 SDK 连接并登录（幂等：已有连接直接返回）。
func (d *Device) connect(ctx context.Context) (*huaweilteapi.Client, error) {
	d.mu.RLock()
	if d.client != nil && d.conn != nil && !d.closed {
		client := d.client
		d.mu.RUnlock()
		return client, nil
	}
	closed := d.closed
	d.mu.RUnlock()
	if closed {
		return nil, errClosed
	}

	// 连接 + 登录在一个有界超时内完成（SDK 请求自身带 Timeout）。
	httpClient := &http.Client{Timeout: requestTimeout}
	raw := "http://" + d.cfg.Host
	conn, err := session.NewConnection(raw, d.cfg.Username, d.cfg.Password, requestTimeout, httpClient)
	if err != nil {
		// 不把凭据带入错误信息；仅按分类记录
		return nil, err
	}

	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed {
		conn.Close()
		return nil, errClosed
	}
	d.conn = conn
	d.client = huaweilteapi.NewClient(conn)
	d.online = true
	d.lastSeen = time.Now()
	return d.client, nil
}

// Relogin 强制重建会话：重置旧连接后重新创建（登录失效后由上层触发重试）。
// 与 Close 不同，Relogin 不改变设备的已关闭状态，设备仍可继续使用。
// 与 Lease 一样，返回的释放函数在租约结束后解锁连接（防止 resetConn
// 打断其他循环正在进行的请求）。
func (d *Device) Relogin(ctx context.Context) (*huaweilteapi.Client, func(), error) {
	d.leaseMu.Lock()
	d.resetConn()
	client, err := d.connect(ctx)
	if err != nil {
		d.leaseMu.Unlock()
		return nil, nil, classifyConnectErr(err)
	}
	return client, d.leaseMu.Unlock, nil
}

// resetConn 关闭并清空当前连接（不改变 closed 状态）。
func (d *Device) resetConn() {
	d.mu.Lock()
	conn := d.conn
	d.conn = nil
	d.client = nil
	d.online = false
	d.mu.Unlock()

	if conn != nil {
		conn.Close()
	}
}

// Close 关闭设备连接并释放所有资源（幂等）。
func (d *Device) Close() error {
	d.mu.Lock()
	if d.closed {
		d.mu.Unlock()
		return nil
	}
	d.closed = true
	conn := d.conn
	d.conn = nil
	d.client = nil
	d.online = false
	d.mu.Unlock()

	if conn != nil {
		conn.Close()
	}
	return nil
}

// SetOnline 更新在线状态并记录 LastSeen（供外部健康探测回调）。
func (d *Device) SetOnline(b bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.online = b
	if b {
		d.lastSeen = time.Now()
	}
}

// IsOnline 报告设备连接是否建立（SDK 连接存在且未关闭）。
func (d *Device) IsOnline() bool {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.online
}

// LastSeen 返回最近成功交互时间。
func (d *Device) LastSeen() time.Time {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.lastSeen
}

// errClosed 表示设备已关闭（携带 KindUnknown 分类，供上层按分类退避）。
var errClosed = &ClassifiedError{Kind: KindUnknown, Err: errDeviceClosed}

// errDeviceClosed 是底层 sentinel。
var errDeviceClosed = &closedError{}

type closedError struct{}

func (e *closedError) Error() string { return "device: closed" }

func (e *closedError) Is(target error) bool {
	_, ok := target.(*closedError)
	return ok
}

// IsClosedError 报告错误是否为"设备已关闭"。
func IsClosedError(err error) bool {
	return errors.Is(err, errDeviceClosed)
}
