package device

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"huawei-cpe/internal/config"
)

// ---- mock CPE server（华为 HiLink HTTP 协议）----
// 握手：GET / 返回含 csrf_token 的 HTML
//      GET api/user/state-login → <State>-1</State>（未登录）
//      POST api/user/login       → "OK"（校验用户名/密码）
// 之后任意 api 请求都校验 CSRF token（绑定 Cookie 的 SessionID）。

type mockCPE struct {
	t        *testing.T
	username string
	token    string

	loginFailOnce *atomic.Bool // 下一次 user/login 返回密码错误
	loginCount    *atomic.Int32

	stateLoggedIn *atomic.Bool // state-login 报已登录
}

func newMockCPE(t *testing.T, username string) *mockCPE {
	return &mockCPE{
		t:             t,
		username:      username,
		token:         "test-csrf-token-abcdef123456",
		loginFailOnce: &atomic.Bool{},
		loginCount:    &atomic.Int32{},
		stateLoggedIn: &atomic.Bool{},
	}
}

// loginFailNext 让下一次 user/login 返回密码错误（108002）。
func (m *mockCPE) loginFailNext() { m.loginFailOnce.Store(true) }

func (m *mockCPE) setStateLoggedIn(v bool) { m.stateLoggedIn.Store(v) }

// xmlResp 写 XML 响应。
func (m *mockCPE) xmlResp(w http.ResponseWriter, root string, body map[string]string) {
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

func xmlEscape(s string) string {
	r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;")
	return r.Replace(s)
}

// errResp 写带 error code 的响应。
func (m *mockCPE) errResp(w http.ResponseWriter, code int) {
	w.Header().Set("Content-Type", "text/xml")
	w.WriteHeader(200)
	body := fmt.Sprintf("<?xml version=\"1.0\" encoding=\"UTF-8\"?>\n<error><code>%d</code><message></message></error>", code)
	_, _ = w.Write([]byte(body))
}

// Handler 返回 mock 的 http.Handler。
func (m *mockCPE) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		if path == "" || path == "/" {
			w.Header().Set("Content-Type", "text/html")
			_, _ = fmt.Fprintf(w, `<html><head><meta name="csrf_token" content="%s"></head><body>Huawei CPE</body></html>`, m.token)
			return
		}
		if !strings.HasPrefix(path, "/api/") {
			m.errResp(w, 125001)
			return
		}
		ep := strings.TrimPrefix(path, "/api/")

		// 校验 CSRF token（SDK 在请求头携带 __RequestVerificationToken）
		reqToken := r.Header.Get("__RequestVerificationToken")
		if reqToken != m.token {
			m.errResp(w, 125003) // Wrong Session Token
			return
		}

		switch ep {
		case "user/state-login":
			state := "-1"
			if m.stateLoggedIn.Load() {
				state = "0"
			}
			m.xmlResp(w, "response", map[string]string{
				"State":         state,
				"password_type": "4",
			})
		case "user/login":
			m.loginCount.Add(1)
			if m.loginFailOnce.Load() {
				m.loginFailOnce.Store(false)
				m.errResp(w, 108002) // Password wrong
				return
			}
			var req struct {
				Username     string `xml:"Username"`
				Password     string `xml:"Password"`
				PasswordType string `xml:"password_type"`
			}
			_ = xml.NewDecoder(r.Body).Decode(&req)
			// SDK 对密码做 SHA256+CSRF 混淆，mock 无法逆推明文；
			// 以"用户名匹配 + 密码非空"作为成功标准。
			if req.Username != m.username || req.Password == "" {
				m.errResp(w, 108001) // Username wrong
				return
			}
			m.stateLoggedIn.Store(true)
			// SDK 期望登录响应顶层就是 "OK"（<response>OK</response>）
			w.Header().Set("Content-Type", "text/xml")
			w.WriteHeader(200)
			_, _ = w.Write([]byte(`<?xml version="1.0" encoding="UTF-8"?><response>OK</response>`))
		default:
			m.errResp(w, 125001)
		}
	})
}

// ---- test logger ----

type testLogger struct {
	t *testing.T
}

func (l testLogger) Debug(msg string, args ...any) { l.t.Logf("[debug] "+msg, args...) }
func (l testLogger) Info(msg string, args ...any)  { l.t.Logf("[info] "+msg, args...) }
func (l testLogger) Warn(msg string, args ...any)  { l.t.Logf("[warn] "+msg, args...) }
func (l testLogger) Error(msg string, args ...any) { l.t.Logf("[error] "+msg, args...) }

// ---- tests ----

func TestDeviceConnectAndLease(t *testing.T) {
	m := newMockCPE(t, "admin")
	srv := httptest.NewServer(m.Handler())
	defer srv.Close()

	host := strings.TrimPrefix(srv.URL, "http://")
	d := New(testLogger{t}, config.CPE{
		ID:       "r1",
		Name:     "Router",
		Enabled:  true,
		Host:     host,
		Username: "admin",
		Password: "secret",
	})

	ctx := context.Background()
	client, release, err := d.Lease(ctx)
	if err != nil {
		t.Fatalf("Lease failed: %v", err)
	}
	if client == nil {
		t.Fatal("client is nil")
	}
	release()

	// 再次租约应复用连接（不重新登录）
	before := m.loginCount.Load()
	_, _, err = d.Lease(ctx)
	if err != nil {
		t.Fatalf("second Lease failed: %v", err)
	}
	if got := m.loginCount.Load() - before; got != 0 {
		t.Fatalf("expected connection reuse (0 new logins), got %d", got)
	}

	if !d.IsOnline() {
		t.Fatal("device should be online after successful connect")
	}
	_ = d.Close()
	if d.IsOnline() {
		t.Fatal("device should be offline after Close")
	}
}

func TestDeviceWrongPasswordIsPermanent(t *testing.T) {
	m := newMockCPE(t, "admin")
	srv := httptest.NewServer(m.Handler())
	defer srv.Close()

	host := strings.TrimPrefix(srv.URL, "http://")
	d := New(testLogger{t}, config.CPE{
		ID:       "r1",
		Host:     host,
		Username: "admin",
		Password: "whatever",
	})

	// 让 mock 登录返回密码错误（108002→KindPermanent）
	m.loginFailNext()
	_, _, err := d.Lease(context.Background())
	if err == nil {
		t.Fatal("expected error from Lease")
	}
	if !PermanentError(err) {
		t.Fatalf("expected permanent error, got: %v (kind=%v)", err, Classify(err))
	}

	_ = d.Close()
	_, _, err = d.Lease(context.Background())
	if err == nil || !IsClosedError(err) {
		t.Fatalf("expected closed error after Close, got: %v", err)
	}
	if PermanentError(err) {
		t.Fatal("closed error must not be permanent")
	}
}

// 会话过期（state-login 报未登录）后 Relogin 应重新完整握手。
func TestDeviceReloginDoesFullHandshake(t *testing.T) {
	m := newMockCPE(t, "admin")
	srv := httptest.NewServer(m.Handler())
	defer srv.Close()

	host := strings.TrimPrefix(srv.URL, "http://")
	d := New(testLogger{t}, config.CPE{
		ID:       "r1",
		Host:     host,
		Username: "admin",
		Password: "secret",
	})

	ctx := context.Background()
	_, _, err := d.Lease(ctx)
	if err != nil {
		t.Fatalf("initial lease failed: %v", err)
	}
	before := m.loginCount.Load()

	// 会话在设备端过期
	m.setStateLoggedIn(false)

	_, _, err = d.Relogin(ctx)
	if err != nil {
		t.Fatalf("relogin failed: %v", err)
	}
	if got := m.loginCount.Load() - before; got == 0 {
		t.Fatal("expected a new login after Relogin")
	}
}

func TestManagerLifecycle(t *testing.T) {
	m := newMockCPE(t, "admin")
	srv := httptest.NewServer(m.Handler())
	defer srv.Close()

	host := strings.TrimPrefix(srv.URL, "http://")
	cfgs := []config.CPE{
		{ID: "r1", Name: "Main", Enabled: true, Host: host, Username: "admin", Password: "secret"},
		{ID: "r2", Name: "Backup", Enabled: false, Host: "192.168.99.1", Username: "admin", Password: "x"},
	}
	mgr := NewManager(testLogger{t}, cfgs)
	defer mgr.Close()

	if got := len(mgr.All()); got != 2 {
		t.Fatalf("All() = %d, want 2", got)
	}
	if got := len(mgr.Enabled()); got != 1 {
		t.Fatalf("Enabled() = %d, want 1", got)
	}
	if got := mgr.Get("r2").Name(); got != "Backup" {
		t.Fatalf("Get(r2).Name() = %q, want Backup", got)
	}

	// 更新：删 r2、加 r3
	changes := mgr.Update([]config.CPE{
		{ID: "r1", Name: "Main-2", Enabled: true, Host: host, Username: "admin", Password: "secret"},
		{ID: "r3", Name: "New", Enabled: true, Host: "10.0.0.1", Username: "admin", Password: "z"},
	})
	joined := strings.Join(changes, ",")
	for _, want := range []string{"added", "removed"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("changes = %q, want contains %q", joined, want)
		}
	}
	if got := len(mgr.All()); got != 2 {
		t.Fatalf("After update All() = %d, want 2", got)
	}
	if mgr.Get("r2") != nil {
		t.Fatal("r2 should be removed")
	}
	if mgr.Get("r3") == nil {
		t.Fatal("r3 should be added")
	}
}

func TestSnapshotJSONHasNoPassword(t *testing.T) {
	m := newMockCPE(t, "admin")
	srv := httptest.NewServer(m.Handler())
	defer srv.Close()

	host := strings.TrimPrefix(srv.URL, "http://")
	d := New(testLogger{t}, config.CPE{
		ID: "r1", Name: "Router", Enabled: true, Host: host,
		Username: "admin", Password: "topsecret",
	})

	snap := d.Snapshot()
	b, err := json.Marshal(snap)
	if err != nil {
		t.Fatalf("marshal snapshot: %v", err)
	}
	js := string(b)
	for _, secret := range []string{"topsecret"} {
		if strings.Contains(js, secret) {
			t.Fatalf("snapshot JSON leaks %q: %s", secret, js)
		}
	}
}