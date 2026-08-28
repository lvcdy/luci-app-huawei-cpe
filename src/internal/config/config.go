// Package config 解析 OpenWrt UCI 配置文件 /etc/config/huawei_cpe。
//
// 采用内置的轻量 UCI 解析器（纯 Go，无外部依赖），使其可跨平台单测，
// 并保证密钥字段绝不参与 String() 输出。
package config

import (
	"bufio"
	"fmt"
	"os"
	"strconv"
	"strings"
)

// Config 是 daemon 侧完整的运行配置（来自全部 UCI section）。
type Config struct {
	// CPEs 是配置的设备列表（config cpe ...）。
	CPEs []CPE
	// Netmon 是网络健康监控配置。
	Netmon Netmon
	// Recovery 是自动恢复配置（Phase 5）。
	Recovery Recovery
	// Notify 是通知渠道配置（Phase 4）。
	Notify Notify
	// Rules 是事件推送规则（Phase 4）。
	Rules Rules
	// History 是历史存储配置（SQLite 信号/流量趋势）。
	History History

	// Path 是配置文件路径（非 UCI 字段，仅记录来源）。
	Path string
}

// History 历史存储配置。DBPath 为空时历史功能整体禁用（降级运行）。
type History struct {
	DBPath        string // SQLite 文件路径（持久分区，勿放 /tmp）
	RetentionDays int    // 保留天数，默认 30
}

// CPE 对应一个 config cpe section。
// 敏感字段（Password）仅存在内存，禁止出现在日志 / API 返回 / String()。
type CPE struct {
	ID              string // section 名（main）
	Enabled         bool
	Name            string
	Host            string
	Username        string
	Password        string
	PollingInterval int // 秒
	SMSSyncInterval int // 秒
}

// Netmon 健康监控配置。
type Netmon struct {
	Enabled    bool
	PingHost   string
	PingPort   int
	CPETimeout int // 秒
}

// Recovery 自动恢复配置。
type Recovery struct {
	Enabled         bool
	MaxAttempts     int
	CooldownMinutes int
	RebootOnFail    bool
}

// Notify 通知渠道（Telegram / SMTP / Webhook）。
// Token 与密码类字段仅存内存。
type Notify struct {
	Telegram struct {
		Enabled  bool
		APIToken string
		ChatIDs  string // 逗号分隔
	}
	SMTP struct {
		Enabled  bool
		Host     string
		Port     int
		User     string
		Password string
		From     string
		To       string
		StartTLS bool
	}
	Webhook struct {
		Enabled bool
		URL     string
	}
}

// Rules 事件推送规则。
type Rules struct {
	Telegram         bool
	Email            bool
	Webhook          bool
	FilterMode       string // all | whitelist | keyword
	Whitelist        string
	Keywords         string
	SMSForwardSecret bool // 0=禁止转发短信内容 1=允许
}

// uciSection 是解析出的原始 section。
type uciSection struct {
	typ  string
	name string
	opts map[string][]string
}

// Load 读取并解析 UCI 文件。
func Load(path string) (*Config, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open config %s: %w", path, err)
	}
	defer f.Close()

	sections, err := parseUCI(f)
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}

	cfg := &Config{Path: path}
	for _, s := range sections {
		switch s.typ {
		case "cpe":
			cfg.CPEs = append(cfg.CPEs, parseCPE(s))
		case "netmon":
			cfg.Netmon = parseNetmon(s)
		case "recovery":
			cfg.Recovery = parseRecovery(s)
		case "history":
			cfg.History.DBPath = strOpt(s, "db_path", "")
			cfg.History.RetentionDays = intOpt(s, "retention_days", 30)
		case "notify":
			name := s.name
			switch name {
			case "telegram":
				cfg.Notify.Telegram.Enabled = boolOpt(s, "enabled")
				cfg.Notify.Telegram.APIToken = strOpt(s, "api_token", "")
				cfg.Notify.Telegram.ChatIDs = strOpt(s, "chat_ids", "")
			case "smtp":
				cfg.Notify.SMTP.Enabled = boolOpt(s, "enabled")
				cfg.Notify.SMTP.Host = strOpt(s, "host", "")
				cfg.Notify.SMTP.Port = intOpt(s, "port", 465)
				cfg.Notify.SMTP.User = strOpt(s, "user", "")
				cfg.Notify.SMTP.Password = strOpt(s, "password", "")
				cfg.Notify.SMTP.From = strOpt(s, "from", "")
				cfg.Notify.SMTP.To = strOpt(s, "to", "")
				cfg.Notify.SMTP.StartTLS = boolOpt(s, "starttls")
			case "webhook":
				cfg.Notify.Webhook.Enabled = boolOpt(s, "enabled")
				cfg.Notify.Webhook.URL = strOpt(s, "url", "")
			}
		case "rules":
			name := s.name
			if name == "sms_received" {
				cfg.Rules.Telegram = boolOpt(s, "telegram")
				cfg.Rules.Email = boolOpt(s, "email")
				cfg.Rules.Webhook = boolOpt(s, "webhook")
				cfg.Rules.FilterMode = firstOpt(s, "filter_mode", "all")
				cfg.Rules.Whitelist = strOpt(s, "whitelist", "")
				cfg.Rules.Keywords = strOpt(s, "keywords", "")
				cfg.Rules.SMSForwardSecret = boolOpt(s, "sms_forward_secret")
			}
		}
	}
	cfg.applyDefaults()
	return cfg, nil
}

// applyDefaults 填充未显式配置的字段默认值。
func (c *Config) applyDefaults() {
	for i := range c.CPEs {
		if c.CPEs[i].PollingInterval <= 0 {
			c.CPEs[i].PollingInterval = 60
		}
		if c.CPEs[i].SMSSyncInterval <= 0 {
			c.CPEs[i].SMSSyncInterval = 30
		}
	}
	if c.Netmon.PingHost == "" {
		c.Netmon.PingHost = "223.5.5.5"
	}
	if c.Netmon.PingPort <= 0 {
		c.Netmon.PingPort = 53
	}
	if c.Netmon.CPETimeout <= 0 {
		c.Netmon.CPETimeout = 5
	}
	if c.Recovery.MaxAttempts <= 0 {
		c.Recovery.MaxAttempts = 3
	}
	if c.History.DBPath == "" {
		// /etc 是 OpenWrt 持久分区（/var → tmpfs 重启即失）。
		c.History.DBPath = "/etc/huawei-cpe/history.db"
	}
	if c.History.RetentionDays <= 0 {
		c.History.RetentionDays = 30
	}
	if c.Recovery.CooldownMinutes <= 0 {
		c.Recovery.CooldownMinutes = 30
	}
	// 未配置 section 的 ID 兜底为类型名
	for i := range c.CPEs {
		if c.CPEs[i].Name == "" {
			c.CPEs[i].Name = c.CPEs[i].ID
		}
	}
}

// parseUCI 解析 UCI 语法（config 段 + option/list 行，支持引号与 # 注释）。
func parseUCI(f *os.File) ([]*uciSection, error) {
	var sections []*uciSection
	var cur *uciSection
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "config") {
			fields := splitFields(line[len("config"):])
			if len(fields) < 1 || len(fields) > 2 {
				return nil, fmt.Errorf("malformed config line: %q", line)
			}
			if cur != nil {
				sections = append(sections, cur)
			}
			typ := fields[0]
			name := typ
			if len(fields) == 2 {
				name = fields[1]
			}
			cur = &uciSection{typ: strings.Trim(typ, "'\""), name: strings.Trim(name, "'\""), opts: map[string][]string{}}
			continue
		}
		if strings.HasPrefix(line, "option") || strings.HasPrefix(line, "list") {
			if cur == nil {
				return nil, fmt.Errorf("option/list outside config section: %q", line)
			}
			kind := "option"
			rest := line[len("option"):]
			if strings.HasPrefix(line, "list") {
				kind = "list"
				rest = line[len("list"):]
			}
			fields := splitFields(rest)
			if len(fields) < 2 {
				return nil, fmt.Errorf("malformed %s line: %q", kind, line)
			}
			key := strings.Trim(fields[0], "'\"")
			val := strings.Trim(fields[1], "'\"")
			if kind == "list" {
				cur.opts[key] = append(cur.opts[key], val)
			} else {
				cur.opts[key] = []string{val}
			}
		}
		// 其它行（缩进内部引用等）忽略
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if cur != nil {
		sections = append(sections, cur)
	}
	return sections, nil
}

// splitFields 按空白切分，支持单引号/双引号包裹的空格值（UCI 语义）。
// 例: `name 'H168-383 主路由'` → ["name", "H168-383 主路由"]
func splitFields(s string) []string {
	var fields []string
	var cur strings.Builder
	inQuote := rune(0)
	for _, r := range s {
		switch {
		case inQuote != 0:
			if r == inQuote {
				inQuote = 0
			} else {
				cur.WriteRune(r)
			}
		case r == '\'' || r == '"':
			inQuote = r
		case r == ' ' || r == '\t' || r == '\r' || r == '\n':
			if cur.Len() > 0 {
				fields = append(fields, cur.String())
				cur.Reset()
			}
		default:
			cur.WriteRune(r)
		}
	}
	if cur.Len() > 0 {
		fields = append(fields, cur.String())
	}
	return fields
}

func parseCPE(s *uciSection) CPE {
	return CPE{
		ID:              s.name,
		Enabled:         boolOpt(s, "enabled"),
		Name:            strOpt(s, "name", ""),
		Host:            strOpt(s, "host", ""),
		Username:        strOpt(s, "username", ""),
		Password:        strOpt(s, "password", ""),
		PollingInterval: intOpt(s, "polling_interval", 60),
		SMSSyncInterval: intOpt(s, "sms_sync_interval", 30),
	}
}

func parseNetmon(s *uciSection) Netmon {
	return Netmon{
		Enabled:    boolOpt(s, "enabled"),
		PingHost:   strOpt(s, "ping_host", ""),
		PingPort:   intOpt(s, "ping_port", 53),
		CPETimeout: intOpt(s, "cpe_timeout", 5),
	}
}

func parseRecovery(s *uciSection) Recovery {
	return Recovery{
		Enabled:         boolOpt(s, "enabled"),
		MaxAttempts:     intOpt(s, "max_attempts", 3),
		CooldownMinutes: intOpt(s, "cooldown_minutes", 30),
		RebootOnFail:    boolOpt(s, "reboot_on_fail"),
	}
}

func firstOpt(s *uciSection, key, def string) string {
	if v := s.opts[key]; len(v) > 0 && v[0] != "" {
		return v[0]
	}
	return def
}

// strOpt 返回 option 的字符串值；缺失/空时返回 def。
func strOpt(s *uciSection, key, def string) string {
	return firstOpt(s, key, def)
}

func boolOpt(s *uciSection, key string) bool {
	v := firstOpt(s, key, "0")
	switch strings.ToLower(v) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

func intOpt(s *uciSection, key string, def int) int {
	v := s.opts[key]
	if len(v) == 0 || v[0] == "" {
		return def
	}
	n, err := strconv.Atoi(v[0])
	if err != nil {
		return def
	}
	return n
}

// Redacted 返回可安全日志输出的配置摘要（所有密钥字段打码）。
func (c *Config) Redacted() string {
	var b strings.Builder
	fmt.Fprintf(&b, "config path=%s cpes=%d", c.Path, len(c.CPEs))
	for _, p := range c.CPEs {
		fmt.Fprintf(&b, " cpe[%s] enabled=%v host=%s user=%s pass=%s poll=%ds sms=%ds",
			p.ID, p.Enabled, p.Host, p.Username, mask(p.Password), p.PollingInterval, p.SMSSyncInterval)
	}
	fmt.Fprintf(&b, " netmon(enabled=%v host=%s:%d) recovery(enabled=%v attempts=%d cooldown=%d reboot=%v)",
		c.Netmon.Enabled, c.Netmon.PingHost, c.Netmon.PingPort,
		c.Recovery.Enabled, c.Recovery.MaxAttempts, c.Recovery.CooldownMinutes, c.Recovery.RebootOnFail)
	fmt.Fprintf(&b, " tg(enabled=%v token=%s) smtp(enabled=%v host=%s pass=%s) webhook(enabled=%v)",
		c.Notify.Telegram.Enabled, mask(c.Notify.Telegram.APIToken),
		c.Notify.SMTP.Enabled, c.Notify.SMTP.Host, mask(c.Notify.SMTP.Password),
		c.Notify.Webhook.Enabled)
	fmt.Fprintf(&b, " rules(mode=%s fwd_secret=%v)", c.Rules.FilterMode, c.Rules.SMSForwardSecret)
	return b.String()
}

// mask 将敏感字符串打码为固定长度星号。
func mask(s string) string {
	if s == "" {
		return "<empty>"
	}
	return "***"
}
