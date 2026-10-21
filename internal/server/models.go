package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"sync"
	"time"

	"codearts2api/internal/apierr"
	"codearts2api/internal/registry"
)

// modelCache 记录模型清单的刷新状态（清单本体在 registry 里）。
type modelCache struct {
	mu       sync.RWMutex
	err      string
	loadedAt time.Time
}

func newModelCache() *modelCache { return &modelCache{} }

// errEmptyModelList 表示上游 200 但没返回任何可用模型。
var errEmptyModelList = errors.New("上游未返回任何可用模型")

// RefreshModels 从上游拉取模型清单并写入注册表。
//
// 2026-10 起免费权益通道下线，模型清单只有一个来源：`GET /v1/model/builtin`
// （积分通道）。拉取失败时保留现有清单，只记录错误。
func (s *Server) RefreshModels(ctx context.Context) error {
	acct := s.Store.NextAccount()
	if acct == nil {
		return apierr.NoAccount
	}
	sess, err := s.Cred.Session(ctx, acct, s.resolveOptions())
	if err != nil {
		return err
	}

	list, err := s.Up.FetchBuiltinModels(ctx, sess)
	if err != nil {
		return err
	}
	models := registry.FromBuiltin(list)
	if len(models) == 0 {
		return errEmptyModelList
	}

	s.Reg.Replace(models)

	s.cache.mu.Lock()
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
				"name":       m.Name,
				"context":    m.Context,
				"max_output": m.MaxOutput,
				"source":     m.Source,
				// 免费权益通道已下线：所有模型都走积分通道、真实扣费。
				"channel":     "credit",
				"billable":    true,
				"credit":      m.Credit,
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
