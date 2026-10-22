package codearts

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
)

// creditRoundTripFunc 把上游调用替换成内存应答。
type creditRoundTripFunc func(*http.Request) (*http.Response, error)

func (f creditRoundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func creditResp(r *http.Request, body string) *http.Response {
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(body)),
		Request:    r,
	}
}

// TestFetchCreditInfoParsesUsage 校验统计插件响应的解析：总额 / 已用 / 剩余 /
// 分类 / token 用量 / 重置日期。
func TestFetchCreditInfoParsesUsage(t *testing.T) {
	const body = `{
	  "package": {
	    "status": "normal",
	    "package_name_cn": "基础套餐",
	    "package_name_en": "Basic",
	    "spec_code": "codearts.basic",
	    "package_url": "https://example/package",
	    "is_credit_package": true,
	    "is_token_package": false
	  },
	  "metrics": [
	    {"name":"usageTotalPackageCredit","package_credit_amount":1000,"package_credit_used":250,
	     "package_credit_remain":750,"package_credit_expiring_amount":50,"value":25},
	    {"name":"usageBasicPackageCredit","package_credit_amount":800,"package_credit_used":200,
	     "package_credit_expiring_amount":0},
	    {"name":"usageOnDemandPackageCredit","package_credit_amount":200,"package_credit_used":50,
	     "package_credit_expiring_amount":50},
	    {"name":"usageTokenChatMessages","usage_token_num":1234,"package_token_amount":5678}
	  ],
	  "end_date": "2026-11-01"
	}`

	c := NewClient()
	var gotPath, gotMethod, gotAuth string
	c.HTTP = &http.Client{Transport: creditRoundTripFunc(func(r *http.Request) (*http.Response, error) {
		gotPath, gotMethod, gotAuth = r.URL.Path, r.Method, r.Header.Get("Authorization")
		return creditResp(r, body), nil
	})}

	info, err := c.FetchCreditInfo(context.Background(), &Session{AccessKey: "ak", SecretKey: "sk"})
	if err != nil {
		t.Fatalf("FetchCreditInfo: %v", err)
	}
	if gotMethod != http.MethodGet || gotPath != CreditPath {
		t.Errorf("请求 = %s %s，期望 GET %s", gotMethod, gotPath, CreditPath)
	}
	if !strings.HasPrefix(gotAuth, "SDK-HMAC-SHA256 ") {
		t.Errorf("请求未签名：Authorization=%q", gotAuth)
	}

	if !info.Available {
		t.Fatal("应判定为有积分套餐")
	}
	if info.Total != 1000 || info.Used != 250 || info.Remaining != 750 {
		t.Errorf("额度 = total %v / used %v / remain %v，期望 1000/250/750",
			info.Total, info.Used, info.Remaining)
	}
	if info.Percent != 25 {
		t.Errorf("百分比 = %v，期望 25", info.Percent)
	}
	if info.Expiring != 50 {
		t.Errorf("到期积分 = %v，期望 50", info.Expiring)
	}
	if info.ResetDate != "2026-11-01" {
		t.Errorf("重置日期 = %q", info.ResetDate)
	}
	if info.Status != "active" || !info.IsCreditPackage || info.IsTokenPackage {
		t.Errorf("套餐信息不对：status=%q credit=%v token=%v",
			info.Status, info.IsCreditPackage, info.IsTokenPackage)
	}
	if info.PackageName != "基础套餐" {
		t.Errorf("套餐名 = %q", info.PackageName)
	}
	if info.Basic == nil || info.Basic.Total != 800 || info.Basic.Used != 200 {
		t.Errorf("基础积分分类不对：%+v", info.Basic)
	}
	if info.OnDemand == nil || info.OnDemand.Expiring != 50 {
		t.Errorf("按需积分分类不对：%+v", info.OnDemand)
	}
	if info.TokenUsed != 1234 || info.TokenLimit != 5678 {
		t.Errorf("token 用量 = %d/%d", info.TokenUsed, info.TokenLimit)
	}
}

// TestParseCreditInfoWithoutPackage 上游没有积分套餐时返回 Available=false 与原因，
// 而不是「0 积分」这种会误导用户的假数据。
func TestParseCreditInfoWithoutPackage(t *testing.T) {
	info, err := parseCreditInfo([]byte(`{"package":{"is_token_package":true},"metrics":[]}`))
	if err != nil {
		t.Fatalf("parseCreditInfo: %v", err)
	}
	if info.Available {
		t.Error("没有积分指标时不应判定为有积分套餐")
	}
	if info.Reason == "" {
		t.Error("应给出不可用原因")
	}
	if !info.IsTokenPackage {
		t.Error("应保留 token 套餐标记")
	}
}

// TestParseCreditInfoRemainFallback 上游只回总额与已用、没回剩余时，自己算。
func TestParseCreditInfoRemainFallback(t *testing.T) {
	info, err := parseCreditInfo([]byte(`{
	  "metrics":[{"name":"usageTotalPackageCredit","package_credit_amount":100,"package_credit_used":40}]
	}`))
	if err != nil {
		t.Fatal(err)
	}
	if info.Remaining != 60 {
		t.Errorf("剩余 = %v，期望 60（100-40）", info.Remaining)
	}
}

// TestParseCreditInfoRemainNeverNegative 上游数据异常（已用超过总额）时剩余不能为负。
func TestParseCreditInfoRemainNeverNegative(t *testing.T) {
	info, err := parseCreditInfo([]byte(`{
	  "metrics":[{"name":"usageTotalPackageCredit","package_credit_amount":100,"package_credit_used":140}]
	}`))
	if err != nil {
		t.Fatal(err)
	}
	if info.Remaining != 0 {
		t.Errorf("剩余 = %v，期望 0（不为负）", info.Remaining)
	}
}

// TestParseCreditInfoStatusMapping 套餐状态映射成控制台用的四个值。
func TestParseCreditInfoStatusMapping(t *testing.T) {
	cases := map[string]string{
		"normal": "active",
		"closed": "expired",
		"freeze": "frozen",
		"":       "unknown",
	}
	for in, want := range cases {
		raw := []byte(`{"package":{"status":"` + in + `"},"metrics":[{"name":"usageTotalPackageCredit"}]}`)
		info, err := parseCreditInfo(raw)
		if err != nil {
			t.Fatal(err)
		}
		if info.Status != want {
			t.Errorf("状态 %q -> %q，期望 %q", in, info.Status, want)
		}
	}
}

// TestFetchCreditInfoUpstreamError 非 200 响应要转成可分类的 UpstreamError。
func TestFetchCreditInfoUpstreamError(t *testing.T) {
	c := NewClient()
	c.HTTP = &http.Client{Transport: creditRoundTripFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusUnauthorized,
			Header:     http.Header{},
			Body:       io.NopCloser(strings.NewReader(`{"error_code":"APIG.0301"}`)),
			Request:    r,
		}, nil
	})}
	_, err := c.FetchCreditInfo(context.Background(), &Session{AccessKey: "ak", SecretKey: "sk"})
	if err == nil {
		t.Fatal("应返回错误")
	}
	ue, ok := err.(*UpstreamError)
	if !ok {
		t.Fatalf("错误类型 = %T，期望 *UpstreamError", err)
	}
	if ue.Status != http.StatusUnauthorized {
		t.Errorf("状态码 = %d，期望 401", ue.Status)
	}
}
