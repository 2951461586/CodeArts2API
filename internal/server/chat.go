package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"codearts2api/internal/apierr"
	"codearts2api/internal/codearts"
	"codearts2api/internal/protocol"
	"codearts2api/internal/registry"
	"codearts2api/internal/store"
)

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
	if model.IsCredit() && !settings.AllowCreditModels {
		writeErr(w, http.StatusForbidden,
			"模型 "+model.ID+" 走积分（计费）通道，当前已在控制台关闭。如需使用请开启「允许积分模型」。",
			"invalid_request_error")
		return
	}

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
	promptLen := protocol.EstimatePromptLen(body)

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
		res, err := s.chatOnce(ctx, acct, model, body)
		if err != nil {
			return err
		}
		raw = res
		return nil
	})
	if err != nil {
		e := apierr.From(err)
		s.record(key, acct, model.ID, false, e.Status, start, promptLen, 0, err)
		writeErr(w, e.Status, err.Error(), e.OpenAI)
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
		return s.streamOnce(ctx, acct, model, body, func(chunk []byte) error {
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
		e := apierr.From(err)
		msg := "全部账号均不可用"
		if err != nil {
			msg = err.Error()
		}
		s.record(key, acct, model.ID, true, e.Status, start, promptLen, outLen, err)
		writeErr(w, e.Status, msg, e.OpenAI)
		return
	}

	if streamErr != nil {
		// 已经在流里了，只能以 SSE 错误块收尾（OpenAI 客户端只看 error 对象）
		e := apierr.From(streamErr)
		errChunk, _ := json.Marshal(map[string]any{
			"error": map[string]any{
				"message": streamErr.Error(),
				"type":    e.OpenAI,
				"code":    e.Status,
			},
		})
		_, _ = fmt.Fprintf(w, "data: %s\n\n", errChunk)
	}
	_, _ = io.WriteString(w, "data: [DONE]\n\n")
	flusher.Flush()

	s.record(key, acct, model.ID, true, apierr.From(streamErr).Status, start, promptLen, outLen, streamErr)
}

// chatOnce 用指定账号发一次非流式请求。
//
// 上游对个别模型（实测 glm-5.3）的非流式接口只回一个空 body 的 400，
// 这里检测到该特征后自动改用「流式 + 本地聚合」，对客户端保持非流式语义。
func (s *Server) chatOnce(ctx context.Context, acct *codearts.Account, model registry.Model, body []byte) ([]byte, error) {
	sess, err := s.Cred.Session(ctx, acct, s.resolveOptions())
	if err != nil {
		return nil, err
	}

	if model.StreamOnly {
		return s.aggregateViaStream(ctx, sess, model, body)
	}

	raw, err := s.Up.Chat(ctx, sess, model.ID, model.Channel, body)
	if err != nil {
		if codearts.IsEmptyRejection(err) {
			s.Reg.MarkStreamOnly(model.ID)
			if agg, aerr := s.aggregateViaStream(ctx, sess, model, body); aerr == nil {
				return agg, nil
			} else {
				return nil, aerr
			}
		}
		return nil, err
	}
	if msg, bad := codearts.IsUpstreamBusinessError(raw); bad {
		return nil, codearts.UpstreamBusinessError(msg)
	}
	return raw, nil
}

// aggregateViaStream 用流式请求取回分片，再在本地聚合成一次性响应。
func (s *Server) aggregateViaStream(ctx context.Context, sess *codearts.Session, model registry.Model, body []byte) ([]byte, error) {
	streamBody, err := rewriteStream(body, true)
	if err != nil {
		return nil, err
	}
	var chunks [][]byte
	err = s.Up.ChatStream(ctx, sess, model.ID, model.Channel, streamBody, func(chunk []byte) error {
		if msg, bad := codearts.IsUpstreamBusinessError(chunk); bad {
			return codearts.UpstreamBusinessError(msg)
		}
		chunks = append(chunks, append([]byte(nil), chunk...))
		return nil
	})
	if err != nil {
		return nil, err
	}
	return codearts.AggregateSSE(chunks)
}

// streamOnce 用指定账号发一次流式请求，逐块回调。
func (s *Server) streamOnce(ctx context.Context, acct *codearts.Account, model registry.Model, body []byte, onChunk func([]byte) error) error {
	sess, err := s.Cred.Session(ctx, acct, s.resolveOptions())
	if err != nil {
		return err
	}
	return s.Up.ChatStream(ctx, sess, model.ID, model.Channel, body, func(chunk []byte) error {
		if msg, bad := codearts.IsUpstreamBusinessError(chunk); bad {
			return codearts.UpstreamBusinessError(msg)
		}
		return onChunk(chunk)
	})
}

// 重试相关常量。
//
// 尝试次数与账号数量**解耦**：上游最常见的失败是「每账号 3 个并发会话」打满
// （TM.00001041），此时账号本身是健康的，只要退避重试即可 —— 如果只有 1 个账号
// 就退化成「不重试」，客户端会直接拿到 502。
const (
	// defaultMaxAttempts 是设置里未配置时的尝试次数。
	defaultMaxAttempts = 3
	// maxAttemptsCap 是尝试次数上限，防止设置被填成很大时长时间占用连接。
	maxAttemptsCap = 8
	// transientBaseWait 是临时性故障的退避基数（第 n 次退避 = n * base）。
	// 上游会话释放需要时间，太短（曾经是 300ms）基本等于不退避。
	transientBaseWait = time.Second
	// transientMaxWait 是单次退避上限。
	transientMaxWait = 3 * time.Second
)

// transientBackoff 返回第 n 次临时性故障后的退避时长（n 从 1 开始）：
// base、2*base、…，封顶 max。抽成纯函数是为了让「退避序列」能被单测钉住 ——
// 之前这里只有一条 `n * base` 的内联表达式，退避写错（比如恒为 0）不会有任何
// 测试失败，只会在线上表现为「重试等于没退避」。
func transientBackoff(n int) time.Duration {
	if n <= 0 {
		return 0
	}
	return min(time.Duration(n)*transientBaseWait, transientMaxWait)
}

// waitRetry 是重试前的等待。测试会替换它以避免真的睡满 1~3 秒；
// 生产路径始终是「可被 ctx 打断的定时等待」。
var waitRetry = func(ctx context.Context, d time.Duration) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(d):
		return nil
	}
}

// withAccountRetry 选账号并在失败时重试。
//
// 返回本次实际使用的账号（可能为 nil），供调用方记录日志归属。
func (s *Server) withAccountRetry(ctx context.Context, timeout time.Duration, fn func(context.Context, *codearts.Account) error) (*codearts.Account, error) {
	accts := s.Store.Accounts()
	if len(accts) == 0 {
		return nil, apierr.NoAccount
	}
	attempts := s.Store.Settings().MaxRetries
	if attempts <= 0 {
		attempts = defaultMaxAttempts
	}
	if attempts > maxAttemptsCap {
		attempts = maxAttemptsCap
	}

	var lastErr error
	var used *codearts.Account
	transientWaits := 0
	for i := 0; i < attempts; i++ {
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
		// 临时性故障（并发上限 / 429 / 5xx）：账号本身没问题，退避后重试
		if codearts.IsTransientUpstream(err) {
			if i == attempts-1 {
				break
			}
			transientWaits++
			if err := waitRetry(ctx, transientBackoff(transientWaits)); err != nil {
				return used, lastErr
			}
			continue
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

// rewriteStream 只改写请求体里的 stream 字段。
func rewriteStream(body []byte, stream bool) ([]byte, error) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, err
	}
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
