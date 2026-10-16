package codearts

import (
	"encoding/json"
	"testing"
)

// TestIsAutoClaimableSafetyInvariant 是签到功能最重要的安全不变量：
//
// 邀请奖励与学生认证奖励**永远不能**被自动领取 —— 即使服务端把它们标成
// claimable。它们必须由真实事件触发（好友真实注册 / 真实学生认证），
// 自动去领既会被服务端拒绝，也属于活动条款明确禁止的作弊。
func TestIsAutoClaimableSafetyInvariant(t *testing.T) {
	cases := []struct {
		name     string
		campaign WelfareCampaign
		wantAuto bool
	}{
		{
			name: "每日签到（可领）→ 可自动",
			campaign: WelfareCampaign{
				CampaignID: 1, Type: "USER_LOGIN", Title: "每日签到领1000积分",
				Claimable: true,
				Extra: struct {
					StartTime    string `json:"startTime"`
					EndTime      string `json:"endTime"`
					TriggerMode  string `json:"triggerMode"`
					TriggerEvent string `json:"triggerEvent"`
				}{TriggerMode: "MANUAL_CLAIM", TriggerEvent: "user.login"},
			},
			wantAuto: true,
		},
		{
			name: "邀请活动（服务端说可领）→ 仍不可自动",
			campaign: WelfareCampaign{
				CampaignID: 3, Type: "INVITE_USER", Title: "推荐码上有礼",
				Claimable: true,
				Extra: struct {
					StartTime    string `json:"startTime"`
					EndTime      string `json:"endTime"`
					TriggerMode  string `json:"triggerMode"`
					TriggerEvent string `json:"triggerEvent"`
				}{TriggerMode: "MANUAL_CLAIM", TriggerEvent: "invite.success"},
			},
			wantAuto: false,
		},
		{
			name: "学生认证（服务端说可领）→ 仍不可自动",
			campaign: WelfareCampaign{
				CampaignID: 2, Type: "STUDENT_CERTIFIED", Title: "学生认证领4000积分",
				Claimable: true,
				Extra: struct {
					StartTime    string `json:"startTime"`
					EndTime      string `json:"endTime"`
					TriggerMode  string `json:"triggerMode"`
					TriggerEvent string `json:"triggerEvent"`
				}{TriggerMode: "MANUAL_CLAIM", TriggerEvent: "student.certified"},
			},
			wantAuto: false,
		},
		{
			name: "每日签到（未到刷新时间）→ 不可领",
			campaign: WelfareCampaign{
				CampaignID: 1, Type: "USER_LOGIN", Claimable: false,
				Extra: WelfareExtra{TriggerMode: "MANUAL_CLAIM"},
			},
			wantAuto: false,
		},
		{
			name: "自动触发型活动 → 不主动领（交给服务端）",
			campaign: WelfareCampaign{
				CampaignID: 9, Type: "SOME_AUTO", Claimable: true,
				Extra: WelfareExtra{TriggerMode: "AUTO"},
			},
			wantAuto: false,
		},
	}
	for _, c := range cases {
		if got := c.campaign.IsAutoClaimable(); got != c.wantAuto {
			t.Errorf("%s: IsAutoClaimable = %v, want %v", c.name, got, c.wantAuto)
		}
	}
}

// TestWelfareDeliveryClaimableFilters 确认 Claimable() 只挑出安全的目标。
func TestWelfareDeliveryClaimableFilters(t *testing.T) {
	d := &WelfareDelivery{Campaigns: []WelfareCampaign{
		{CampaignID: 1, Type: "USER_LOGIN", Claimable: true},
		{CampaignID: 2, Type: "STUDENT_CERTIFIED", Claimable: true},
		{CampaignID: 3, Type: "INVITE_USER", Claimable: true},
		{CampaignID: 4, Type: "USER_LOGIN", Claimable: false},
	}}
	got := d.Claimable()
	if len(got) != 1 || got[0].CampaignID != 1 {
		t.Errorf("只应挑出 campaignId=1，实际 %+v", got)
	}
}

// TestWelfareDeliveryParse 用真实响应形状验证解析。
func TestWelfareDeliveryParse(t *testing.T) {
	raw := []byte(`{"code":0,"message":"ok","data":{"items":[
      {"campaignId":1,"title":"每日签到领1000积分","type":"USER_LOGIN",
       "benefitAmount":1000,"benefitUnit":"CREDIT","status":"ELIGIBLE","claimable":true,
       "pendingCount":0,"pendingTotalAmount":0.0,
       "extra":{"startTime":"2026-09-08T16:00:00Z","endTime":"2026-12-30T16:00:00Z",
                "triggerMode":"MANUAL_CLAIM","triggerEvent":"user.login"}},
      {"campaignId":3,"title":"推荐码上有礼","type":"INVITE_USER",
       "benefitAmount":1000,"benefitUnit":"CREDIT","status":"CLAIMED","claimable":false,
       "extra":{"triggerMode":"MANUAL_CLAIM","triggerEvent":"invite.success"}}]}}`)

	var env welfareEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("信封解析失败: %v", err)
	}
	if env.Code != welfareOK {
		t.Fatalf("code = %d", env.Code)
	}
	var data struct {
		Items []WelfareCampaign `json:"items"`
	}
	if err := json.Unmarshal(env.Data, &data); err != nil {
		t.Fatalf("items 解析失败: %v", err)
	}
	d := &WelfareDelivery{Campaigns: data.Items}
	if len(d.Campaigns) != 2 {
		t.Fatalf("campaign 数量 = %d", len(d.Campaigns))
	}

	daily, ok := d.Find(1)
	if !ok {
		t.Fatal("找不到 campaignId=1")
	}
	if daily.Title != "每日签到领1000积分" || daily.BenefitAmount != 1000 || daily.BenefitUnit != "CREDIT" {
		t.Errorf("每日签到解析错误: %+v", daily)
	}
	if daily.Extra.TriggerEvent != "user.login" {
		t.Errorf("triggerEvent = %q", daily.Extra.TriggerEvent)
	}
	if !daily.IsAutoClaimable() {
		t.Error("每日签到应可自动领取")
	}

	invite, ok := d.Find(3)
	if !ok {
		t.Fatal("找不到 campaignId=3")
	}
	if invite.IsAutoClaimable() {
		t.Error("邀请活动绝不可自动领取")
	}
	if len(d.Claimable()) != 1 {
		t.Errorf("可自动领取的数量 = %d，应为 1", len(d.Claimable()))
	}
}

// TestDescribeWelfareError 校验错误翻译覆盖了实测到的两种 40001。
func TestDescribeWelfareError(t *testing.T) {
	cases := []struct {
		res  *WelfareClaimResult
		want string
	}{
		{&WelfareClaimResult{OK: true, Title: "每日签到领1000积分"}, "每日签到领1000积分：领取成功"},
		{&WelfareClaimResult{Code: 40001, Title: "每日签到领1000积分",
			Message: "尚未到达权益刷新时间，暂时无法领取"},
			"每日签到领1000积分：今日已领，等下次权益刷新"},
		{&WelfareClaimResult{Code: 40001, Title: "推荐码上有礼",
			Message: "该活动类型需要先通过触发接口激活，无法直接领取"},
			"推荐码上有礼：需要真实事件触发（如好友注册），无法直接领取"},
	}
	for _, c := range cases {
		if got := DescribeWelfareError(c.res); got != c.want {
			t.Errorf("DescribeWelfareError = %q，应为 %q", got, c.want)
		}
	}
	if DescribeWelfareError(nil) != "" {
		t.Error("nil 应返回空串")
	}
}

// TestWelfarePathsMatchIDE 锁定三个 ops 端点路径（与 IDE 一致）。
func TestWelfarePathsMatchIDE(t *testing.T) {
	want := map[string]string{
		WelfareDeliveryPath: "/v1/ops/delivery",
		WelfareClaimPath:    "/v1/ops/claim",
		WelfareInvitePath:   "/v1/ops/invite/generate",
	}
	for got, expect := range want {
		if got != expect {
			t.Errorf("路径 = %q，应为 %q", got, expect)
		}
	}
}
