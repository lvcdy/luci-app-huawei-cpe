package poller

import (
	"strconv"
	"strings"
)

// 字段容忍解析助手。
//
// SDK (huawei-lte-api-go) 对 GET 响应不解析字段，map 值类型取决于传输格式：
//   - XML 接口：数值/枚举一律是 string（如 "901"、"-75"、""）；
//   - JSON 接口：数值是 float64，字符串是 string。
//
// 这些助手统一处理两种形态：缺键、nil、空串、非数字 → 返回零值 + ok=false，
// 调用方据此决定缺省显示 Unavailable/隐藏，绝不 panic。

// str 提取字符串字段。
func str(m map[string]any, key string) (string, bool) {
	if m == nil {
		return "", false
	}
	v, ok := m[key]
	if !ok || v == nil {
		return "", false
	}
	switch t := v.(type) {
	case string:
		s := strings.TrimSpace(t)
		if s == "" {
			return "", false
		}
		return s, true
	case int:
		return strconv.Itoa(t), true
	case int64:
		return strconv.FormatInt(t, 10), true
	case float64:
		// JSON 数值：整数位不带小数点
		return strconv.FormatFloat(t, 'f', -1, 64), true
	default:
		return "", false
	}
}

// strOr 提取字符串，缺省回落。
func strOr(m map[string]any, key string, fallback string) string {
	if s, ok := str(m, key); ok {
		return s
	}
	return fallback
}

// int64p 提取 64 位整数（容忍 string/float64/int）。
func int64p(m map[string]any, key string) (int64, bool) {
	if m == nil {
		return 0, false
	}
	v, ok := m[key]
	if !ok || v == nil {
		return 0, false
	}
	switch t := v.(type) {
	case string:
		s := strings.TrimSpace(t)
		if s == "" {
			return 0, false
		}
		n, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			return 0, false
		}
		return n, true
	case int:
		return int64(t), true
	case int64:
		return t, true
	case float64:
		return int64(t), true
	default:
		return 0, false
	}
}

// intp 提取整数（容忍 string/float64/int）。
func intp(m map[string]any, key string) (int, bool) {
	n, ok := int64p(m, key)
	return int(n), ok
}

// intpOr 提取整数，缺省回落。
func intpOr(m map[string]any, key string, fallback int) int {
	if n, ok := intp(m, key); ok {
		return n
	}
	return fallback
}

// boolp 提取布尔（容忍 string "0"/"1"/"true"/"false"）。
func boolp(m map[string]any, key string) (bool, bool) {
	if m == nil {
		return false, false
	}
	v, ok := m[key]
	if !ok || v == nil {
		return false, false
	}
	switch t := v.(type) {
	case bool:
		return t, true
	case string:
		switch strings.ToLower(strings.TrimSpace(t)) {
		case "1", "true", "yes", "on":
			return true, true
		case "0", "false", "no", "off":
			return false, true
		}
		return false, false
	case float64:
		return t != 0, true
	case int:
		return t != 0, true
	default:
		return false, false
	}
}

// nested 提取嵌套 map（如 response 根被剥落后，某些端点仍有子对象）。
func nested(m map[string]any, key string) (map[string]any, bool) {
	if m == nil {
		return nil, false
	}
	v, ok := m[key]
	if !ok || v == nil {
		return nil, false
	}
	if sub, ok := v.(map[string]any); ok {
		return sub, true
	}
	return nil, false
}
