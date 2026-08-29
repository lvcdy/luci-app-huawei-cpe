// 设备写操作 HTTP 端点（功能 5 锁频 / 6 流量开关 / 7 重启 / 网络模式）。
//
// 安全：全部 POST 仅回环；body 采用严格 JSON 解码（未知字段拒绝）；
// 错误只回泛化消息，CPE 响应体原样不透出（可能含敏感信息）。
package httpapi

import (
	"encoding/json"
	"net/http"

	"huawei-cpe/internal/device"
)

// handleCellLock GET 返回当前锁频参数（读缓存的 celllock 采集结果）。
func (s *Server) handleCellLockGet(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !s.knownDevice(id) {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "device not found"})
		return
	}
	if s.store == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "cache not ready"})
		return
	}
	snap, _, ok := s.store.Get(id)
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "no snapshot yet"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"device":  id,
		"lock":    snap.Lock.Lock,
		"freq":    snap.Lock.Freq,
		"pci":     snap.Lock.PCI,
		"maxfreq": snap.Lock.MaxFreq,
	})
}

// handleCellLock POST 设置锁频（/cell-lock）。
// body: {"lock":1,"freq":1825,"pci":0} 或解锁 {"lock":0,"freq":0,"pci":0}。
func (s *Server) handleCellLock(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !s.knownDevice(id) {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "device not found"})
		return
	}
	var req struct {
		Lock int `json:"lock"`
		Freq int `json:"freq"`
		PCI  int `json:"pci"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid json body"})
		return
	}

	d := s.mgr.Get(id)
	if d == nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "device manager not ready"})
		return
	}
	if err := d.SetCellLock(r.Context(), device.CellLockReq{
		Lock: req.Lock, Freq: req.Freq, PCI: req.PCI,
	}); err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": "lock-cell failed"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"device": id, "locked": req.Lock != 0})
}

// handleNetMode POST 设置网络模式（/net-mode）。
// body 任意字段可省略（保持该维度不变）：
//
//	{"network_mode":"03"}          → 4G-only（字符串枚举 "00".."0302"）
//	{"lte_band":null}              → 重置 4G 频段
//	{"network_band":null,"network_mode":"00"} → 自动模式
func (s *Server) handleNetMode(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !s.knownDevice(id) {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "device not found"})
		return
	}
	var req struct {
		LTEBand     *int    `json:"lte_band"`
		NetworkBand *int    `json:"network_band"`
		NetworkMode *string `json:"network_mode"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid json body"})
		return
	}
	if req.LTEBand == nil && req.NetworkBand == nil && req.NetworkMode == nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "nothing to set"})
		return
	}

	d := s.mgr.Get(id)
	if d == nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "device manager not ready"})
		return
	}
	// 嵌套 map：将 int 转成 SDK 期望的 interface{} 位掩码/字符串
	var lte, nb any
	if req.LTEBand != nil {
		lte = *req.LTEBand
	}
	if req.NetworkBand != nil {
		nb = *req.NetworkBand
	}
	var nm any
	if req.NetworkMode != nil {
		nm = *req.NetworkMode
	}
	if err := d.SetNetMode(r.Context(), device.NetModeReq{
		LTEBand: lte, NetworkBand: nb, NetworkMode: nm,
	}); err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": "set-net-mode failed"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"device": id, "applied": true})
}

// handleDataSwitch POST 开关流量（/data-switch）。
// body: {"on":true} 开 / {"on":false} 关。
func (s *Server) handleDataSwitch(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !s.knownDevice(id) {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "device not found"})
		return
	}
	var req struct {
		On bool `json:"on"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid json body"})
		return
	}

	d := s.mgr.Get(id)
	if d == nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "device manager not ready"})
		return
	}
	if err := d.SetDataSwitch(r.Context(), req.On); err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": "data-switch failed"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"device": id, "on": req.On})
}

// handleReboot POST 重启 CPE（/reboot）。CPE 重启会断开连接约数十秒，
// 轮询自动在检测到离线后进入恢复流程；此端点立即返回 accepted。
func (s *Server) handleReboot(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !s.knownDevice(id) {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "device not found"})
		return
	}

	d := s.mgr.Get(id)
	if d == nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "device manager not ready"})
		return
	}
	if err := d.Reboot(r.Context()); err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": "reboot failed"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"device": id, "rebooting": true})
}

// pollerFor 返回设备的暂停控制器（功能 10）；nil = 未装配。
func (s *Server) pollerFor(id string) PauseController {
	if s.pollers == nil {
		return nil
	}
	return s.pollers[id]
}

// handlePollingSuspend POST 暂停轮询（/polling/suspend）。
// 后台页面失焦时调用：停止向 CPE 请求，保留最近快照（读 API 仍可用）。
func (s *Server) handlePollingSuspend(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !s.knownDevice(id) {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "device not found"})
		return
	}
	p := s.pollerFor(id)
	if p == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "poller control not ready"})
		return
	}
	p.Suspend()
	writeJSON(w, http.StatusOK, map[string]any{"device": id, "suspended": true})
}

// handlePollingResume POST 恢复轮询（/polling/resume）。
// 前台页面聚焦时调用：立即触发一次采集并恢复周期。
func (s *Server) handlePollingResume(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !s.knownDevice(id) {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "device not found"})
		return
	}
	p := s.pollerFor(id)
	if p == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "poller control not ready"})
		return
	}
	p.Resume()
	writeJSON(w, http.StatusOK, map[string]any{"device": id, "suspended": false})
}

// handlePollingStatus GET 返回轮询状态（/polling）。
func (s *Server) handlePollingStatus(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !s.knownDevice(id) {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "device not found"})
		return
	}
	p := s.pollerFor(id)
	if p == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "poller control not ready"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"device": id, "suspended": p.IsSuspended(),
	})
}
