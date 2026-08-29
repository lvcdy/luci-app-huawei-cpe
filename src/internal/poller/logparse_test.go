package poller

import (
	"encoding/base64"
	"testing"
)

// ---- 功能 4：系统操作日志解析（logparse.go）----

func TestParseLogInfoList(t *testing.T) {
	m := map[string]any{
		"loginfo": []any{
			map[string]any{
				"logtype":    "Syslog",
				"loglevel":   "INFO",
				"logtime":    "2026-08-29 12:34:56",
				"logcontent": "CPE rebooted",
			},
			map[string]any{
				"logtype":    "Operation",
				"loglevel":   "WARN",
				"logtime":    "2026-08-29 12:00:00",
				"logcontent": "Weak signal",
			},
		},
	}
	entries := parseLogInfo(m)
	if len(entries) != 2 {
		t.Fatalf("len = %d, want 2", len(entries))
	}
	// 倒序：最新在前
	if entries[0].Info != "Weak signal" || entries[1].Info != "CPE rebooted" {
		t.Errorf("order = [%q, %q], want [Weak signal, CPE rebooted]",
			entries[0].Info, entries[1].Info)
	}
	if entries[0].Type != "Operation" || entries[0].Level != "WARN" {
		t.Errorf("entry0 = %+v", entries[0])
	}
	if entries[0].Time != "2026-08-29 12:00:00" {
		t.Errorf("time = %q", entries[0].Time)
	}
}

func TestParseLogInfoSingleFlat(t *testing.T) {
	// 平铺单条（无 loginfo 列表）：当前 map 即单条
	m := map[string]any{
		"logcount":   "1",
		"logtype":    "Alarm",
		"loglevel":   "ERROR",
		"logtime":    "2026-08-28 08:00:00",
		"logcontent": "Over temperature",
	}
	entries := parseLogInfo(m)
	if len(entries) != 1 {
		t.Fatalf("len = %d, want 1", len(entries))
	}
	if entries[0].Info != "Over temperature" || entries[0].Type != "Alarm" {
		t.Errorf("entry = %+v", entries[0])
	}
}

func TestParseLogInfoSingleMap(t *testing.T) {
	// loginfo 是单 map（非列表）
	m := map[string]any{
		"loginfo": map[string]any{
			"logtype":    "x",
			"loglevel":   "INFO",
			"logtime":    "t",
			"logcontent": "single",
		},
	}
	entries := parseLogInfo(m)
	if len(entries) != 1 || entries[0].Info != "single" {
		t.Fatalf("entries = %+v", entries)
	}
}

func TestParseLogInfoAllDirty(t *testing.T) {
	// 全空 → nil（前端隐藏卡片）
	if got := parseLogInfo(map[string]any{"logcount": "0"}); got != nil {
		t.Fatalf("want nil, got %+v", got)
	}
	if got := parseLogInfo(nil); got != nil {
		t.Fatalf("want nil for nil input, got %+v", got)
	}
}

func TestParseLogInfoTruncate(t *testing.T) {
	var list []any
	for i := 0; i < maxLogEntries+10; i++ {
		list = append(list, map[string]any{
			"logtype":    "x",
			"logcontent": "entry",
		})
	}
	entries := parseLogInfo(map[string]any{"loginfo": list})
	if len(entries) != maxLogEntries {
		t.Fatalf("len = %d, want %d", len(entries), maxLogEntries)
	}
}

func TestDecodeLogContentBase64(t *testing.T) {
	plain := "CPE reboot request from web UI"
	enc := base64.StdEncoding.EncodeToString([]byte(plain))
	got := decodeLogContent("BASE64:" + enc)
	if got != plain {
		t.Errorf("decode = %q, want %q", got, plain)
	}
	// 无前缀的裸 base64：解码后是可读文本 → 采纳
	if got := decodeLogContent(enc); got != plain {
		t.Errorf("bare b64 = %q, want %q", got, plain)
	}
	// 普通英文文本（恰好全为 base64 字符）：误解码结果乱码 → 保留原文
	if got := decodeLogContent("single"); got != "single" {
		t.Errorf("plain word 'single' = %q, want passthrough", got)
	}
	if got := decodeLogContent("Weak signal"); got != "Weak signal" {
		t.Errorf("plain 'Weak signal' = %q, want passthrough", got)
	}
	// 非 base64 → 原文
	if got := decodeLogContent("hello world"); got != "hello world" {
		t.Errorf("plain = %q", got)
	}
	// 空 → 空
	if got := decodeLogContent("   "); got != "" {
		t.Errorf("blank = %q", got)
	}
}
