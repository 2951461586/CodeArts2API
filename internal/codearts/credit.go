package codearts

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// CreditPath 是「账号积分套餐与用量」的查询入口（snap-manager 统计插件）。
//
// 注意：它不是已下线的 benefit 额度接口（/api/v1/user/tokens/balance）——
// 那条属于免费权益通道，2026-10 起一律回 `4000: invalid channel`。
// 这条是积分通道自己的统计接口，返回套餐总额 / 已用 / 剩余 / 到期时间。
//
// 上游实现见 CodeArts Agent 26.9.902 的 `getTstatisticsPluginUrl()`：
// `${chatDomain}/snap-manager/v1/statistics/plugin`。
const CreditPath = "/snap-manager/v1/statistics/plugin"

// creditMetric 是统计插件返回的一条指标。
//
// 不同指标的字段集不同：积分类指标用 package_credit_*，token 类指标用
// usage_token_num / package_token_amount。未出现的字段保持零值。
type creditMetric struct {
	Name        string   `json:"name"`
	Value       *float64 `json:"value"`
	Amount      float64  `json:"package_credit_amount"`
	Used        float64  `json:"package_credit_used"`
	Remain      *float64 `json:"package_credit_remain"`
	Expiring    float64  `json:"package_credit_expiring_amount"`
	UsageTokens int64    `json:"usage_token_num"`
	TotalTokens int64    `json:"package_token_amount"`
}

// creditPackage 是账号当前的套餐信息。
type creditPackage struct {
	Status          string `json:"status"`
	NameCN          string `json:"package_name_cn"`
	NameEN          string `json:"package_name_en"`
	SpecCode        string `json:"spec_code"`
	URL             string `json:"package_url"`
	IsTokenPackage  bool   `json:"is_token_package"`
	IsCreditPackage bool   `json:"is_credit_package"`
}

// CreditCategory 是积分的一个来源分类（基础 / 按需 / 赠送）。
type CreditCategory struct {
	Total    float64 `json:"total"`
	Used     float64 `json:"used"`
	Expiring float64 `json:"expiring"`
}

// CreditInfo 是一个账号的积分额度快照。
//
// Available=false 表示上游没有返回积分套餐（token 套餐、白名单或未开通积分）——
// 这不是错误，控制台应显示原因而不是「0 积分」。
type CreditInfo struct {
	Available bool   `json:"available"`
	Reason    string `json:"reason,omitempty"`

	// Total / Used / Remaining 是积分数量；Percent 是上游给的百分比（0–100）。
	Total     float64 `json:"total"`
	Used      float64 `json:"used"`
	Remaining float64 `json:"remaining"`
	Percent   float64 `json:"percent"`
	Expiring  float64 `json:"expiring"`

	ResetDate   string `json:"reset_date,omitempty"`
	Status      string `json:"status,omitempty"` // active / expired / frozen / unknown
	PackageName string `json:"package_name,omitempty"`
	SpecCode    string `json:"spec_code,omitempty"`
	PackageURL  string `json:"package_url,omitempty"`

	IsCreditPackage bool `json:"is_credit_package"`
	IsTokenPackage  bool `json:"is_token_package"`

	Basic    *CreditCategory `json:"basic,omitempty"`
	OnDemand *CreditCategory `json:"on_demand,omitempty"`
	Bonus    *CreditCategory `json:"bonus,omitempty"`

	// TokenUsed / TokenLimit 是 token 套餐的用量（积分套餐通常为 0）。
	TokenUsed  int64 `json:"token_used,omitempty"`
	TokenLimit int64 `json:"token_limit,omitempty"`
}

// FetchCreditInfo 查询该凭据的积分额度。
//
// 这是一次轻量的只读统计查询，不消耗积分、也不产生推理费用。
// 上游要求与其它 snap-manager 接口相同的 SDK-HMAC-SHA256 签名（凭据自带）。
func (c *Client) FetchCreditInfo(ctx context.Context, sess *Session) (*CreditInfo, error) {
	resp, err := c.do(ctx, sess, http.MethodGet, SnapHost, CreditPath, nil, c.baseHeaders(sess, ""))
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
	return parseCreditInfo(raw)
}

// parseCreditInfo 把统计插件的响应解析成对外的稳定形状。
func parseCreditInfo(raw []byte) (*CreditInfo, error) {
	var payload struct {
		Package creditPackage  `json:"package"`
		Metrics []creditMetric `json:"metrics"`
		EndDate string         `json:"end_date"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return nil, fmt.Errorf("积分额度解析失败: %w", err)
	}

	info := &CreditInfo{
		ResetDate:       strings.TrimSpace(payload.EndDate),
		Status:          creditPackageStatus(payload.Package.Status),
		PackageName:     firstNonEmpty(payload.Package.NameCN, payload.Package.NameEN),
		SpecCode:        payload.Package.SpecCode,
		PackageURL:      payload.Package.URL,
		IsCreditPackage: payload.Package.IsCreditPackage,
		IsTokenPackage:  payload.Package.IsTokenPackage,
	}

	total := findCreditMetric(payload.Metrics, "usageTotalPackageCredit")
	if total == nil {
		info.Reason = "上游未返回积分套餐（可能是 token 套餐、白名单或未开通积分）"
		return info, nil
	}

	info.Available = true
	info.Total = total.Amount
	info.Used = total.Used
	if total.Remain != nil {
		info.Remaining = *total.Remain
	} else {
		// 部分套餐只回总额与已用，剩余需要自己算。
		info.Remaining = info.Total - info.Used
	}
	if info.Remaining < 0 {
		info.Remaining = 0
	}
	if total.Value != nil {
		info.Percent = *total.Value
	}
	info.Expiring = total.Expiring

	info.Basic = creditCategory(findCreditMetric(payload.Metrics, "usageBasicPackageCredit"))
	info.OnDemand = creditCategory(findCreditMetric(payload.Metrics, "usageOnDemandPackageCredit"))
	info.Bonus = creditCategory(findCreditMetric(payload.Metrics, "usageBonusPackageCredit"))
	if tok := findCreditMetric(payload.Metrics, "usageTokenChatMessages"); tok != nil {
		info.TokenUsed = tok.UsageTokens
		info.TokenLimit = tok.TotalTokens
	}
	return info, nil
}

// findCreditMetric 按名字查找一条指标。
func findCreditMetric(metrics []creditMetric, name string) *creditMetric {
	for i := range metrics {
		if metrics[i].Name == name {
			return &metrics[i]
		}
	}
	return nil
}

// creditCategory 把一条分类指标转成 CreditCategory；指标缺失时返回 nil。
func creditCategory(m *creditMetric) *CreditCategory {
	if m == nil {
		return nil
	}
	return &CreditCategory{Total: m.Amount, Used: m.Used, Expiring: m.Expiring}
}

// creditPackageStatus 把上游套餐状态映射成控制台用的四个值。
func creditPackageStatus(s string) string {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "normal":
		return "active"
	case "closed":
		return "expired"
	case "freeze", "frozen":
		return "frozen"
	case "":
		return "unknown"
	default:
		return strings.ToLower(strings.TrimSpace(s))
	}
}

// firstNonEmpty 返回第一个非空字符串。
func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}
