package device

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"huawei-cpe/internal/config"
	"huawei-cpe/internal/testutil"
)

// ---- mock CPE server（共享实现在 internal/testutil）----
// 握手：GET / 返回含 csrf_token 的 HTML
//      GET api/user/state-login → <State>-1</State>（未登录）
//      POST api/user/login       → "OK"（校验用户名/密码）
// 之后任意 api 请求都校验 CSRF token（绑定 Cookie 的 SessionID）。

type mockCPE struct{ *testutil.MockCPE }

func newMockCPE(t *testing.T, username string) *mockCPE {
	return &mockCPE{testutil.NewMockCPE(username)}
}

func (m *mockCPE) loginFailNext()          { m.LoginFailNext() }
func (m *mockCPE) setStateLoggedIn(v bool) { m.SetLoggedIn(v) }
func (m *mockCPE) logins() int32           { return m.LoginCount() }
func (m *mockCPE) Handler() http.Handler   { return m.MockCPE }

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
	before := m.logins()
	_, release2, err := d.Lease(ctx)
	if err != nil {
		t.Fatalf("second Lease failed: %v", err)
	}
	release2()
	if got := m.logins() - before; got != 0 {
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
	_, release1, err := d.Lease(ctx)
	if err != nil {
		t.Fatalf("initial lease failed: %v", err)
	}
	release1()
	before := m.logins()

	// 会话在设备端过期
	m.setStateLoggedIn(false)

	_, release2, err := d.Relogin(ctx)
	if err != nil {
		t.Fatalf("relogin failed: %v", err)
	}
	release2()
	if got := m.logins() - before; got == 0 {
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
