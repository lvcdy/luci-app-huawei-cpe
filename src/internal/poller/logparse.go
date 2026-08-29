package poller

import (
	"encoding/base64"
	"strings"
	"unicode/utf8"
)

// 系统操作日志解析（功能 4：api/log/loginfo）。
//
// 华为 5G CPE 的 loginfo 响应是 XML 键值对，常见结构：
//
//	<response>
//	  <loginfo>
//	    <logtype>...日志类型...</logtype>
//	    <loglevel>INFO</loglevel>
//	    <logtime>2026-08-29 12:34:56</logtime>
//	    <logcontent>...可能为 base64...</logcontent>
//	  </loginfo>
//	  <loginfo>...</loginfo>
//	</response>
//
// 或平铺：
//
//	<response><logcount>5</logcount><logtype>..</logtype><loglevel>..</loglevel></response>
//
// 内容可能是普通文本也可能是 base64 编码（内存告警/操作日志混合）。
// 解析规则：取 loginfo 列表（或平铺单条）；字段容忍；内容 base64 能解则解，
// 解不开保留原文；最后按时间倒序截断到 maxLogEntries 条。

// maxLogEntries 是快照中保留的日志条目上限（轮询每周期都会全量刷，截断防膨胀）。
const maxLogEntries = 50

// parseLogInfo 解析 loginfo 响应为结构化条目（倒序：最新在前）。
// 空响应 / 全脏 → 返回 nil（前端隐藏卡片而非显示空表）。
func parseLogInfo(m map[string]any) []LogEntry {
	if m == nil {
		return nil
	}

	// 1. 收集所有 loginfo 子对象（列表形态）或剥掉响应根后平铺
	var raws []map[string]any
	if list, ok := collectLogEntries(m); ok {
		raws = list
	} else {
		// 平铺形态：单条 = 当前 map
		raws = []map[string]any{m}
	}

	var out []LogEntry
	for _, raw := range raws {
		e := LogEntry{
			Type:  strOr(raw, "logtype", strOr(raw, "LogType", "")),
			Level: strOr(raw, "loglevel", strOr(raw, "LogLevel", "")),
			Time:  strOr(raw, "logtime", strOr(raw, "LogTime", strOr(raw, "LogTimeStamp", ""))),
			Info:  decodeLogContent(strOr(raw, "logcontent", strOr(raw, "LogContent", ""))),
		}
		// 全空条目丢弃
		if e.Type == "" && e.Level == "" && e.Time == "" && e.Info == "" {
			continue
		}
		out = append(out, e)
	}

	// 2. 倒序（固件多数按时间升序给，前端显示最新在前）
	// 时间解析太脆（固件格式漂移），只做稳定逆序。
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}

	// 3. 截断到上限
	if len(out) > maxLogEntries {
		out = out[:maxLogEntries]
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// collectLogEntries 从响应中收集 loginfo 条目列表。
// 该函数容忍三种形态：
//  1. m["loginfo"] 是 []any（每个元素是 map）；
//  2. m["loginfo"] 是 map[string]any（单条，包成列表）；
//  3. m["loginfo"] 是 []map[string]any（SDK JSON 形态）。
func collectLogEntries(m map[string]any) ([]map[string]any, bool) {
	v, ok := m["loginfo"]
	if !ok || v == nil {
		return nil, false
	}
	switch t := v.(type) {
	case []any:
		var out []map[string]any
		for _, item := range t {
			if sub, ok := item.(map[string]any); ok {
				out = append(out, sub)
			}
		}
		return out, len(out) > 0
	case []map[string]any:
		return t, len(t) > 0
	case map[string]any:
		return []map[string]any{t}, true
	}
	return nil, false
}

// decodeLogContent 尝试 base64 解码日志内容；失败返回原文。
// 华为部分日志内容带前缀（如 "BASE64:" / "base64," / "b64:"），先剥离。
//
// 误解码防护：普通可读文本（如 "Over temperature"）恰好全部由 base64 字符构成时，
// 也可能通过错误解码得到非 UTF-8 / 乱码。因此：
//   - 显式带前缀 → 强制解码（即使结果不可读，保留原样）；
//   - 无前缀 → 仅当解码结果是有效 UTF-8 且可打印字符占比足够时才采纳，否则返回原文。
func decodeLogContent(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	raw := s
	prefixed := false
	for _, prefix := range []string{"BASE64:", "base64,", "b64:"} {
		if strings.HasPrefix(strings.ToUpper(s), strings.ToUpper(prefix)) {
			prefixed = true
			raw = s[len(prefix):]
			break
		}
	}
	trimmed := strings.TrimSpace(raw)
	if decoded, err := base64.StdEncoding.DecodeString(trimmed); err == nil {
		if prefixed || readableText(decoded) {
			return string(decoded)
		}
	}
	if decoded, err := base64.RawStdEncoding.DecodeString(trimmed); err == nil {
		if prefixed || readableText(decoded) {
			return string(decoded)
		}
	}
	return s
}

// readableText 判断解码结果是否适合作为日志文本返回：
// 有效 UTF-8 且可打印字符（字母、数字、标点、空格）占比 ≥ 80%。
// 防止无前缀时把普通英文文本误解码成乱码（如 "single" → Raw 解码后是二进制垃圾）。
func readableText(b []byte) bool {
	if !utf8.Valid(b) {
		return false
	}
	if len(b) == 0 {
		return false
	}
	printable := 0
	for _, r := range string(b) {
		switch {
		case r >= ' ' && r <= '~':
			printable++
		case r == '\n' || r == '\r' || r == '\t':
			printable++
		}
	}
	return float64(printable)/float64(len(b)) >= 0.8
}
