package poller

import "testing"

// ---- 功能 1/3：小区 CSV 解析（cellparse.go）----

func TestSecCellCSV7Col(t *testing.T) {
	// ARFCN,Band,PCI,RSRP,RSRQ,RSSI,SINR（无 BW、无 CellID）
	c, ok := secCellCSV("A1,B3,301,-75,-8,-65,22")
	if !ok {
		t.Fatal("expected ok for 7-col line")
	}
	if c.ARFCN != "A1" || c.Band != "B3" {
		t.Errorf("ARFCN/Band = %q/%q, want A1/B3", c.ARFCN, c.Band)
	}
	if c.PCI != 301 || c.RSRP != -75 || c.RSRQ != -8 || c.RSSI != -65 || c.SINR != 22 {
		t.Errorf("fields = PCI:%d RSRP:%d RSRQ:%d RSSI:%d SINR:%d, want 301/-75/-8/-65/22",
			c.PCI, c.RSRP, c.RSRQ, c.RSSI, c.SINR)
	}
	if c.BW != 0 {
		t.Errorf("BW = %d, want 0 (absent)", c.BW)
	}
	if c.CellID != 0 {
		t.Errorf("CellID = %d, want 0 (absent)", c.CellID)
	}
}

func TestSecCellCSV8ColWithBW(t *testing.T) {
	// ARFCN,Band,BW,PCI,RSRP,RSRQ,RSSI,SINR（8 列且 BW<=100 → 判 BW）
	c, ok := secCellCSV("A1,B3,20,301,-75,-8,-65,22")
	if !ok {
		t.Fatal("expected ok")
	}
	if c.BW != 20 || c.PCI != 301 {
		t.Errorf("BW/PCI = %d/%d, want 20/301", c.BW, c.PCI)
	}
	if c.RSRP != -75 || c.CellID != 0 {
		t.Errorf("RSRP/CellID = %d/%d, want -75/0", c.RSRP, c.CellID)
	}
}

func TestSecCellCSV9Col(t *testing.T) {
	// ARFCN,Band,BW,PCI,RSRP,RSRQ,RSSI,SINR,CellID
	c, ok := secCellCSV("A1,n78,100,302,-80,-9,-70,18,87654321")
	if !ok {
		t.Fatal("expected ok")
	}
	if c.ARFCN != "A1" || c.Band != "n78" {
		t.Errorf("ARFCN/Band = %q/%q, want A1/n78", c.ARFCN, c.Band)
	}
	if c.BW != 100 || c.PCI != 302 || c.RSRP != -80 {
		t.Errorf("BW/PCI/RSRP = %d/%d/%d, want 100/302/-80", c.BW, c.PCI, c.RSRP)
	}
	if c.CellID != 87654321 {
		t.Errorf("CellID = %d, want 87654321", c.CellID)
	}
}

func TestSecCellCSV8ColPCIFirst(t *testing.T) {
	// 8 列但第 3 列>100：按无 BW 解读（PCI 在前）
	c, ok := secCellCSV("A1,B3,301,-75,-8,-65,22,55")
	if !ok {
		t.Fatal("expected ok")
	}
	if c.BW != 0 {
		t.Errorf("BW = %d, want 0", c.BW)
	}
	if c.PCI != 301 {
		t.Errorf("PCI = %d, want 301", c.PCI)
	}
}

func TestSecCellCSVDirty(t *testing.T) {
	// 列数不足/空行 → false（调用方丢弃）
	if _, ok := secCellCSV("A1,B3"); ok {
		t.Error("expected fail for short line")
	}
	if _, ok := secCellCSV(""); ok {
		t.Error("expected fail for empty line")
	}
	// 脏数字：列数足够 → 宽容解析，脏值置 0
	// "A1,B3,abc,-75,-8,-65,22,9" 是 8 列；fields[2](abc=0) 不满足 "本列<=100 且下一列更大"
	// （fields[3]=-75 不比 0 大），所以按 7 列布局：PCI=fields[2]=0，其余列正常。
	c, ok := secCellCSV("A1,B3,abc,-75,-8,-65,22,9")
	if !ok {
		t.Fatal("dirty numerics with enough columns should parse OK (tolerant)")
	}
	if c.PCI != 0 {
		t.Errorf("PCI = %d, want 0 (dirty)", c.PCI)
	}
	if c.RSRP != -75 || c.SINR != 22 {
		t.Errorf("RSRP/SINR = %d/%d, want -75/22", c.RSRP, c.SINR)
	}
}

func TestParseCellList(t *testing.T) {
	v := "A1,B3,20,301,-75,-8,-65,22,111;A2,n78,100,302,-80,-9,-70,18,222\nA3,B1,5,303,-90,-10,-75,15"
	cells := parseCellList(v)
	if len(cells) != 3 {
		t.Fatalf("len = %d, want 3 (%+v)", len(cells), cells)
	}
	if cells[0].PCI != 301 || cells[1].PCI != 302 || cells[2].PCI != 303 {
		t.Errorf("PCIs = %d/%d/%d, want 301/302/303",
			cells[0].PCI, cells[1].PCI, cells[2].PCI)
	}
	// 坏行忽略
	bad := parseCellList("BAD,bad;A1,B3,301,-75,-8,-65,22")
	if len(bad) != 1 || bad[0].PCI != 301 {
		t.Errorf("dirty filtering failed: %+v", bad)
	}
	if len(parseCellList("")) != 0 {
		t.Error("empty input should produce empty slice")
	}
}

func TestFillCellFromInfo(t *testing.T) {
	var cell CellDetail
	m := map[string]any{
		"arfcn": "1825", "earfcn": "1825", "nrarfcn": "156000",
		"bandwidth": "20", "cqi": "12", "pci": "301", "cell_id": "12345678",
	}
	fillCellFromInfo(&cell, m)
	if cell.ARFCN != "1825" || cell.EARFCN != "1825" || cell.NRARFCN != "156000" {
		t.Errorf("ARFCN/EARFCN/NRARFCN = %q/%q/%q", cell.ARFCN, cell.EARFCN, cell.NRARFCN)
	}
	if cell.Bandwidth != 20 || cell.CQI != 12 || cell.PCI != 301 || cell.CellID != 12345678 {
		t.Errorf("BW/CQI/PCI/CellID = %d/%d/%d/%d",
			cell.Bandwidth, cell.CQI, cell.PCI, cell.CellID)
	}

	// earfcn 兜底 ARFCN（无 arfcn 键）
	var c2 CellDetail
	fillCellFromInfo(&c2, map[string]any{"earfcn": "1650"})
	if c2.ARFCN != "1650" || c2.EARFCN != "1650" {
		t.Errorf("fallback ARFCN = %q (earfcn %q)", c2.ARFCN, c2.EARFCN)
	}
}
