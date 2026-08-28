// Package testutil 提供测试共享基建：模拟华为 CPE 的 HTTP mock。
//
// 行为契约（与 SDK huawei-lte-api-go 交互所需）：
//   - GET /              → HTML，含 <meta name="csrf_token">（SDK Reload 时解析）
//   - 每个 /api/ 响应头回传 __RequestVerificationToken（SDK 登录队列维护依赖）
//   - token 不匹配 → 125003（WrongSessionToken）
//   - user/state-login → State 反映登录态；user/login 校验用户名/密码非空
//   - 其它端点：返回 SetEndpoint 配置的 XML 快照；未配置返回 100002（NotSupported）
package testutil

import (
	"encoding/xml"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
)

// EndpointSnap 是一个端点返回的固定 XML 快照。
type EndpointSnap struct {
	Root string            // XML 根节点名（如 response/currentplmn）
	Body map[string]string // 键值对
}

// MockCPE 是模拟华为 CPE Web API 的 HTTP handler。
// 零值不可用；用 NewMockCPE 构造。
type MockCPE struct {
	Username string // 期望的登录用户名
	Token    string // CSRF token

	loginCount atomic.Int32
	loggedIn   atomic.Bool

	// loginFailOnce 使下一次 user/login 返回 108002（密码错误）。
	loginFailOnce atomic.Bool

	mu    sync.Mutex
	snaps map[string]EndpointSnap
}

// NewMockCPE 构造 mock。username 是期望的登录用户名；登录态初始为未登录。
func NewMockCPE(username string) *MockCPE {
	return &MockCPE{
		Username: username,
		Token:    "test-csrf-token-abcdef123456",
		snaps:    map[string]EndpointSnap{},
	}
}

// SetEndpoint 配置某端点（不含 /api/ 前缀）返回的固定 XML 快照。
func (m *MockCPE) SetEndpoint(ep string, root string, body map[string]string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.snaps[ep] = EndpointSnap{Root: root, Body: body}
}

// LoginFailNext 使下一次 user/login 返回密码错误（108002）。
func (m *MockCPE) LoginFailNext() { m.loginFailOnce.Store(true) }

// SetLoggedIn 设置 state-login 上报的登录态。
func (m *MockCPE) SetLoggedIn(v bool) { m.loggedIn.Store(v) }

// LoginCount 返回累计 user/login 成功请求次数。
func (m *MockCPE) LoginCount() int32 { return m.loginCount.Load() }

// ServeHTTP 实现 http.Handler。
func (m *MockCPE) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	if path == "" || path == "/" {
		w.Header().Set("Content-Type", "text/html")
		_, _ = fmt.Fprintf(w,
			`<html><head><meta name="csrf_token" content="%s"></head><body>Huawei CPE</body></html>`,
			m.Token)
		return
	}
	if !strings.HasPrefix(path, "/api/") {
		m.errResp(w, 125001)
		return
	}
	ep := strings.TrimPrefix(path, "/api/")

	// 真机行为：每个 API 响应头都回传 CSRF token，SDK 凭此维持登录后队列。
	w.Header().Set("__RequestVerificationToken", m.Token)

	if r.Header.Get("__RequestVerificationToken") != m.Token {
		m.errResp(w, 125003) // Wrong Session Token
		return
	}

	switch ep {
	case "user/state-login":
		state := "-1"
		if m.loggedIn.Load() {
			state = "0"
		}
		m.xmlResp(w, "response", map[string]string{
			"State":         state,
			"password_type": "4",
		})
	case "user/login":
		if m.loginFailOnce.Swap(false) {
			m.errResp(w, 108002) // Password wrong
			return
		}
		m.loginCount.Add(1)
		var req struct {
			Username     string `xml:"Username"`
			Password     string `xml:"Password"`
			PasswordType string `xml:"password_type"`
		}
		_ = xml.NewDecoder(r.Body).Decode(&req)
		// SDK 对密码做 SHA256+CSRF 混淆，mock 无法逆推明文；
		// 以"用户名匹配 + 密码非空"作为成功标准。
		if req.Username != m.Username || req.Password == "" {
			m.errResp(w, 108001) // Username wrong
			return
		}
		m.loggedIn.Store(true)
		w.Header().Set("Content-Type", "text/xml")
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`<?xml version="1.0" encoding="UTF-8"?><response>OK</response>`))
	default:
		m.mu.Lock()
		snap, ok := m.snaps[ep]
		m.mu.Unlock()
		if ok {
			m.xmlResp(w, snap.Root, snap.Body)
			return
		}
		// 未配置的端点：返回不支持（100002），用于能力矩阵测试
		m.errResp(w, 100002)
	}
}

// xmlResp 输出固定 XML 快照。
func (m *MockCPE) xmlResp(w http.ResponseWriter, root string, body map[string]string) {
	var b strings.Builder
	b.WriteString("<?xml version=\"1.0\" encoding=\"UTF-8\"?>\n")
	b.WriteString("<" + root + ">")
	for k, v := range body {
		b.WriteString("<" + k + ">" + xmlEscape(v) + "</" + k + ">")
	}
	b.WriteString("</" + root + ">")
	w.Header().Set("Content-Type", "text/xml")
	w.WriteHeader(200)
	_, _ = w.Write([]byte(b.String()))
}

// errResp 输出 error 响应（错误响应不回传新 token，与真机一致）。
func (m *MockCPE) errResp(w http.ResponseWriter, code int) {
	w.Header().Set("Content-Type", "text/xml")
	w.WriteHeader(200)
	body := fmt.Sprintf(
		"<?xml version=\"1.0\" encoding=\"UTF-8\"?>\n<error><code>%d</code><message></message></error>",
		code)
	_, _ = w.Write([]byte(body))
}

func xmlEscape(s string) string {
	r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;")
	return r.Replace(s)
}
