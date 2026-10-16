package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"sync"
	"time"

	"codearts2api/internal/codearts"
	"codearts2api/internal/registry"
)

// modelCache 缓存上游模型清单与额度快照。
type modelCache struct {
	mu        sync.RWMutex
	benefit   []registry.Model
	builtin   []registry.Model
	balance   *codearts.TokensBalance
	balanceAt time.Time
	err       string
	loadedAt  time.Time
}

func newModelCache() *modelCache { return &modelCache{} }

// Refresh 从上游拉取模型清单（benefit + builtin）并写入注册表。
func (s *Server) RefreshModels(ctx context.Context) error {
	acct := s.Store.NextAccount()
	if acct == nil {
		return errNoAccount
	}
	sess, err := s.Cred.Session(ctx, acct, s.resolveOptions())
	if err != nil {
		return err
	}

	var benefit []registry.Model
	if cfg, err := s.Up.FetchGatewayConfig(ctx, sess); err == nil {
		benefit = registry.FromGatewayConfig(cfg.Models)
	} else {
		return err
	}

	var builtin []registry.Model
	if list, err := s.Up.FetchBuiltinModels(ctx, sess); err == nil {
		builtin = registry.FromBuiltin(list)
	}

	s.Reg.Replace(benefit)
	if len(builtin) > 0 {
		s.Reg.ReplaceBuiltin(builtin)
	}

	s.cache.mu.Lock()
	s.cache.benefit = benefit
	s.cache.builtin = builtin
	s.cache.loadedAt = time.Now()
	s.cache.err = ""
	s.cache.mu.Unlock()
	return nil
}

// refreshModelsIfStale 在缓存过期时后台刷新一次模型清单。
func (s *Server) refreshModelsIfStale(maxAge time.Duration) {
	s.cache.mu.RLock()
	stale := time.Since(s.cache.loadedAt) > maxAge
	s.cache.mu.RUnlock()
	if !stale {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := s.RefreshModels(ctx); err != nil {
		s.cache.mu.Lock()
		s.cache.err = err.Error()
		s.cache.loadedAt = time.Now()
		s.cache.mu.Unlock()
	}
}

// handleModels 实现 GET /v1/models。
func (s *Server) handleModels(w http.ResponseWriter, r *http.Request) {
	s.refreshModelsIfStale(10 * time.Minute)

	settings := s.Store.Settings()
	models := s.Reg.List(settings.ModelAliases)
	now := time.Now().Unix()

	data := make([]map[string]any, 0, len(models))
	for _, m := range models {
		data = append(data, map[string]any{
			"id":       m.ID,
			"object":   "model",
			"created":  now,
			"owned_by": "codearts",
			"codearts": map[string]any{
				"name":        m.Name,
				"context":     m.Context,
				"max_output":  m.MaxOutput,
				"source":      m.Source,
				"description": m.Desc,
			},
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": data})
}

// handleAdminRefreshModels 强制刷新模型清单（控制台按钮）。
func (s *Server) handleAdminRefreshModels(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "仅支持 POST", "invalid_request_error")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	if err := s.RefreshModels(ctx); err != nil {
		writeErr(w, http.StatusBadGateway, "刷新失败: "+err.Error(), "upstream_error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":     true,
		"models": s.Reg.List(s.Store.Settings().ModelAliases),
	})
}

// handleAdminBalance 查询额度余额（带 60 秒缓存）。
func (s *Server) handleAdminBalance(w http.ResponseWriter, r *http.Request) {
	s.cache.mu.RLock()
	bal, at := s.cache.balance, s.cache.balanceAt
	s.cache.mu.RUnlock()
	if bal != nil && time.Since(at) < 60*time.Second {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "balance": bal, "cached": true})
		return
	}

	acct := s.Store.NextAccount()
	if acct == nil {
		writeErr(w, http.StatusBadRequest, errNoAccount.Error(), "invalid_request_error")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	sess, err := s.Cred.Session(ctx, acct, s.resolveOptions())
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error(), "upstream_error")
		return
	}
	out, err := s.Up.FetchBalance(ctx, sess)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error(), "upstream_error")
		return
	}
	s.cache.mu.Lock()
	s.cache.balance = out
	s.cache.balanceAt = time.Now()
	s.cache.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "balance": out, "account": acct.DisplayName()})
}

// upstreamErrorPayload 尝试从上游错误响应里提取可读信息。
func upstreamErrorPayload(raw []byte) string {
	var probe struct {
		ErrorCode string `json:"error_code"`
		ErrorMsg  string `json:"error_msg"`
		Error     struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &probe); err == nil {
		if probe.ErrorCode != "" {
			return probe.ErrorCode + ": " + probe.ErrorMsg
		}
		if probe.Error.Message != "" {
			return probe.Error.Message
		}
	}
	return truncateStr(string(raw), 300)
}

// readAll 读取响应体（带大小上限）。
func readAll(rc io.ReadCloser, limit int64) ([]byte, error) {
	defer rc.Close()
	return io.ReadAll(io.LimitReader(rc, limit))
}

// truncateStr 截断长字符串。
func truncateStr(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
