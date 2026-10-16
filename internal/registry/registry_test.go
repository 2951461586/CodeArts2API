package registry

import (
	"testing"

	"codearts2api/internal/codearts"
)

// TestChannelRouting 锁定「模型 → 上游通道」的映射，这是计费安全的关键：
// 免费权益通道的模型必须带 maas_type=benefit，积分通道的必须不带。
func TestChannelRouting(t *testing.T) {
	r := New()
	r.Replace(FromGatewayConfig([]codearts.GatewayModel{
		{ModelID: "glm-5.3-flash", ModelName: "glm-5.3-flash", ContextWindow: 1048576, MaxTokens: 131072},
		{ModelID: "deepseek-v4-pro-0813", ModelName: "deepseek-v4-pro-0813"},
	}))
	r.ReplaceBuiltin(FromBuiltin([]codearts.BuiltinModel{
		{ModelID: "glm-5.3", ModelName: "glm-5.3", Enable: true,
			Credit: []codearts.CreditTier{{RatioDisplay: "0.79x"}}},
		{ModelID: "GLM-5.2", ModelName: "GLM-5.2", Enable: true,
			Credit: []codearts.CreditTier{{RatioDisplay: "0.7x"}}},
		{ModelID: "disabled-model", ModelName: "disabled-model", Enable: false},
	}))

	cases := []struct {
		query   string
		wantID  string
		channel codearts.Channel
		credit  bool
	}{
		{"glm-5.3-flash", "glm-5.3-flash", codearts.ChannelBenefit, false},
		{"glm-5.3", "glm-5.3", codearts.ChannelCredit, true},
		{"GLM-5.3", "glm-5.3", codearts.ChannelCredit, true},
		{"glm-5.3-flash", "glm-5.3-flash", codearts.ChannelBenefit, false},
		{"GLM-5.2", "GLM-5.2", codearts.ChannelCredit, true},
		{"deepseek-v4-pro-0813", "deepseek-v4-pro-0813", codearts.ChannelBenefit, false},
		// 内置别名 → 指向免费通道
		{"gpt-4o", "glm-5.3-flash", codearts.ChannelBenefit, false},
	}
	for _, c := range cases {
		m := r.Resolve(c.query, nil)
		if m.ID != c.wantID {
			t.Errorf("Resolve(%q).ID = %q, want %q", c.query, m.ID, c.wantID)
		}
		if m.Channel != c.channel {
			t.Errorf("Resolve(%q).Channel = %q, want %q", c.query, m.Channel, c.channel)
		}
		if m.IsCredit() != c.credit {
			t.Errorf("Resolve(%q).IsCredit() = %v, want %v", c.query, m.IsCredit(), c.credit)
		}
	}
}

// TestCreditRatioExposed 确认积分倍率会透传到模型清单。
func TestCreditRatioExposed(t *testing.T) {
	r := New()
	r.ReplaceBuiltin(FromBuiltin([]codearts.BuiltinModel{
		{ModelID: "glm-5.3", Enable: true, Credit: []codearts.CreditTier{{RatioDisplay: "0.79x"}}},
	}))
	list := r.List(nil)
	var found bool
	for _, m := range list {
		if m.ID == "glm-5.3" {
			found = true
			if m.Credit != "0.79x" {
				t.Errorf("credit = %q, want 0.79x", m.Credit)
			}
			if !m.IsCredit() {
				t.Error("glm-5.3 应走积分通道")
			}
		}
	}
	if !found {
		t.Fatal("模型清单里没有 glm-5.3")
	}
}

// TestDisabledBuiltinSkipped 确认上游标记为 disabled 的模型不会暴露出去。
func TestDisabledBuiltinSkipped(t *testing.T) {
	r := New()
	r.ReplaceBuiltin(FromBuiltin([]codearts.BuiltinModel{
		{ModelID: "gone", Enable: false},
	}))
	for _, m := range r.List(nil) {
		if m.ID == "gone" {
			t.Error("disabled 的模型不应出现在清单里")
		}
	}
}

// TestMarkStreamOnly 确认流式标记能落到模型上。
func TestMarkStreamOnly(t *testing.T) {
	r := New()
	r.ReplaceBuiltin(FromBuiltin([]codearts.BuiltinModel{{ModelID: "glm-5.3", Enable: true}}))
	if r.Resolve("glm-5.3", nil).StreamOnly {
		t.Fatal("初始不应是 stream-only")
	}
	r.MarkStreamOnly("glm-5.3")
	if !r.Resolve("glm-5.3", nil).StreamOnly {
		t.Error("MarkStreamOnly 后应标记为 stream-only")
	}
}

// TestCustomAlias 确认自定义别名能覆盖到目标模型的通道。
func TestCustomAlias(t *testing.T) {
	r := New()
	r.ReplaceBuiltin(FromBuiltin([]codearts.BuiltinModel{{ModelID: "glm-5.3", Enable: true}}))
	m := r.Resolve("my-cheap", map[string]string{"my-cheap": "glm-5.3-flash"})
	if m.Channel != codearts.ChannelBenefit {
		t.Errorf("自定义别名应继承目标通道，得到 %q", m.Channel)
	}
	if m.ID != "glm-5.3-flash" {
		t.Errorf("自定义别名解析错误: %q", m.ID)
	}
}
