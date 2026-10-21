package codearts

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// 运营活动（「活动福利」）子系统。
//
// 复刻 extensions/vscode-codebot/out/extension.js 里的 s6i：
//
//	GET  ${policyCenterDomain}/v1/ops/delivery?channel=IDE   列出活动与可领取状态
//	POST ${policyCenterDomain}/v1/ops/claim                  {campaignId, idempotentKey, channel}
//	POST ${policyCenterDomain}/v1/ops/confirm                确认到账
//	POST ${policyCenterDomain}/v1/ops/invite/generate        {campaignId} 生成邀请码
//
// 实测（2026-10）线上共有三个 campaign：
//
//	campaignId=1  USER_LOGIN         每日签到领1000积分   triggerEvent=user.login
//	campaignId=2  STUDENT_CERTIFIED  学生认证领4000积分   triggerEvent=student.certified
//	campaignId=3  INVITE_USER        推荐码上有礼         triggerEvent=invite.success
//
// 只有**每日签到**可以自动化领取：它由每天的时间推进触发，服务端到点会把
// status 置为 ELIGIBLE、claimable=true。另外两个必须由真实事件触发
// （真实好友注册 / 真实学生认证），服务端会直接拒绝直接领取：
//
//	{"code":40001,"message":"该活动类型需要先通过触发接口激活，无法直接领取"}
//
// 因此本包只做「服务端说能领就领」，绝不伪造触发事件。

// 活动状态取值。
const (
	WelfareEligible  = "ELIGIBLE"  // 可领取
	WelfareClaimed   = "CLAIMED"   // 已领取
	WelfareConfirmed = "CONFIRMED" // 已到账
	WelfareConsumed  = "CONSUMED"  // 已使用
	WelfareEntry     = "ENTRY"     // 需先完成前置动作（如学生认证）
)

// 领取结果码。
const (
	welfareOK          = 0
	welfareNotYetReady = 40001 // 尚未到权益刷新时间 / 需要先触发
)

// WelfareExtra 是活动的调度元信息。
type WelfareExtra struct {
	StartTime    string `json:"startTime"`
	EndTime      string `json:"endTime"`
	TriggerMode  string `json:"triggerMode"`
	TriggerEvent string `json:"triggerEvent"`
}

// WelfareCampaign 是一个运营活动。
type WelfareCampaign struct {
	CampaignID    int          `json:"campaignId"`
	Title         string       `json:"title"`
	Type          string       `json:"type"`
	BenefitAmount float64      `json:"benefitAmount"`
	BenefitUnit   string       `json:"benefitUnit"`
	Status        string       `json:"status"`
	Claimable     bool         `json:"claimable"`
	Description   string       `json:"description"`
	PendingCount  int          `json:"pendingCount"`
	PendingAmount float64      `json:"pendingTotalAmount"`
	Extra         WelfareExtra `json:"extra"`
}

// IsManualClaim 报告该活动是否靠「手动领取」推进（即可以自动化）。
func (w WelfareCampaign) IsManualClaim() bool {
	return w.Extra.TriggerMode == "MANUAL_CLAIM" || w.Extra.TriggerMode == ""
}

// IsAutoClaimable 报告当前是否可以安全地自动领取。
//
// 只有「服务端明确说 claimable」且「靠时间推进的手动领取活动」才满足。
// 邀请 / 学生认证这类必须由真实事件触发的活动永远不满足。
func (w WelfareCampaign) IsAutoClaimable() bool {
	if !w.Claimable {
		return false
	}
	switch w.Type {
	case "INVITE_USER", "STUDENT_CERTIFIED":
		// 需要真实事件触发，服务端也会拒绝直接领取
		return false
	}
	return w.IsManualClaim()
}

// WelfareDelivery 是活动福利列表。
type WelfareDelivery struct {
	Campaigns []WelfareCampaign
	Raw       json.RawMessage
}

// Claimable 返回当前可领取的活动。
func (d *WelfareDelivery) Claimable() []WelfareCampaign {
	var out []WelfareCampaign
	for _, c := range d.Campaigns {
		if c.IsAutoClaimable() {
			out = append(out, c)
		}
	}
	return out
}

// Find 按 campaignId 查找活动。
func (d *WelfareDelivery) Find(id int) (WelfareCampaign, bool) {
	for _, c := range d.Campaigns {
		if c.CampaignID == id {
			return c, true
		}
	}
	return WelfareCampaign{}, false
}

// welfareEnvelope 是 ops 接口的统一信封。
type welfareEnvelope struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data"`
}

// opsHeaders 是 ops 接口需要的请求头。
func (c *Client) opsHeaders(sess *Session) map[string]string {
	h := c.baseHeaders(sess, "")
	h["Agent-Type"] = "PromptCenter"
	return h
}

// FetchWelfareDelivery 拉取活动福利列表。
func (c *Client) FetchWelfareDelivery(ctx context.Context, sess *Session) (*WelfareDelivery, error) {
	resp, err := c.do(ctx, sess, http.MethodGet, SnapHost, WelfareDeliveryPath+"?channel=IDE", nil, c.opsHeaders(sess))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, &UpstreamError{Status: resp.StatusCode, Message: truncate(string(raw), 300)}
	}
	var env welfareEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("活动列表解析失败: %w", err)
	}
	if env.Code != welfareOK {
		return nil, &UpstreamError{
			Status:  http.StatusOK,
			Message: fmt.Sprintf("%d: %s", env.Code, env.Message),
		}
	}
	var data struct {
		Items []WelfareCampaign `json:"items"`
	}
	if len(env.Data) > 0 {
		if err := json.Unmarshal(env.Data, &data); err != nil {
			return nil, fmt.Errorf("活动列表解析失败: %w", err)
		}
	}
	return &WelfareDelivery{Campaigns: data.Items, Raw: env.Data}, nil
}

// WelfareClaimResult 是一次领取的结果。
type WelfareClaimResult struct {
	OK         bool
	Code       int
	Message    string
	CampaignID int
	Title      string
}

// ClaimWelfare 领取指定活动。
//
// 幂等：请求带 idempotentKey，重复调用不会重复发放。
func (c *Client) ClaimWelfare(ctx context.Context, sess *Session, campaignID int, title string) (*WelfareClaimResult, error) {
	body, err := json.Marshal(map[string]any{
		"campaignId":    campaignID,
		"idempotentKey": NewUUID(),
		"channel":       "IDE",
	})
	if err != nil {
		return nil, err
	}
	resp, err := c.do(ctx, sess, http.MethodPost, SnapHost, WelfareClaimPath, body, c.opsHeaders(sess))
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
	var env welfareEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("领取结果解析失败: %w", err)
	}
	return &WelfareClaimResult{
		OK:         env.Code == welfareOK,
		Code:       env.Code,
		Message:    env.Message,
		CampaignID: campaignID,
		Title:      title,
	}, nil
}

// ClaimDailyWelfare 领取全部「当前可领」的运营活动奖励。
//
// 只领服务端明确标记 claimable 的活动，因此不会碰到需要真实事件触发的
// 邀请 / 学生认证类活动。
func (c *Client) ClaimDailyWelfare(ctx context.Context, sess *Session) ([]*WelfareClaimResult, error) {
	delivery, err := c.FetchWelfareDelivery(ctx, sess)
	if err != nil {
		return nil, err
	}
	targets := delivery.Claimable()
	if len(targets) == 0 {
		return nil, nil
	}
	var out []*WelfareClaimResult
	for _, t := range targets {
		res, err := c.ClaimWelfare(ctx, sess, t.CampaignID, t.Title)
		if err != nil {
			return out, err
		}
		out = append(out, res)
	}
	return out, nil
}

// InviteCode 是邀请码 / 邀请链接。
type InviteCode struct {
	ID              int64  `json:"id"`
	InviteCode      string `json:"inviteCode"`
	InviteURL       string `json:"inviteUrl"`
	CampaignID      int    `json:"campaignId"`
	InviterUserID   string `json:"inviterUserId"`
	Status          string `json:"status"`
	MaxActivations  int    `json:"maxActivations"`
	ActivationCount int    `json:"activationCount"`
	Valid           bool   `json:"valid"`
	CreatedAt       string `json:"createdAt"`
	UpdatedAt       string `json:"updatedAt"`
}

// GenerateInviteCode 生成（或取回）专属邀请码与邀请链接。
//
// 注意：这里只负责**拿到链接**。邀请奖励必须由真实好友通过链接完成注册
// 才会产生 —— 服务端对直接领取邀请活动会返回
// 「该活动类型需要先通过触发接口激活，无法直接领取」。
func (c *Client) GenerateInviteCode(ctx context.Context, sess *Session, campaignID int) (*InviteCode, error) {
	body, err := json.Marshal(map[string]any{"campaignId": campaignID})
	if err != nil {
		return nil, err
	}
	resp, err := c.do(ctx, sess, http.MethodPost, SnapHost, WelfareInvitePath, body, c.opsHeaders(sess))
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
	var env welfareEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("邀请码解析失败: %w", err)
	}
	if env.Code != welfareOK {
		return nil, &UpstreamError{
			Status:  http.StatusOK,
			Message: fmt.Sprintf("%d: %s", env.Code, env.Message),
		}
	}
	var code InviteCode
	if err := json.Unmarshal(env.Data, &code); err != nil {
		return nil, fmt.Errorf("邀请码解析失败: %w", err)
	}
	return &code, nil
}

// DescribeClaimError 把领取类接口的错误翻译成一句人话。
func DescribeClaimError(err error) string {
	if err == nil {
		return ""
	}
	msg := err.Error()
	switch {
	case strings.Contains(msg, "APIG.0602"):
		return "凭据（STS security token）已过期，请重新登录"
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

// DescribeWelfareError 把领取错误翻译成一句人话。
func DescribeWelfareError(res *WelfareClaimResult) string {
	if res == nil {
		return ""
	}
	if res.OK {
		return res.Title + "：领取成功"
	}
	switch res.Code {
	case welfareNotYetReady:
		if strings.Contains(res.Message, "刷新时间") {
			return res.Title + "：今日已领，等下次权益刷新"
		}
		if strings.Contains(res.Message, "触发接口激活") {
			return res.Title + "：需要真实事件触发（如好友注册），无法直接领取"
		}
	}
	return res.Title + "：" + res.Message
}
