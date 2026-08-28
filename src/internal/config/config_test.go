package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeConfig 临时写一个 UCI 配置文件供测试。
func writeConfig(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "huawei_cpe")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return p
}

const sample = `# comment
config cpe 'main'
	option enabled '1'
	option name 'H168-383 主路由'
	option host '192.168.8.1'
	option username 'admin'
	option password 'secret123'
	option polling_interval '60'
	option sms_sync_interval '30'

config cpe 'second'
	option enabled '0'
	option name 'H151-383'
	option host '192.168.8.2'

config netmon 'health'
	option enabled '1'
	option ping_host '223.5.5.5'
	option ping_port '53'

config recovery 'recovery'
	option enabled '0'
	option max_attempts '3'
	option cooldown_minutes '30'

config notify 'telegram'
	option enabled '1'
	option api_token '123456:ABC-DEF'
	option chat_ids '111,222'

config notify 'webhook'
	option enabled '0'
	option url 'https://hooks.example.com/aaa'

config rules 'sms_received'
	option telegram '1'
	option email '0'
	option webhook '0'
	option filter_mode 'whitelist'
	option whitelist '13800000000'
	option sms_forward_secret '1'
`

func TestLoadFull(t *testing.T) {
	cfg, err := Load(writeConfig(t, sample))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(cfg.CPEs) != 2 {
		t.Fatalf("want 2 cpes, got %d", len(cfg.CPEs))
	}
	m := cfg.CPEs[0]
	if !m.Enabled || m.Name != "H168-383 主路由" || m.Host != "192.168.8.1" ||
		m.Username != "admin" || m.Password != "secret123" {
		t.Errorf("cpe main parsed wrong: %+v", m)
	}
	if m.PollingInterval != 60 || m.SMSSyncInterval != 30 {
		t.Errorf("cpe intervals wrong: %+v", m)
	}

	if !cfg.Netmon.Enabled || cfg.Netmon.PingHost != "223.5.5.5" || cfg.Netmon.PingPort != 53 {
		t.Errorf("netmon wrong: %+v", cfg.Netmon)
	}
	if cfg.Recovery.Enabled || cfg.Recovery.MaxAttempts != 3 || cfg.Recovery.CooldownMinutes != 30 {
		t.Errorf("recovery wrong: %+v", cfg.Recovery)
	}
	if !cfg.Notify.Telegram.Enabled || cfg.Notify.Telegram.APIToken != "123456:ABC-DEF" {
		t.Errorf("telegram wrong: %+v", cfg.Notify.Telegram)
	}
	if cfg.Notify.Webhook.Enabled || cfg.Notify.Webhook.URL != "https://hooks.example.com/aaa" {
		t.Errorf("webhook wrong: %+v", cfg.Notify.Webhook)
	}
	if !cfg.Rules.Telegram || cfg.Rules.FilterMode != "whitelist" ||
		cfg.Rules.Whitelist != "13800000000" || !cfg.Rules.SMSForwardSecret {
		t.Errorf("rules wrong: %+v", cfg.Rules)
	}
}

func TestLoadMissingFile(t *testing.T) {
	if _, err := Load(filepath.Join(t.TempDir(), "nope")); err == nil {
		t.Fatal("expected error for missing file")
	}
}

func TestDefaults(t *testing.T) {
	cfg, err := Load(writeConfig(t, "config cpe 'main'\n"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	c := cfg.CPEs[0]
	if c.PollingInterval != 60 || c.SMSSyncInterval != 30 {
		t.Errorf("defaults wrong: %+v", c)
	}
	if cfg.Netmon.PingHost != "223.5.5.5" || cfg.Netmon.PingPort != 53 {
		t.Errorf("netmon defaults wrong: %+v", cfg.Netmon)
	}
	if cfg.Recovery.MaxAttempts != 3 || cfg.Recovery.CooldownMinutes != 30 {
		t.Errorf("recovery defaults wrong: %+v", cfg.Recovery)
	}
}

// TestRedactedNoSecrets 验证脱敏摘要绝不包含密钥值。
func TestRedactedNoSecrets(t *testing.T) {
	cfg, err := Load(writeConfig(t, `config cpe 'main'
	option password 'topsecret-pw'
	option host '192.168.8.1'
config notify 'telegram'
	option api_token '123456:TG-SECRET'
config notify 'smtp'
	option password 'smtp-secret'
`))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	r := cfg.Redacted()
	for _, secret := range []string{"topsecret-pw", "123456", "TG-SECRET", "smtp-secret"} {
		if strings.Contains(r, secret) {
			t.Errorf("Redacted() leaked secret %q: %s", secret, r)
		}
	}
	if !strings.Contains(r, "pass=***") {
		t.Errorf("Redacted() should mask password: %s", r)
	}
}

func TestQuotedAndList(t *testing.T) {
	content := `config cpe 'main'
	option name 'Huawei'
	list allow 'a'
	list allow 'b'
`
	cfg, err := Load(writeConfig(t, content))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.CPEs[0].Name != "Huawei" {
		t.Errorf("name wrong: %q", cfg.CPEs[0].Name)
	}
}
