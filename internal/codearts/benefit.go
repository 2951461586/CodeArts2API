package codearts

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// 免费福利 Token 的领取接口。
//
// 复刻 extensions/vscode-codebot/out/extension.js 里的 B3c()：
//
//	GET https://opengw.developer.huaweicloud.com/api/v1/benefit/claim
//	GET https://opengw.developer.huaweicloud.com/api/v1/benefit/claim?channel=education
//
// 领取是无副作用的幂等 GET：当天已领过再领一次仍返回 0000 success，
// 额度不变。官方活动页说明「登录成功后客户端会自动为你领取该福利」，
// 因此 IDE 运行时其实已经替我们领了 —— 网关的自动签到是
// 「IDE 没开 / 换账号 / 跨天」时的兜底。

// BenefitGatewayConfig 报告福利通道是否开放。
type BenefitGatewayConfig struct {
	Enabled                 bool `json:"enabled"`
	IsExaminationDomain     bool `json:"is_examination_domain"`
	ExaminationModelEnabled bool `json:"examination_model_enable"`
}

// BenefitClaim 是领取结果。
type BenefitClaim struct {
	Channel  string `json:"channel"`
	DomainID string `json:"domain_id"`
	UserID   string `json:"user_id"`
	UserName string `json:"user_name"`
	CreateAt int64  `json:"create_time"`
	UpdateAt int64  `json:"update_time"`
}

// FetchBenefitGatewayConfig 查询福利通道是否开放（普通 / 教育 / 考试）。
func (c *Client) FetchBenefitGatewayConfig(ctx context.Context, sess *Session, channel string) (*BenefitGatewayConfig, error) {
	h := c.baseHeaders("", ChannelCredit)
	h["Agent-Type"] = "PromptCenter"
	path := BenefitGatewayConfigPath
	if channel != "" {
		h["channel"] = channel
	}
	resp, err := c.do(ctx, sess, http.MethodGet, SnapHost, path, nil, h)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, &UpstreamError{Status: resp.StatusCode, Message: truncate(string(raw), 300)}
	}
	var cfg BenefitGatewayConfig
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return nil, fmt.Errorf("福利通道配置解析失败: %w", err)
	}
	return &cfg, nil
}

// ClaimBenefit 领取当日的免费福利 Token。
//
// channel 为空时领默认（codearts）通道；传 "education" / "examination"
// 可领对应的特殊通道（普通账号会被上游拒绝，属预期）。
func (c *Client) ClaimBenefit(ctx context.Context, sess *Session, channel string) (*BenefitClaim, error) {
	path := BenefitClaimPath
	if channel != "" {
		path += "?channel=" + channel
	}
	resp, err := c.do(ctx, sess, http.MethodGet, BenefitHost, path, nil, c.baseHeaders("", ChannelCredit))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, &UpstreamError{Status: resp.StatusCode, Message: truncate(string(raw), 300)}
	}

	var env gatewayEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("领取结果解析失败: %w", err)
	}
	if env.ErrorCode != "" && env.ErrorCode != "0000" {
		return nil, &UpstreamError{
			Status:  http.StatusOK,
			Message: env.ErrorCode + ": " + env.ErrorMsg,
		}
	}
	var claim BenefitClaim
	if len(env.Result) > 0 {
		if err := json.Unmarshal(env.Result, &claim); err != nil {
			return nil, fmt.Errorf("领取结果解析失败: %w", err)
		}
	}
	if claim.Channel == "" {
		claim.Channel = "codearts"
	}
	return &claim, nil
}

// ClaimResult 是一次自动签到的结果。
type ClaimResult struct {
	OK      bool
	Channel string
	Message string
}

// ClaimDaily 执行一次「每日签到」：先确认福利通道开放，再领取。
func (c *Client) ClaimDaily(ctx context.Context, sess *Session) (*ClaimResult, error) {
	cfg, err := c.FetchBenefitGatewayConfig(ctx, sess, "")
	if err != nil {
		return nil, err
	}
	if !cfg.Enabled {
		return &ClaimResult{OK: false, Message: "当前账号未开放免费福利 Token 活动"}, nil
	}
	claim, err := c.ClaimBenefit(ctx, sess, "")
	if err != nil {
		return nil, err
	}
	msg := "已领取当日免费福利 Token（每日 1000 万，当日 24:00 清零）"
	if claim.UserName != "" {
		msg = "已为 " + claim.UserName + " 领取当日免费福利 Token"
	}
	return &ClaimResult{OK: true, Channel: claim.Channel, Message: msg}, nil
}

// DescribeClaimError 把签到错误翻译成一句人话。
func DescribeClaimError(err error) string {
	if err == nil {
		return ""
	}
	msg := err.Error()
	switch {
	case strings.Contains(msg, "APIG.0602"):
		return "凭据（STS security token）已过期，无法签到"
	case strings.Contains(msg, "APIG.0301"), strings.Contains(msg, "APIG.0303"):
		return "签名校验失败，凭据可能已失效"
	case strings.Contains(msg, "4006"):
		return "今日名额已用完"
	case strings.Contains(msg, "4004"):
		return "权益不存在或已过期"
	case strings.Contains(msg, "4003"):
		return "账号已被禁用"
	}
	return msg
}
