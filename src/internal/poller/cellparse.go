package poller

import (
	"strconv"
	"strings"
)

// 小区 RF 解析（功能 1/3：服务小区详情的 ARFCN/带宽/CQI/CellID，
// 载波聚合 SecCellInfo / 邻小区 NbrCellInfo 的 CSV 风格列表）。
//
// 华为 5G CPE 的小区数据来自三个来源，格式各不相同，全部按
// "容忍优先" 解析——字段缺/脏/顺序漂移绝不 panic，只丢弃坏行：
//
//  1. net/cell-info（CellInfo）：XML 键值对，键名含 arfcn/earfcn/nrarfcn、
//     bandwidth/bw、cqi 与 cell_id/cellid；
//  2. device/seccellinfo（SecCellInfo）：CSV 风格字符串
//     "ARFCN,Band,BW,PCI,RSRP,RSRQ,RSSI,SINR,CellID;...",
//     键 nrseccell_list / lteseccell_list / cell_id；
//  3. device/nbrcellinfo（NbrCellInfo）：CSV 风格多行（; 分隔或空行），
//     键 nbrcell_nrlist / nbrcell_ltelist。
//
// 列位序以 SDK 注释为准，但按行内元素数自适应（>=7 列时按
// ARFCN,Band,PCI,RSRP,RSRQ,RSSI,SINR 解读；>=9 列时第 9 列为 CellID）。

// CellDetail 是服务小区（主小区）的 RF 详情（来源 net/cell-info + 信号字段并集）。
type CellDetail struct {
	ARFCN     string // 通用 ARFCN（4G earfcn / 5G nrarfcn 的并集显示）
	EARFCN    string // LTE ARFCN（可空）
	NRARFCN   string // NR ARFCN（可空）
	Bandwidth int    // 小区带宽 MHz（0 = 未知）
	CQI       int    // 信道质量指示（0 = 未知）
	PCI       int    // 物理小区 ID（0 = 未知）
	CellID    int64  // 小区标识（0 = 未知）
}

// CellState 是载波聚合（CA）/邻区的单小区条目。
type CellState struct {
	ARFCN  string // 频率点（earfcn/nrarfcn）
	Band   string // band 描述（B3 / n78 等）
	BW     int    // 带宽 MHz（0 = 未知）
	PCI    int    // 物理小区 ID
	RSRP   int    // dBm（华为样式负值）
	RSRQ   int    // dB（华为样式 x2）
	RSSI   int    // dBm
	SINR   int    // dB
	CellID int64  // 小区标识（0 = 未知）
}

// secCellCSV 解析一条 "ARFCN,Band,BW,PCI,RSRP,RSRQ,RSSI,SINR[,CellID]"
// 或 "ARFCN,Band,PCI,RSRP,RSRQ,RSSI,SINR" 的 CSV 行。
// 失败（列数不足/数字脏）返回零值 + false，调用方丢弃该行。
func secCellCSV(line string) (CellState, bool) {
	fields := strings.Split(strings.TrimSpace(line), ",")
	// 至少需要 ARFCN + Band + PCI + RSRP + RSRQ + RSSI + SINR = 7 列；
	// 部分固件 Band 列缺省为 0，仍按 7 列处理。
	if len(fields) < 7 {
		return CellState{}, false
	}
	c := CellState{
		ARFCN: strings.TrimSpace(fields[0]),
		Band:  strings.TrimSpace(fields[1]),
	}
	// 第 3 列（index 2）可能是 BW 或 PCI：
	//  - 9 列 CSV（…BW,PCI,RSRP…,CellID）一定是 BW；
	//  - 8 列（BW,PCI,… 无 CellID）用“本列<=100 且下一列(PCI)更大”判定；
	//  - 7 列（ARFCN,Band,PCI,…）无 BW，直接按 PCI 解。
	idx := 2
	if len(fields) >= 9 {
		c.BW = atoiSafe(fields[2])
		idx = 3
	} else if len(fields) == 8 && atoiSafe(fields[2]) <= 100 && atoiSafe(fields[3]) > atoiSafe(fields[2]) {
		c.BW = atoiSafe(fields[2])
		idx = 3
	}
	c.PCI = atoiSafe(fields[idx])
	c.RSRP = atoiSafe(fields[idx+1])
	c.RSRQ = atoiSafe(fields[idx+2])
	c.RSSI = atoiSafe(fields[idx+3])
	c.SINR = atoiSafe(fields[idx+4])
	// 第 9 列（若有）是 CellID
	if len(fields) >= idx+6 {
		c.CellID = atoi64Safe(fields[idx+5])
	}
	return c, true
}

// atoiSafe 容忍解析整数；脏值返回 0（不 panic）。
func atoiSafe(s string) int {
	n, _ := strconv.Atoi(strings.TrimSpace(s))
	return n
}

// atoi64Safe 容忍解析 64 位整数；脏值返回 0。
func atoi64Safe(s string) int64 {
	n, _ := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	return n
}

// parseCellList 解析 CSV 风格小区列表（; 或换行分隔多行）。
// 返回解析成功的所有小区；空/全脏返回空切片（前端隐藏该卡片）。
func parseCellList(v string) []CellState {
	var out []CellState
	for _, line := range strings.FieldsFunc(v, func(r rune) bool {
		return r == ';' || r == '\n' || r == '\r' || r == '|'
	}) {
		if c, ok := secCellCSV(line); ok {
			out = append(out, c)
		}
	}
	return out
}

// fillCellFromInfo 解析 net/cell-info 的键值形态。
// 字段容忍：任何键缺失/脏值都不影响其它字段。
func fillCellFromInfo(cell *CellDetail, m map[string]any) {
	if cell == nil || m == nil {
		return
	}
	// ARFCN 通用键：arfcn（部分型号）；4G earfcn / 5G nrarfcn
	cell.ARFCN = strOr(m, "arfcn", cell.ARFCN)
	if v, ok := str(m, "earfcn"); ok {
		cell.EARFCN = v
		if cell.ARFCN == "" {
			cell.ARFCN = v
		}
	}
	if v, ok := str(m, "nrarfcn"); ok {
		cell.NRARFCN = v
		if cell.ARFCN == "" {
			cell.ARFCN = v
		}
	}
	// 带宽：bandwidth（MHz）；部分型号 bw
	if v, ok := intp(m, "bandwidth"); ok {
		cell.Bandwidth = v
	} else if v, ok := intp(m, "bw"); ok {
		cell.Bandwidth = v
	}
	// CQI：cqi
	if v, ok := intp(m, "cqi"); ok {
		cell.CQI = v
	}
	// PCI / CellID：cell_id（部分型号 cellid / CellID）
	if v, ok := intp(m, "pci"); ok {
		cell.PCI = v
	}
	if v, ok := int64p(m, "cell_id"); ok {
		cell.CellID = v
	} else if v, ok := int64p(m, "cellid"); ok {
		cell.CellID = v
	}
}

// findNestedList 提取嵌套键中的 CSV 列表字符串。
// 华为 XML 响应可能把列表值套在子对象里（如 <nrseccell_list> 直接是字符串，
// 但部分固件是 <seccellinfo><nrseccell_list>…）；这里逐层找。
func findNestedList(m map[string]any, keys ...string) string {
	for _, k := range keys {
		if v, ok := str(m, k); ok {
			return v
		}
		if sub, ok := nested(m, k); ok {
			if v, ok := str(sub, "Value"); ok {
				return v
			}
			if v, ok := str(sub, "value"); ok {
				return v
			}
		}
	}
	return ""
}
