package codearts

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"
)

// 上游固定端点。取值来自 extensions/vscode-codebot/product.json
// 的 commercialVersionDomain.newFramework.productDomain。
const (
	// SnapHost 是推理（inferhub）与大多数业务 API 的入口。
	SnapHost = "https://snap-access.cn-north-4.myhuaweicloud.com"
	// SnapHostBackup 是备用域名（.cn 后缀，与主域名等价）。
	SnapHostBackup = "https://snap-access.cn-north-4.myhuaweicloud.cn"
	// BenefitHost 提供免费权益（benefit 通道）的模型清单与额度。
	BenefitHost = "https://opengw.developer.huaweicloud.com"
	// IAMTokenURL 是 STS / IAM 令牌端点（登录与续期）。
	IAMTokenURL = "https://sts.cn-north-4.myhuaweicloud.com/v1/oauth2/tokens"
	// ClientID 是 OAuth 客户端标识（= product.json 的 urlProtocol）。
	ClientID = "codearts-agent"

	// ChatPath 是 OpenAI 兼容的推理入口。
	ChatPath = "/api/v2/chat/completions"
	// GatewayConfigPath 返回 benefit 通道的可用模型与 base_url。
	GatewayConfigPath = "/api/v1/gateway/config"
	// BalancePath 返回每日 / 每月 token 额度。
	BalancePath = "/api/v1/user/tokens/balance"
	// BuiltinModelsPath 返回订阅 / 企业内置模型（需 Agent-Type: PromptCenter）。
	BuiltinModelsPath = "/v1/model/builtin"
	// BenefitGatewayConfigPath 报告 benefit 通道是否开放。
	BenefitGatewayConfigPath = "/v1/benefit-gateway-config"
	// BenefitClaimPath 是每日免费福利 Token 的领取入口（幂等 GET）。
	BenefitClaimPath = "/api/v1/benefit/claim"

	// PluginName / PluginVersion 等身份头，服务端会据此做路由与埋点。
	// 缺省值与 CodeArts Agent 26.9.902 一致。
	PluginName    = "codemate_vscode"
	PluginVersion = "26.9.902"
	IDEName       = "VSCode"
	IDEVersion    = "26.9.902"
	AppID         = "CodeAgent3.0"
)

// Channel 是上游的路由通道。
//
// 实测（2026-10）：CodeArts inferhub 用请求头 maas_type 区分两条通道 ——
// 同一个模型名在不同通道下是否可用、是否计费都不同。
type Channel string

const (
	// ChannelBenefit 走每日免费权益额度：请求头带 `maas_type: benefit`。
	// 额度见 /api/v1/user/tokens/balance 的 daily_token_limit。
	ChannelBenefit Channel = "benefit"
	// ChannelCredit 走订阅 / 积分通道：**不带** maas_type 头，按积分计费。
	// 例如 GLM-5.3 / GLM-5.2 / openpangu-2.0-pro 都属于这一类。
	ChannelCredit Channel = "credit"
)

// Client 是 CodeArts 上游 HTTP 客户端。
type Client struct {
	HTTP *http.Client
	// Identity 是可覆盖的上游身份头（为空时用内置缺省值）。
	Identity Identity
}

// Identity 是发给上游的插件 / IDE 身份标识。
type Identity struct {
	PluginName    string `json:"plugin_name"`
	PluginVersion string `json:"plugin_version"`
	IDEName       string `json:"ide_name"`
	IDEVersion    string `json:"ide_version"`
	AppID         string `json:"app_id"`
	Language      string `json:"language"`
}

// DefaultIdentity 返回与 CodeArts Agent 26.9.902 对齐的身份头。
func DefaultIdentity() Identity {
	return Identity{
		PluginName:    PluginName,
		PluginVersion: PluginVersion,
		IDEName:       IDEName,
		IDEVersion:    IDEVersion,
		AppID:         AppID,
		Language:      "zh-cn",
	}
}

// NewClient 构造上游客户端。
func NewClient() *Client {
	transport := &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   15 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		MaxIdleConns:          64,
		MaxIdleConnsPerHost:   16,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   15 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		ForceAttemptHTTP2:     true,
	}
	return &Client{
		HTTP: &http.Client{
			Transport: transport,
			// 流式响应可能持续数分钟，不设总超时；由 context 控制
		},
		Identity: DefaultIdentity(),
	}
}

// baseHeaders 构造一次请求的公共头（含身份头与 tracing 头）。
//
// channel 决定是否带 `maas_type: benefit`：benefit 走免费权益额度，
// credit 不带该头从而落到积分通道。
func (c *Client) baseHeaders(model string, channel Channel) map[string]string {
	id := c.Identity
	if id.PluginName == "" {
		id = DefaultIdentity()
	}
	traceID := RandomHex(16)
	spanID := RandomHex(8)
	h := map[string]string{
		"content-type":           "application/json",
		"app-id":                 id.AppID,
		"x-ot-function":          "agent-general",
		"x-ot-trace-id":          traceID,
		"x-ot-span-id":           spanID,
		"x-snap-traceid":         traceID + "_" + RandomHex(8),
		"x-ot-client-type":       "IDE",
		"x-ot-client-version":    id.IDEVersion,
		"plugin-name":            id.PluginName,
		"plugin-version":         id.PluginVersion,
		"ide-name":               id.IDEName,
		"ide-version":            id.IDEVersion,
		"created-time":           time.Now().UTC().Format("2006-01-02T15:04:05Z"),
		"user-msg-id":            "msg_" + RandomHex(12),
		"x-ot-session-id":        "ses_" + RandomHex(12),
		"x-ot-parent-session-id": "",
		"user-session-id":        "ses_" + RandomHex(12),
		"is_confidential":        "false",
	}
	if id.Language != "" {
		h["X-Language"] = id.Language
	}
	if channel == ChannelBenefit {
		h["maas_type"] = string(ChannelBenefit)
	}
	if model != "" {
		// 上游会校验 model-id 与请求体里的 model 完全一致（不一致报 406），
		// 因此这里必须与 rewriteModel 写入的值相同。
		h["model-id"] = model
		h["model-name"] = model
	}
	return h
}

// do 签名并发送一次请求，返回原始响应。
//
// 主域名连接失败时会自动回退到 .cn 备用域名。
func (c *Client) do(ctx context.Context, sess *Session, method, host, path string, body []byte, headers map[string]string) (*http.Response, error) {
	if sess == nil || sess.AccessKey == "" || sess.SecretKey == "" {
		return nil, fmt.Errorf("账号凭据缺失或已过期，请重新登录 CodeArts Agent 或手工录入凭据")
	}
	var lastErr error
	for _, h := range hostCandidates(host) {
		rawURL := h + path
		signed, err := signRequest(sess.AccessKey, sess.SecretKey, sess.SecurityToken, method, rawURL, body, headers)
		if err != nil {
			return nil, err
		}
		var rdr io.Reader
		if len(body) > 0 {
			rdr = bytes.NewReader(body)
		}
		req, err := http.NewRequestWithContext(ctx, method, rawURL, rdr)
		if err != nil {
			return nil, err
		}
		for k, v := range signed {
			req.Header.Set(k, v)
		}
		resp, err := c.HTTP.Do(req)
		if err != nil {
			lastErr = err
			continue
		}
		return resp, nil
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("上游不可达")
	}
	return nil, lastErr
}

// hostCandidates 返回主域名与备用域名。
func hostCandidates(host string) []string {
	if host == SnapHost {
		return []string{SnapHost, SnapHostBackup}
	}
	return []string{host}
}

// GatewayConfig 是 benefit 通道的模型清单。
type GatewayConfig struct {
	BaseURL string         `json:"base_url"`
	Models  []GatewayModel `json:"models"`
}

// GatewayModel 是 benefit 通道里的一个模型。
type GatewayModel struct {
	ModelID       string `json:"model_id"`
	ModelName     string `json:"model_name"`
	ContextWindow int    `json:"context_window"`
	MaxTokens     int    `json:"max_tokens"`
	Sort          int    `json:"sort"`
}

// gatewayEnvelope 是 benefit 接口的统一信封。
type gatewayEnvelope struct {
	ErrorCode string          `json:"error_code"`
	ErrorMsg  string          `json:"error_msg"`
	Result    json.RawMessage `json:"result"`
}

// FetchGatewayConfig 拉取 benefit 通道可用模型。
func (c *Client) FetchGatewayConfig(ctx context.Context, sess *Session) (*GatewayConfig, error) {
	resp, err := c.do(ctx, sess, http.MethodGet, BenefitHost, GatewayConfigPath, nil, c.baseHeaders("", ChannelCredit))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("获取模型列表失败 HTTP %d: %s", resp.StatusCode, truncate(string(raw), 300))
	}
	var env gatewayEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("模型列表解析失败: %w", err)
	}
	if env.ErrorCode != "" && env.ErrorCode != "0000" {
		return nil, fmt.Errorf("获取模型列表失败 %s: %s", env.ErrorCode, env.ErrorMsg)
	}
	var cfg GatewayConfig
	if err := json.Unmarshal(env.Result, &cfg); err != nil {
		return nil, fmt.Errorf("模型列表解析失败: %w", err)
	}
	return &cfg, nil
}

// TokensBalance 是每日 / 每月 token 额度。
type TokensBalance struct {
	Channel           string `json:"channel"`
	DailyTokenLimit   int64  `json:"daily_token_limit"`
	DailyTokensUsed   int64  `json:"daily_tokens_used"`
	MonthlyTokenLimit int64  `json:"monthly_token_limit"`
	MonthlyTokensUsed int64  `json:"monthly_tokens_used"`
	TotalBalance      int64  `json:"total_balance"`
	TotalQuota        int64  `json:"total_quota"`
	UsedAmount        int64  `json:"used_amount"`
	UpdateTime        int64  `json:"update_time"`
	ExpireTime        int64  `json:"expire_time"`
}

// FetchBalance 查询额度余额。
func (c *Client) FetchBalance(ctx context.Context, sess *Session) (*TokensBalance, error) {
	resp, err := c.do(ctx, sess, http.MethodGet, BenefitHost, BalancePath, nil, c.baseHeaders("", ChannelCredit))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("查询额度失败 HTTP %d: %s", resp.StatusCode, truncate(string(raw), 300))
	}
	var env gatewayEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("额度解析失败: %w", err)
	}
	if env.ErrorCode != "" && env.ErrorCode != "0000" {
		return nil, fmt.Errorf("查询额度失败 %s: %s", env.ErrorCode, env.ErrorMsg)
	}
	var bal TokensBalance
	if err := json.Unmarshal(env.Result, &bal); err != nil {
		return nil, fmt.Errorf("额度解析失败: %w", err)
	}
	return &bal, nil
}

// BuiltinModel 是订阅 / 企业内置模型。
//
// 这些模型走**积分（计费）通道** —— 请求时不带 maas_type 头，按积分计费。
type BuiltinModel struct {
	ModelID        string       `json:"model_id"`
	ModelName      string       `json:"model_name"`
	ModelCategory  string       `json:"model_category"`
	ModelDesc      string       `json:"model_desc"`
	Enable         bool         `json:"enable"`
	DisplayEnabled bool         `json:"display_enabled"`
	SupportsImages bool         `json:"supports_images"`
	ChannelSource  string       `json:"channel_source"`
	Credit         []CreditTier `json:"credit"`
}

// CreditTier 是模型的分段计费档位。
//
// 实测取值形如：ratio=0.05、input_price=8、cache_price=2、output_price=28、
// ratio_display=0.7x —— ratio_display 即用户看到的积分消耗倍率。
type CreditTier struct {
	Ratio        string `json:"ratio"`
	InputPrice   string `json:"input_price"`
	CachePrice   string `json:"cache_price"`
	OutputPrice  string `json:"output_price"`
	RatioDisplay string `json:"ratio_display"`
	Status       string `json:"status"`
}

// CreditRatio 返回首档积分倍率的展示值（如 "0.79x"），无计费信息时返回空串。
func (m BuiltinModel) CreditRatio() string {
	for _, c := range m.Credit {
		if c.RatioDisplay != "" {
			return c.RatioDisplay
		}
	}
	return ""
}

// IsBillable 报告该模型是否会消耗积分。
func (m BuiltinModel) IsBillable() bool { return len(m.Credit) > 0 }

// FetchBuiltinModels 拉取内置模型清单（需要 Agent-Type: PromptCenter）。
func (c *Client) FetchBuiltinModels(ctx context.Context, sess *Session) ([]BuiltinModel, error) {
	h := c.baseHeaders("", ChannelCredit)
	h["Agent-Type"] = "PromptCenter"
	resp, err := c.do(ctx, sess, http.MethodGet, SnapHost, BuiltinModelsPath, nil, h)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("获取内置模型失败 HTTP %d: %s", resp.StatusCode, truncate(string(raw), 300))
	}
	var out struct {
		Count         int            `json:"count"`
		BuiltinModels []BuiltinModel `json:"builtinModels"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("内置模型解析失败: %w", err)
	}
	return out.BuiltinModels, nil
}

// ChatStream 向 inferhub 发起流式对话，逐块回调。
//
// 上游返回标准 OpenAI SSE（data: {chat.completion.chunk}），
// 因此这里只做「拆包 + 错误识别」，不做任何协议转换。
func (c *Client) ChatStream(ctx context.Context, sess *Session, model string, channel Channel, body []byte, onChunk func([]byte) error) error {
	h := c.baseHeaders(model, channel)
	resp, err := c.do(ctx, sess, http.MethodPost, SnapHost, ChatPath, body, h)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		return &UpstreamError{Status: resp.StatusCode, Message: truncate(string(raw), 600)}
	}

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 4<<20)
	for scanner.Scan() {
		line := bytes.TrimRight(scanner.Bytes(), "\r")
		if len(line) == 0 {
			continue
		}
		if !bytes.HasPrefix(line, []byte("data:")) {
			continue
		}
		payload := bytes.TrimSpace(line[len("data:"):])
		if len(payload) == 0 {
			continue
		}
		if bytes.Equal(payload, []byte("[DONE]")) {
			return nil
		}
		if err := onChunk(payload); err != nil {
			return err
		}
	}
	return scanner.Err()
}

// Chat 向 inferhub 发起非流式对话，返回原始响应体。
func (c *Client) Chat(ctx context.Context, sess *Session, model string, channel Channel, body []byte) ([]byte, error) {
	h := c.baseHeaders(model, channel)
	resp, err := c.do(ctx, sess, http.MethodPost, SnapHost, ChatPath, body, h)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, &UpstreamError{Status: resp.StatusCode, Message: truncate(string(raw), 600)}
	}
	return raw, nil
}

// UpstreamError 是上游返回的错误。
type UpstreamError struct {
	Status  int
	Message string
}

// Error 实现 error。
func (e *UpstreamError) Error() string {
	return fmt.Sprintf("上游 HTTP %d: %s", e.Status, e.Message)
}

// Probe 用一个最小请求探测凭据是否可用。
//
// 默认走免费权益通道，避免探活本身消耗积分。
func (c *Client) Probe(ctx context.Context, sess *Session, model string, channel Channel) error {
	if model == "" {
		model = "glm-5.3-flash"
		channel = ChannelBenefit
	}
	if channel == "" {
		channel = ChannelBenefit
	}
	body, err := json.Marshal(map[string]any{
		"model":      model,
		"messages":   []map[string]string{{"role": "user", "content": "hi"}},
		"max_tokens": 8,
		"stream":     false,
	})
	if err != nil {
		return err
	}
	raw, err := c.Chat(ctx, sess, model, channel, body)
	if err != nil {
		return err
	}
	return detectUpstreamError(raw)
}

// detectUpstreamError 识别「HTTP 200 但业务失败」的响应。
//
// 上游对未知模型等错误会返回 200 + {"error_code":"4005","error_msg":"unsupported model"}，
// 不识别的话会被当成正常回复透传给客户端。
func detectUpstreamError(raw []byte) error {
	var probe struct {
		ErrorCode string          `json:"error_code"`
		ErrorMsg  string          `json:"error_msg"`
		Choices   json.RawMessage `json:"choices"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil {
		return nil
	}
	if probe.ErrorCode == "" {
		return nil
	}
	if len(probe.Choices) > 0 && string(probe.Choices) != "null" {
		return nil
	}
	return &UpstreamError{Status: http.StatusOK, Message: probe.ErrorCode + ": " + probe.ErrorMsg}
}

// truncate 截断长字符串，用于错误信息。
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// IsUpstreamBusinessError 报告一个 SSE 分片是否是业务错误。
func IsUpstreamBusinessError(payload []byte) (string, bool) {
	var probe struct {
		ErrorCode string `json:"error_code"`
		ErrorMsg  string `json:"error_msg"`
	}
	if err := json.Unmarshal(payload, &probe); err != nil {
		return "", false
	}
	if probe.ErrorCode == "" {
		return "", false
	}
	msg := probe.ErrorMsg
	if msg == "" {
		msg = "上游返回业务错误 " + probe.ErrorCode
	}
	return msg, true
}

// EnsureTrimmed 去掉字符串两端空白（供调用方复用）。
func EnsureTrimmed(s string) string { return strings.TrimSpace(s) }
