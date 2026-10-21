package codearts

import "testing"

// TestUpstreamSessionIDIsStable 回归：同一凭据必须复用同一个上游会话 ID。
//
// 回归背景：baseHeaders 曾经给每个请求生成**两个不同的随机** session id，
// 等于每次请求都向上游开一个新会话，而上游按账号限制 3 个并发会话 ——
// 结果就是频繁撞上 TM.00001041。
func TestUpstreamSessionIDIsStable(t *testing.T) {
	sess := &Session{DomainID: "dom-1", AccessKey: "AKAAA"}

	first := sess.UpstreamSessionID()
	if first == "" {
		t.Fatal("UpstreamSessionID 不应为空")
	}
	for i := 0; i < 5; i++ {
		if got := sess.UpstreamSessionID(); got != first {
			t.Fatalf("第 %d 次调用结果变了: %q → %q", i+1, first, got)
		}
	}

	// 同一份凭据的副本（Manager 续期时就是这么拷的）必须得到同一个 ID
	cp := *sess
	if got := cp.UpstreamSessionID(); got != first {
		t.Errorf("凭据副本的 session id 变了: %q → %q", first, got)
	}

	// 凭据轮换后应当换新 ID
	rotated := &Session{DomainID: "dom-1", AccessKey: "AKBBB"}
	if got := rotated.UpstreamSessionID(); got == first {
		t.Error("凭据轮换后 session id 应变化")
	}

	// 没有 domain 时退化为按账号名区分
	byName := &Session{AccountLabel: "someone"}
	if got := byName.UpstreamSessionID(); got == "" || got == first {
		t.Errorf("仅有 AccountLabel 时应能得到独立 ID，got %q", got)
	}
}

// TestUpstreamSessionIDEmpty 没有任何可区分信息时返回空串（调用方会回退到随机 ID）。
func TestUpstreamSessionIDEmpty(t *testing.T) {
	var nilSess *Session
	if got := nilSess.UpstreamSessionID(); got != "" {
		t.Errorf("nil Session 应返回空串，got %q", got)
	}
	if got := (&Session{}).UpstreamSessionID(); got != "" {
		t.Errorf("空 Session 应返回空串，got %q", got)
	}
}

// TestBaseHeadersReusesSessionID 两个会话头必须是同一个值，且随凭据稳定。
func TestBaseHeadersReusesSessionID(t *testing.T) {
	c := NewClient()
	sess := &Session{DomainID: "dom-1", AccessKey: "AKAAA"}

	h1 := c.baseHeaders(sess, "glm-5.3")
	sid := h1["x-ot-session-id"]
	if sid == "" {
		t.Fatal("x-ot-session-id 缺失")
	}
	if h1["user-session-id"] != sid {
		t.Errorf("两个会话头不一致: %q vs %q", sid, h1["user-session-id"])
	}
	if _, ok := h1["maas_type"]; ok {
		t.Errorf("免费权益通道已下线，不应再带 maas_type，got %q", h1["maas_type"])
	}
	if h1["model-id"] != "glm-5.3" || h1["model-name"] != "glm-5.3" {
		t.Error("model-id / model-name 应与请求体里的模型一致")
	}

	h2 := c.baseHeaders(sess, "glm-5.3")
	if h2["x-ot-session-id"] != sid {
		t.Errorf("同一凭据的第二次请求换了 session id: %q → %q", sid, h2["x-ot-session-id"])
	}
	// tracing 头仍然每次都要变
	if h1["x-ot-trace-id"] == h2["x-ot-trace-id"] {
		t.Error("trace id 每次请求都应不同")
	}

	// 没有凭据信息时回退为随机 ID，但两个头仍要一致
	h4 := c.baseHeaders(&Session{}, "glm-5.3")
	if h4["x-ot-session-id"] == "" || h4["x-ot-session-id"] != h4["user-session-id"] {
		t.Errorf("回退路径的会话头不一致: %q / %q", h4["x-ot-session-id"], h4["user-session-id"])
	}
}
