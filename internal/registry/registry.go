// Package registry 维护对外暴露的模型清单与别名解析。
package registry

import (
	"sort"
	"strings"
	"sync"
	"time"

	"codearts2api/internal/codearts"
)

// Model 是一个对外可用的模型。
type Model struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Context   int    `json:"context"`
	MaxOutput int    `json:"max_output"`
	Desc      string `json:"description"`
	// Source 标记来源：benefit=免费权益通道，builtin=订阅/企业内置
	Source string `json:"source"`
	// Channel 决定上游路由（codearts.ChannelBenefit / ChannelCredit）。
	Channel codearts.Channel `json:"channel"`
	// Credit 是积分消耗倍率，仅 credit 通道有值（如 "0.79x"）。
	Credit string `json:"credit,omitempty"`
	// StreamOnly 标记上游只支持流式：非流式会返回一个空 body 的 400。
	// 网关会自动降级为「流式 + 本地聚合」，这里只是避免每次多打一次空枪。
	StreamOnly bool `json:"stream_only,omitempty"`
}

// IsCredit 报告该模型是否走积分（计费）通道。
func (m Model) IsCredit() bool { return m.Channel == codearts.ChannelCredit }

// Fallback 是上游模型接口不可达时使用的兜底清单。
//
// 取自 2026-10 实测的 benefit 通道（GET /api/v1/gateway/config）。
var Fallback = []Model{
	{ID: "glm-5.3-flash", Name: "GLM-5.3-Flash", Context: 1048576, MaxOutput: 131072,
		Desc: "智谱新一代轻量模型，响应快，适合日常编码与 Agent 循环", Source: "benefit", Channel: codearts.ChannelBenefit},
	{ID: "deepseek-v4-flash-0731", Name: "DeepSeek-V4-Flash", Context: 1048576, MaxOutput: 393216,
		Desc: "DeepSeek 轻量快速版，长上下文", Source: "benefit", Channel: codearts.ChannelBenefit},
	{ID: "deepseek-v4-pro-0813", Name: "DeepSeek-V4-Pro", Context: 1000000, MaxOutput: 393216,
		Desc: "DeepSeek 专业版，复杂推理与代码生成", Source: "benefit", Channel: codearts.ChannelBenefit},
	{ID: "deepseek-v4.1-flash", Name: "DeepSeek-V4.1-Flash", Context: 1000000, MaxOutput: 384000,
		Desc: "DeepSeek V4.1 轻量版", Source: "benefit", Channel: codearts.ChannelBenefit},
}

// aliases 是内置别名，指向 CodeArts 上游模型 ID。
var aliases = map[string]string{
	"gpt-4o":            "glm-5.3-flash",
	"gpt-4o-mini":       "glm-5.3-flash",
	"gpt-4":             "glm-5.3-flash",
	"gpt-4-turbo":       "glm-5.3-flash",
	"gpt-3.5-turbo":     "glm-5.3-flash",
	"gpt-5":             "deepseek-v4-pro-0813",
	"claude-3-5-sonnet": "deepseek-v4-pro-0813",
	"claude-sonnet-4":   "deepseek-v4-pro-0813",
	"claude-3-haiku":    "glm-5.3-flash",
	"deepseek-chat":     "deepseek-v4-flash-0731",
	"deepseek-reasoner": "deepseek-v4-pro-0813",
	"glm-4":             "glm-5.3-flash",
	"codearts":          "glm-5.3-flash",
	"codearts-flash":    "glm-5.3-flash",
	"codearts-pro":      "deepseek-v4-pro-0813",
}

// Registry 是可热更新的模型清单。
type Registry struct {
	mu      sync.RWMutex
	models  []Model
	updated time.Time
}

// New 构造注册表，初始使用兜底清单。
func New() *Registry {
	r := &Registry{}
	r.models = append([]Model(nil), Fallback...)
	return r
}

// Replace 用上游返回的 benefit 模型清单替换内置项（保留 builtin 项）。
func (r *Registry) Replace(benefit []Model) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var builtin []Model
	for _, m := range r.models {
		if m.Source == "builtin" {
			builtin = append(builtin, m)
		}
	}
	out := make([]Model, 0, len(benefit)+len(builtin))
	out = append(out, benefit...)
	out = append(out, builtin...)
	r.models = out
	r.updated = time.Now()
}

// ReplaceBuiltin 更新订阅 / 企业内置模型项。
func (r *Registry) ReplaceBuiltin(builtin []Model) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var benefit []Model
	for _, m := range r.models {
		if m.Source != "builtin" {
			benefit = append(benefit, m)
		}
	}
	out := make([]Model, 0, len(benefit)+len(builtin))
	out = append(out, benefit...)
	out = append(out, builtin...)
	r.models = out
	r.updated = time.Now()
}

// List 返回当前模型清单（含自定义别名）。
func (r *Registry) List(custom map[string]string) []Model {
	r.mu.RLock()
	base := append([]Model(nil), r.models...)
	r.mu.RUnlock()

	seen := map[string]bool{}
	out := make([]Model, 0, len(base)+len(custom))
	for _, m := range base {
		if seen[m.ID] {
			continue
		}
		seen[m.ID] = true
		out = append(out, m)
	}
	keys := make([]string, 0, len(custom))
	for k := range custom {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, alias := range keys {
		target := custom[alias]
		m, ok := r.lookup(base, target)
		if !ok {
			// 允许别名直接指向一个上游未列出的模型名
			m = Model{ID: target, Name: target, Source: "alias"}
		}
		if seen[alias] {
			continue
		}
		seen[alias] = true
		m.ID = alias
		m.Name = alias + " → " + m.Name
		out = append(out, m)
	}
	return out
}

// MarkStreamOnly 记录某模型只支持流式（上游非流式会返回空 body 的 400）。
//
// 实测 glm-5.3 就是这样；网关会自动降级为「流式 + 本地聚合」，
// 这里记下来只是省掉每次多打一次空枪。
func (r *Registry) MarkStreamOnly(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i := range r.models {
		if strings.EqualFold(r.models[i].ID, id) {
			r.models[i].StreamOnly = true
			return
		}
	}
}

// Updated 返回最近一次从上游刷新模型清单的时间。
func (r *Registry) Updated() time.Time {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.updated
}

// Resolve 把用户传入的模型名解析为上游模型 ID。
//
// 依次尝试：精确 ID → 自定义别名 → 内置别名 → 大小写无关匹配 → 包含匹配。
// 无法识别时回落到 glm-5.3-flash（benefit 通道的默认模型）。
func (r *Registry) Resolve(name string, custom map[string]string) Model {
	r.mu.RLock()
	base := append([]Model(nil), r.models...)
	r.mu.RUnlock()

	n := strings.TrimSpace(name)
	if n == "" {
		return r.fallback(base, "glm-5.3-flash")
	}
	if m, ok := r.lookup(base, n); ok {
		return m
	}
	if target, ok := custom[n]; ok {
		if m, ok := r.lookup(base, target); ok {
			return m
		}
		return Model{ID: target, Name: target, Source: "alias", Channel: codearts.ChannelBenefit}
	}
	lower := strings.ToLower(n)
	if target, ok := aliases[lower]; ok {
		return r.fallback(base, target)
	}
	for _, m := range base {
		if strings.EqualFold(m.ID, n) {
			return m
		}
	}
	for _, m := range base {
		if strings.Contains(strings.ToLower(m.ID), lower) {
			return m
		}
	}
	return r.fallback(base, "glm-5.3-flash")
}

func (r *Registry) lookup(list []Model, id string) (Model, bool) {
	for _, m := range list {
		if strings.EqualFold(m.ID, id) {
			return m, true
		}
	}
	return Model{}, false
}

func (r *Registry) fallback(list []Model, id string) Model {
	if m, ok := r.lookup(list, id); ok {
		return m
	}
	for _, m := range Fallback {
		if m.ID == id {
			return m
		}
	}
	return Model{ID: id, Name: id, Source: "benefit", Channel: codearts.ChannelBenefit}
}

// FromGatewayConfig 把上游 benefit 通道清单转换为 registry.Model。
func FromGatewayConfig(models []codearts.GatewayModel) []Model {
	out := make([]Model, 0, len(models))
	for _, m := range models {
		out = append(out, Model{
			ID:        m.ModelID,
			Name:      m.ModelName,
			Context:   m.ContextWindow,
			MaxOutput: m.MaxTokens,
			Desc:      "CodeArts 免费权益通道（计入每日免费额度）",
			Source:    "benefit",
			Channel:   codearts.ChannelBenefit,
		})
	}
	return out
}

// FromBuiltin 把订阅 / 企业内置模型转换为 registry.Model。
//
// 这些模型走的是**积分（计费）通道** —— 请求时不带 maas_type 头。
// 上游返回的 credit[].ratio_display 就是积分消耗倍率，透传给控制台展示。
func FromBuiltin(models []codearts.BuiltinModel) []Model {
	out := make([]Model, 0, len(models))
	for _, m := range models {
		if !m.Enable {
			continue
		}
		desc := m.ModelCategory
		if m.ModelDesc != "" {
			desc = m.ModelDesc
		}
		out = append(out, Model{
			ID:      m.ModelID,
			Name:    m.ModelName,
			Desc:    desc,
			Source:  "builtin",
			Channel: codearts.ChannelCredit,
			Credit:  m.CreditRatio(),
		})
	}
	return out
}
