package server

import (
	"errors"
	"net/http"
	"strings"
	"testing"

	"codearts2api/internal/apierr"
	"codearts2api/internal/codearts"
	"codearts2api/internal/registry"
	"codearts2api/internal/store"
)

// TestResolveModelRejectsUnknown 锁定「未知模型名 → 404，不静默回落」。
//
// 回归背景：免费权益通道下线后所有模型都真实扣积分。以前 Resolve 找不到
// 就回落到免费通道的 glm-5.3-flash（既已下线、又是不扣费通道），
// 现在必须报错，否则用户写错模型名会被静默换成另一个模型并扣费。
func TestResolveModelRejectsUnknown(t *testing.T) {
	s := newTestServer(t, 0, 1)
	s.Reg.Replace(registry.FromBuiltin([]codearts.BuiltinModel{
		{ModelID: "glm-5.3", ModelName: "GLM-5.3", Enable: true},
		{ModelID: "GLM-5.2", ModelName: "GLM-5.2", Enable: true},
	}))
	settings := store.DefaultSettings()

	// 已下线的旧模型名：解析失败，且错误信息里带上可用模型。
	_, err := s.resolveModel(settings, "glm-5.3-flash")
	if err == nil {
		t.Fatal("已下线的模型名应解析失败")
	}
	var mnf *apierr.ModelNotFound
	if !errors.As(err, &mnf) {
		t.Fatalf("错误类型应为 *apierr.ModelNotFound，得到 %T", err)
	}
	if !strings.Contains(err.Error(), "glm-5.3-flash") {
		t.Errorf("错误信息应包含请求的模型名: %s", err.Error())
	}
	if !strings.Contains(err.Error(), "GLM-5.2") {
		t.Errorf("错误信息应列出可用模型: %s", err.Error())
	}
	if got := apierr.From(err).Status; got != http.StatusNotFound {
		t.Errorf("对外状态码 = %d，期望 404", got)
	}

	// 已知模型照常解析。
	m, err := s.resolveModel(settings, "glm-5.3")
	if err != nil {
		t.Fatalf("已知模型解析失败: %v", err)
	}
	if m.ID != "glm-5.3" {
		t.Errorf("解析结果 = %q，期望 glm-5.3", m.ID)
	}
}

// TestResolveModelPriority 确认 强制模型 > 请求模型 > 默认模型。
func TestResolveModelPriority(t *testing.T) {
	s := newTestServer(t, 0, 1)
	s.Reg.Replace(registry.FromBuiltin([]codearts.BuiltinModel{
		{ModelID: "glm-5.3", Enable: true},
		{ModelID: "GLM-5.2", Enable: true},
	}))

	settings := store.DefaultSettings()
	settings.DefaultModel = "GLM-5.2"

	if m, err := s.resolveModel(settings, ""); err != nil || m.ID != "GLM-5.2" {
		t.Errorf("空模型名应回落到默认模型，得到 %q err=%v", m.ID, err)
	}
	if m, err := s.resolveModel(settings, "glm-5.3"); err != nil || m.ID != "glm-5.3" {
		t.Errorf("请求里的模型应优先于默认模型，得到 %q err=%v", m.ID, err)
	}
	settings.ForceModel = "GLM-5.2"
	if m, err := s.resolveModel(settings, "glm-5.3"); err != nil || m.ID != "GLM-5.2" {
		t.Errorf("强制模型应优先于请求里的模型，得到 %q err=%v", m.ID, err)
	}
}
