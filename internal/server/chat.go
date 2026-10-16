package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"codearts2api/internal/codearts"
	"codearts2api/internal/registry"
	"codearts2api/internal/store"
)

// errNoAccount 表示账号池为空。
var errNoAccount = errors.New("没有可用账号，请先在控制台「导入本机桌面端」或在 CodeArts Agent 里登录")

// chatRequest 是 OpenAI Chat Completions 的请求体（仅取网关需要处理的字段）。
//
// 其余字段（tools / response_format / temperature / …）不解析，
// 而是原样转发给上游 —— 上游本身就是 OpenAI 兼容协议，无需转换。
type chatRequest struct {
	Model    string `json:"model"`
	Stream   bool   `json:"stream"`
	Messages []struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	} `json:"messages"`
}

// handleChatCompletions 是 /v1/chat/completions 的主入口。
func (s *Server) handleChatCompletions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "仅支持 POST", "invalid_request_error")
		return
	}
	start := time.Now()

	body, err := io.ReadAll(io.LimitReader(r.Body, 32<<20))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "读取请求体失败: "+err.Error(), "invalid_request_error")
		return
	}

	var req chatRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "请求体解析失败: "+err.Error(), "invalid_request_error")
		return
	}
	if len(req.Messages) == 0 {
		writeErr(w, http.StatusBadRequest, "messages 不能为空", "invalid_request_error")
		return
	}

	settings := s.Store.Settings()
	modelName := strings.TrimSpace(req.Model)
	if settings.ForceModel != "" {
		modelName = settings.ForceModel
	}
	if modelName == "" {
		modelName = settings.DefaultModel
	}
	model := s.Reg.Resolve(modelName, settings.ModelAliases)

	upBody, err := rewriteModel(body, model.ID, req.Stream)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "请求体改写失败: "+err.Error(), "invalid_request_error")
		return
	}

	key := extractKey(r)
	timeout := time.Duration(settings.RequestTimeout) * time.Second
	if timeout <= 0 {
		timeout = 300 * time.Second
	}
	promptLen := estimatePromptLen(body)

	if req.Stream {
		s.streamChat(w, r, upBody, model, key, timeout, start, promptLen)
		return
	}
	s.blockingChat(w, r, upBody, model, key, timeout, start, promptLen)
}

// blockingChat 收集完整回复后一次性返回。
func (s *Server) blockingChat(w http.ResponseWriter, r *http.Request, body []byte, model registry.Model, key string, timeout time.Duration, start time.Time, promptLen int) {
	var (
		raw    []byte
		outLen int
	)

	acct, err := s.withAccountRetry(r.Context(), timeout, func(ctx context.Context, acct *codearts.Account) error {
		res, err := s.chatOnce(ctx, acct, model.ID, body)
		if err != nil {
			return err
		}
		raw = res
		return nil
	})
	if err != nil {
		s.record(key, acct, model.ID, false, http.StatusBadGateway, start, promptLen, 0, err)
		writeErr(w, http.StatusBadGateway, err.Error(), "upstream_error")
		return
	}

	outLen = completionLen(raw)
	s.record(key, acct, model.ID, false, http.StatusOK, start, promptLen, outLen, nil)

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(raw)
}

// streamChat 以 OpenAI SSE 格式流式返回（上游本身就是 SSE，做透明转发 + 错误识别）。
func (s *Server) streamChat(w http.ResponseWriter, r *http.Request, body []byte, model registry.Model, key string, timeout time.Duration, start time.Time, promptLen int) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeErr(w, http.StatusInternalServerError, "服务端不支持流式响应", "server_error")
		return
	}

	// 先尝试建立上游连接；失败时还能以普通 HTTP 错误返回。
	var (
		started   bool
		outLen    int
		streamErr error
	)

	acct, err := s.withAccountRetry(r.Context(), timeout, func(ctx context.Context, acct *codearts.Account) error {
		return s.streamOnce(ctx, acct, model.ID, body, func(chunk []byte) error {
			if !started {
				h := w.Header()
				h.Set("Content-Type", "text/event-stream; charset=utf-8")
				h.Set("Cache-Control", "no-cache")
				h.Set("Connection", "keep-alive")
				h.Set("X-Accel-Buffering", "no")
				h.Set("X-Content-Type-Options", "nosniff")
				w.WriteHeader(http.StatusOK)
				started = true
			}
			outLen += deltaLen(chunk)
			if _, err := fmt.Fprintf(w, "data: %s\n\n", chunk); err != nil {
				return err
			}
			flusher.Flush()
			return nil
		})
	})
	streamErr = err

	if !started {
		// 上游连一次都没成功：按普通错误返回，客户端能拿到明确的状态码
		status := http.StatusBadGateway
		msg := "全部账号均不可用"
		if err != nil {
			msg = err.Error()
		}
		s.record(key, acct, model.ID, true, status, start, promptLen, outLen, err)
		writeErr(w, status, msg, "upstream_error")
		return
	}

	if streamErr != nil {
		// 已经在流里了，只能以 SSE 错误块收尾
		errChunk, _ := json.Marshal(map[string]any{
			"error": map[string]any{
				"message": streamErr.Error(),
				"type":    "upstream_error",
			},
		})
		_, _ = fmt.Fprintf(w, "data: %s\n\n", errChunk)
	}
	_, _ = io.WriteString(w, "data: [DONE]\n\n")
	flusher.Flush()

	s.record(key, acct, model.ID, true, statusOf(streamErr), start, promptLen, outLen, streamErr)
}

// chatOnce 用指定账号发一次非流式请求。
func (s *Server) chatOnce(ctx context.Context, acct *codearts.Account, model string, body []byte) ([]byte, error) {
	sess, err := s.Cred.Session(ctx, acct, s.resolveOptions())
	if err != nil {
		return nil, err
	}
	raw, err := s.Up.Chat(ctx, sess, model, body)
	if err != nil {
		return nil, err
	}
	if msg, bad := codearts.IsUpstreamBusinessError(raw); bad {
		return nil, fmt.Errorf("上游错误: %s", msg)
	}
	return raw, nil
}

// streamOnce 用指定账号发一次流式请求，逐块回调。
func (s *Server) streamOnce(ctx context.Context, acct *codearts.Account, model string, body []byte, onChunk func([]byte) error) error {
	sess, err := s.Cred.Session(ctx, acct, s.resolveOptions())
	if err != nil {
		return err
	}
	return s.Up.ChatStream(ctx, sess, model, body, func(chunk []byte) error {
		if msg, bad := codearts.IsUpstreamBusinessError(chunk); bad {
			return fmt.Errorf("上游错误: %s", msg)
		}
		return onChunk(chunk)
	})
}

// withAccountRetry 选账号并在失败时换号重试。
//
// 返回本次实际使用的账号（可能为 nil），供调用方记录日志归属。
func (s *Server) withAccountRetry(ctx context.Context, timeout time.Duration, fn func(context.Context, *codearts.Account) error) (*codearts.Account, error) {
	accts := s.Store.Accounts()
	if len(accts) == 0 {
		return nil, errNoAccount
	}
	maxTries := s.Store.Settings().MaxRetries
	if maxTries <= 0 {
		maxTries = 3
	}
	if maxTries > len(accts) {
		maxTries = len(accts)
	}

	var lastErr error
	var used *codearts.Account
	for i := 0; i < maxTries; i++ {
		acct := s.Store.NextAccount()
		if acct == nil {
			break
		}
		used = acct
		cctx, cancel := context.WithTimeout(ctx, timeout)
		err := fn(cctx, acct)
		cancel()

		if err == nil {
			s.Store.MarkAccountResult(acct.ID, true, "")
			return acct, nil
		}
		lastErr = err
		// 客户端主动断开（curl/浏览器提前关连接）不是账号故障，不应计入失败
		if ctx.Err() != nil {
			break
		}
		s.Store.MarkAccountResult(acct.ID, false, err.Error())
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("全部账号均不可用")
	}
	return used, lastErr
}

// rewriteModel 把请求体里的 model 换成解析后的上游模型 ID。
//
// 其余字段原样保留，避免丢失 tools / response_format 等上游已支持的能力。
func rewriteModel(body []byte, model string, stream bool) ([]byte, error) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, err
	}
	mb, err := json.Marshal(model)
	if err != nil {
		return nil, err
	}
	m["model"] = mb
	sb, err := json.Marshal(stream)
	if err != nil {
		return nil, err
	}
	m["stream"] = sb
	return json.Marshal(m)
}

// record 记录一次请求结果。
func (s *Server) record(key string, acct *codearts.Account, model string, stream bool, status int, start time.Time, inLen, outLen int, err error) {
	e := store.LogEntry{
		Time:      time.Now().Unix(),
		Key:       codearts.Mask(key),
		Model:     model,
		Stream:    stream,
		Status:    status,
		LatencyMS: time.Since(start).Milliseconds(),
		PromptLen: inLen,
		OutputLen: outLen,
	}
	if acct != nil {
		e.Account = acct.DisplayName()
	}
	if err != nil {
		e.Err = err.Error()
	}
	s.Store.Record(e, outLen)
}

// statusOf 把错误映射为 HTTP 状态码。
func statusOf(err error) int {
	if err == nil {
		return http.StatusOK
	}
	return http.StatusBadGateway
}

// estimatePromptLen 粗略估算输入规模（上游不返回 prompt token 数时使用）。
func estimatePromptLen(body []byte) int {
	return len(body) / 4
}

// completionLen 统计非流式响应里的输出字符数。
func completionLen(raw []byte) int {
	var resp struct {
		Choices []struct {
			Message struct {
				Content          string `json:"content"`
				ReasoningContent string `json:"reasoning_content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return 0
	}
	n := 0
	for _, c := range resp.Choices {
		n += len([]rune(c.Message.Content)) + len([]rune(c.Message.ReasoningContent))
	}
	return n
}

// deltaLen 统计一个流式分片里的输出字符数。
func deltaLen(chunk []byte) int {
	var c struct {
		Choices []struct {
			Delta struct {
				Content          string `json:"content"`
				ReasoningContent string `json:"reasoning_content"`
			} `json:"delta"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(chunk, &c); err != nil {
		return 0
	}
	n := 0
	for _, ch := range c.Choices {
		n += len([]rune(ch.Delta.Content)) + len([]rune(ch.Delta.ReasoningContent))
	}
	return n
}
