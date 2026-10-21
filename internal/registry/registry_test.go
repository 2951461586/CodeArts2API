package registry

import (
	"testing"

	"codearts2api/internal/codearts"
)

// TestResolveOnlyKnownModels 锁定「未知模型名必须解析失败」这条底线。
//
// 免费权益通道下线后每个模型都真实扣积分，静默回落到某个默认模型
// 等于替用户乱花钱，因此这里逐条确认解析路径不会「猜」。
func TestResolveOnlyKnownModels(t *testing.T) {
	r := New()
	r.Replace(FromBuiltin([]codearts.BuiltinModel{
		{ModelID: "glm-5.3", ModelName: "GLM-5.3", Enable: true,
			Credit: []codearts.CreditTier{{RatioDisplay: "0.79x"}}},
		{ModelID: "GLM-5.2", ModelName: "GLM-5.2", Enable: true,
			Credit: []codearts.CreditTier{{RatioDisplay: "0.7x"}}},
		{ModelID: "deepseek-v4.1-flash", ModelName: "DeepSeek-V4.1-Flash", Enable: true,
			Credit: []codearts.CreditTier{{RatioDisplay: "0.18x"}}},
		{ModelID: "disabled-model", ModelName: "disabled-model", Enable: false},
	}))

	cases := []struct {
		query  string
		wantID string
		wantOK bool
	}{
		{"glm-5.3", "glm-5.3", true},
		{"GLM-5.3", "glm-5.3", true}, // 大小写无关
		{"glm-5.3-flash", "", false}, // 已下线的旧模型名
		{"deepseek-v4-pro-0813", "", false},
		{"gpt-4o", "glm-5.3", true}, // 内置别名 → 现存模型
		{"gpt-5", "GLM-5.2", true},
		{"deepseek-chat", "deepseek-v4.1-flash", true},
		{"disabled-model", "", false}, // 上游标记 disabled 的不暴露
		{"glm", "", false},            // 不做模糊匹配
		{"", "glm-5.3", true},         // 未指定模型 → 默认模型
	}
	for _, c := range cases {
		m, ok := r.Resolve(c.query, nil)
		if ok != c.wantOK {
			t.Errorf("Resolve(%q) ok = %v, want %v", c.query, ok, c.wantOK)
			continue
		}
		if ok && m.ID != c.wantID {
			t.Errorf("Resolve(%q).ID = %q, want %q", c.query, m.ID, c.wantID)
		}
	}
}

// TestCreditRatioExposed 确认积分倍率会透传到模型清单。
func TestCreditRatioExposed(t *testing.T) {
	r := New()
	r.Replace(FromBuiltin([]codearts.BuiltinModel{
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
		}
	}
	if !found {
		t.Fatal("模型清单里没有 glm-5.3")
	}
}

// TestDisabledBuiltinSkipped 确认上游标记为 disabled 的模型不会暴露出去。
func TestDisabledBuiltinSkipped(t *testing.T) {
	r := New()
	r.Replace(FromBuiltin([]codearts.BuiltinModel{
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
	r.Replace(FromBuiltin([]codearts.BuiltinModel{{ModelID: "glm-5.3", Enable: true}}))
	if m, ok := r.Resolve("glm-5.3", nil); !ok || m.StreamOnly {
		t.Fatal("初始不应是 stream-only")
	}
	r.MarkStreamOnly("glm-5.3")
	if m, ok := r.Resolve("glm-5.3", nil); !ok || !m.StreamOnly {
		t.Error("MarkStreamOnly 后应标记为 stream-only")
	}
}

// TestCustomAlias 确认自定义别名解析到目标模型。
func TestCustomAlias(t *testing.T) {
	r := New()
	r.Replace(FromBuiltin([]codearts.BuiltinModel{{ModelID: "glm-5.3", Enable: true}}))

	m, ok := r.Resolve("my-cheap", map[string]string{"my-cheap": "glm-5.3"})
	if !ok || m.ID != "glm-5.3" {
		t.Fatalf("自定义别名解析错误: %+v ok=%v", m, ok)
	}
	if m.Source != "alias" {
		t.Errorf("别名的 Source 应为 alias，得到 %q", m.Source)
	}

	// 别名指向一个不存在的模型：解析失败，而不是把死模型名发给上游。
	if _, ok := r.Resolve("my-dead", map[string]string{"my-dead": "glm-5.3-flash"}); ok {
		t.Error("别名指向已下线的模型时应解析失败")
	}
	// 也不应该在 /v1/models 里宣传它。
	for _, x := range r.List(map[string]string{"my-dead": "glm-5.3-flash"}) {
		if x.ID == "my-dead" {
			t.Error("目标不存在的别名不应出现在模型清单里")
		}
	}
}

// TestFallbackModelsAreCurrent 确认兜底清单里没有已下线的免费通道模型。
func TestFallbackModelsAreCurrent(t *testing.T) {
	if len(Fallback) == 0 {
		t.Fatal("兜底清单不能为空")
	}
	dead := map[string]bool{
		"glm-5.3-flash":          true,
		"deepseek-v4-flash-0731": true,
		"deepseek-v4-pro-0813":   true,
	}
	for _, m := range Fallback {
		if dead[m.ID] {
			t.Errorf("兜底清单仍包含已下线的模型 %q", m.ID)
		}
		if m.Credit == "" {
			t.Logf("提示：%q 没有积分倍率（上游未公布计费档）", m.ID)
		}
	}
}
