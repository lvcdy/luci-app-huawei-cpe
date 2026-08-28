// Package httpapi 提供 LuCI 前端访问的本地 JSON API。
//
// 安全约束：
//   - 仅监听 127.0.0.1（回环），绝不绑定对外地址
//   - 所有响应 JSON 不含任何密钥字段（密码 / token / 短信正文默认不返回）
//   - 统一 /api/v1 前缀
package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"runtime/debug"
	"time"

	"huawei-cpe/internal/config"
)

// DefaultAddr 是 API 监听地址（仅回环）。Phase 2+ 起可配置。
const DefaultAddr = "127.0.0.1:9090"

// Server 是 HTTP JSON API 服务。
type Server struct {
	log *slog.Logger
	cfg *config.Config
	srv *http.Server
	ln  net.Listener
	mux *http.ServeMux
}

// New 创建 API server（未监听，需调用 Start）。
func New(log *slog.Logger, cfg *config.Config) *Server {
	s := &Server{log: log, cfg: cfg, mux: http.NewServeMux()}
	s.routes()
	return s
}

// routes 注册全部端点。
func (s *Server) routes() {
	s.mux.HandleFunc("GET /api/v1/health", s.handleHealth)
	s.mux.HandleFunc("GET /api/v1/devices", s.handleDevices)
	s.mux.HandleFunc("GET /api/v1/status", s.handleStatus)
}

// Start 在回环地址监听并启动服务。
func (s *Server) Start() error {
	ln, err := net.Listen("tcp", DefaultAddr)
	if err != nil {
		return fmt.Errorf("listen %s: %w", DefaultAddr, err)
	}
	s.ln = ln
	s.srv = &http.Server{
		Handler:           s.recoverMiddleware(s.mux),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	go func() {
		if err := s.srv.Serve(ln); err != nil && err != http.ErrServerClosed {
			s.log.Error("http server", "err", err)
		}
	}()
	return nil
}

// Shutdown 优雅关闭。
func (s *Server) Shutdown(ctx context.Context) error {
	if s.srv == nil {
		return nil
	}
	return s.srv.Shutdown(ctx)
}

// UpdateConfig 热更新配置引用（reload 时调用）。
func (s *Server) UpdateConfig(cfg *config.Config) { s.cfg = cfg }

// recoverMiddleware 兜底 panic 不崩溃 daemon，返回 500（日志不泄露密钥）。
func (s *Server) recoverMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				s.log.Error("panic in http handler",
					"method", r.Method, "path", r.URL.Path,
					"panic", string(debug.Stack()))
				writeJSON(w, http.StatusInternalServerError, map[string]any{
					"error": "internal server error",
				})
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// --- handlers ------------------------------------------------------------

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"status": "ok",
		"cpes":   len(s.cfg.CPEs),
		"uptime": time.Since(serverStart),
	})
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	// Phase 1：仅配置摘要（脱敏）；P1.6+ 提供真实状态缓存。
	writeJSON(w, http.StatusOK, map[string]any{
		"state": "running",
		"cpes":  redactedCPEs(s.cfg),
	})
}

func (s *Server) handleDevices(w http.ResponseWriter, r *http.Request) {
	devs := make([]map[string]any, 0, len(s.cfg.CPEs))
	for _, c := range s.cfg.CPEs {
		devs = append(devs, map[string]any{
			"id":      c.ID,
			"name":    c.Name,
			"host":    c.Host,
			"enabled": c.Enabled,
			// 绝不返回 password/username
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"devices": devs})
}

// serverStart 记录服务启动时间（health 用）。
var serverStart = time.Now()

// redactedCPEs 输出脱敏的设备列表（仅 id/name/host，无凭据）。
func redactedCPEs(cfg *config.Config) []map[string]any {
	out := make([]map[string]any, 0, len(cfg.CPEs))
	for _, c := range cfg.CPEs {
		out = append(out, map[string]any{
			"id":   c.ID,
			"name": c.Name,
			"host": c.Host,
		})
	}
	return out
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
