package protocol

import (
	"encoding/json"
	"reflect"
	"testing"
)

// 本文件是 Anthropic ↔ OpenAI 协议转换的**金标准**测试。
//
// 转换层是整个 Anthropic 兼容面最容易悄悄出错的地方：字段名、内容块顺序、
// stop_reason 取值、tool_use / tool_result 的配对方式，任何一处偏差都不会让
// 网关报错，但会让客户端拿到语义错误的结果。
//
// 这些用例先把**当前行为**钉住，之后重构（例如把纯转换抽到 internal/protocol）
// 只要行为不变，测试就应当全部通过。

// jsonEq 比较「结构等价」：忽略 map 键顺序与空白差异。
func jsonEq(t *testing.T, got any, wantJSON string) {
	t.Helper()
	gb, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("序列化结果失败: %v", err)
	}
	var g, w any
	if err := json.Unmarshal(gb, &g); err != nil {
		t.Fatalf("反序列化结果失败: %v (%s)", err, gb)
	}
	if err := json.Unmarshal([]byte(wantJSON), &w); err != nil {
		t.Fatalf("期望值不是合法 JSON: %v", err)
	}
	if !reflect.DeepEqual(g, w) {
		t.Errorf("结构不一致\n got: %s\nwant: %s", gb, wantJSON)
	}
}

// TestAnthropicToOpenAIBasic 覆盖 system / max_tokens / temperature / stop
// 以及最朴素的文本消息。
func TestAnthropicToOpenAIBasic(t *testing.T) {
	temp := 0.5
	req := &Request{
		Model:         "claude-x",
		MaxTokens:     256,
		System:        json.RawMessage(`"you are helpful"`),
		Temperature:   &temp,
		StopSequences: []string{"END"},
		Messages: []Message{
			{Role: "user", Content: json.RawMessage(`"hi"`)},
		},
	}
	out, err := ToOpenAI(req, "glm-5.3-flash")
	if err != nil {
		t.Fatal(err)
	}
	jsonEq(t, out, `{
		"model": "glm-5.3-flash",
		"stream": false,
		"max_tokens": 256,
		"temperature": 0.5,
		"stop": ["END"],
		"messages": [
			{"role": "system", "content": "you are helpful"},
			{"role": "user", "content": "hi"}
		]
	}`)
}

// TestAnthropicToOpenAIToolRoundTrip 覆盖 tool_use → tool_calls 与
// tool_result → role=tool 的展开（一条 Anthropic 消息会变成多条 OpenAI 消息）。
func TestAnthropicToOpenAIToolRoundTrip(t *testing.T) {
	req := &Request{
		Messages: []Message{
			{Role: "assistant", Content: json.RawMessage(`[
				{"type":"text","text":"let me check"},
				{"type":"tool_use","id":"toolu_1","name":"get_weather","input":{"city":"北京"}}
			]`)},
			{Role: "user", Content: json.RawMessage(`[
				{"type":"tool_result","tool_use_id":"toolu_1","content":[{"type":"text","text":"晴 25C"}]}
			]`)},
		},
	}
	out, err := ToOpenAI(req, "m")
	if err != nil {
		t.Fatal(err)
	}
	jsonEq(t, out["messages"], `[
		{
			"role": "assistant",
			"content": "let me check",
			"tool_calls": [{
				"id": "toolu_1",
				"type": "function",
				"function": {"name":"get_weather","arguments":"{\"city\":\"北京\"}"}
			}]
		},
		{"role": "tool", "tool_call_id": "toolu_1", "content": "晴 25C"}
	]`)
}

// TestAnthropicToOpenAIImageAndToolChoice 覆盖图片（base64 / url）与
// tool_choice 的三种形态。
func TestAnthropicToOpenAIImageAndToolChoice(t *testing.T) {
	req := &Request{
		Messages: []Message{
			{Role: "user", Content: json.RawMessage(`[
				{"type":"image","source":{"type":"base64","media_type":"image/jpeg","data":"AAA"}},
				{"type":"image","source":{"type":"url","url":"https://x/y.png"}},
				{"type":"text","text":"what"}
			]`)},
		},
		Tools: []Tool{{
			Name: "t", Description: "d",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"a":{"type":"string"}}}`),
		}},
		ToolChoice: json.RawMessage(`{"type":"any"}`),
	}
	out, err := ToOpenAI(req, "m")
	if err != nil {
		t.Fatal(err)
	}
	jsonEq(t, out["messages"], `[
		{"role":"user","content":[
			{"type":"image_url","image_url":{"url":"data:image/jpeg;base64,AAA"}},
			{"type":"image_url","image_url":{"url":"https://x/y.png"}},
			{"type":"text","text":"what"}
		]}
	]`)
	jsonEq(t, out["tools"], `[
		{"type":"function","function":{
			"name":"t","description":"d",
			"parameters":{"type":"object","properties":{"a":{"type":"string"}}}
		}}
	]`)
	if out["tool_choice"] != "required" {
		t.Errorf("tool_choice(type=any) 应映射为 required，got %v", out["tool_choice"])
	}
}

// TestAnthropicToolChoiceMapping 固化四种 tool_choice 的映射。
func TestAnthropicToolChoiceMapping(t *testing.T) {
	cases := []struct {
		raw  string
		want any
	}{
		{`{"type":"any"}`, "required"},
		{`{"type":"none"}`, "none"},
		{`{"type":"auto"}`, "auto"},
		{`{"type":"tool","name":"f"}`, map[string]any{
			"type": "function", "function": map[string]any{"name": "f"},
		}},
		{`not-json`, "auto"},
	}
	for _, c := range cases {
		if got := ToolChoice(json.RawMessage(c.raw)); !reflect.DeepEqual(got, c.want) {
			t.Errorf("ToolChoice(%s) = %#v，期望 %#v", c.raw, got, c.want)
		}
	}
}

// TestAnthropicSystemText 覆盖 system 的两种写法（字符串 / 文本块数组）。
func TestAnthropicSystemText(t *testing.T) {
	cases := []struct{ raw, want string }{
		{``, ""},
		{`"  hello  "`, "hello"},
		{`[{"type":"text","text":"a"},{"type":"text","text":"b"}]`, "a\nb"},
		{`[{"type":"image"},{"type":"text","text":"only"}]`, "only"},
		{`not-json`, ""},
	}
	for _, c := range cases {
		if got := SystemText(json.RawMessage(c.raw)); got != c.want {
			t.Errorf("SystemText(%s) = %q，期望 %q", c.raw, got, c.want)
		}
	}
}

// TestAnthropicToolResultText 覆盖工具结果的三种形态。
func TestAnthropicToolResultText(t *testing.T) {
	cases := []struct {
		block map[string]any
		want  string
	}{
		{map[string]any{"content": "plain"}, "plain"},
		{map[string]any{"content": []any{
			map[string]any{"type": "text", "text": "a"},
			map[string]any{"type": "text", "text": "b"},
		}}, "a\nb"},
		{map[string]any{"content": map[string]any{"k": "v"}}, `{"k":"v"}`},
		{map[string]any{}, ""},
	}
	for _, c := range cases {
		if got := ToolResultText(c.block); got != c.want {
			t.Errorf("ToolResultText(%v) = %q，期望 %q", c.block, got, c.want)
		}
	}
}

// TestOpenAIToAnthropicThinkingTextTools 覆盖内容块顺序
// （thinking → text → tool_use）与 stop_reason= tool_use。
func TestOpenAIToAnthropicThinkingTextTools(t *testing.T) {
	raw := []byte(`{
		"choices":[{
			"message":{
				"content":"hi",
				"reasoning_content":"think",
				"tool_calls":[{"id":"call_1","function":{"name":"f","arguments":"{\"x\":1}"}}]
			},
			"finish_reason":"stop"
		}],
		"usage":{"prompt_tokens":11,"completion_tokens":7}
	}`)
	reply := FromOpenAI(raw, "m")
	if reply.Text != "hi" || reply.Thinking != "think" {
		t.Errorf("text/thinking 统计错误: %q / %q", reply.Text, reply.Thinking)
	}
	// id 是随机生成的，固定后再整体比对。
	reply.Payload["id"] = "msg_fixed"
	jsonEq(t, reply.Payload, `{
		"id":"msg_fixed","type":"message","role":"assistant","model":"m",
		"content":[
			{"type":"thinking","thinking":"think"},
			{"type":"text","text":"hi"},
			{"type":"tool_use","id":"call_1","name":"f","input":{"x":1}}
		],
		"stop_reason":"tool_use","stop_sequence":null,
		"usage":{"input_tokens":11,"output_tokens":7}
	}`)
}

// TestOpenAIToAnthropicStopReasons 覆盖 finish_reason → stop_reason。
func TestOpenAIToAnthropicStopReasons(t *testing.T) {
	cases := []struct{ finish, want string }{
		{"stop", "end_turn"},
		{"length", "max_tokens"},
		{"tool_calls", "end_turn"}, // 没有 tool_calls 数组时仍按 end_turn
	}
	for _, c := range cases {
		raw := []byte(`{"choices":[{"message":{"content":"x"},"finish_reason":"` + c.finish + `"}]}`)
		got := FromOpenAI(raw, "m").Payload["stop_reason"]
		if got != c.want {
			t.Errorf("finish_reason=%s → stop_reason=%v，期望 %s", c.finish, got, c.want)
		}
	}
}

// TestOpenAIToAnthropicEmptyContent 空响应也要给出一个合法的空 text 块，
// 否则 Anthropic 客户端会因为 content 为空数组而报错。
func TestOpenAIToAnthropicEmptyContent(t *testing.T) {
	reply := FromOpenAI([]byte(`{"choices":[{"message":{"content":""}}]}`), "m")
	jsonEq(t, reply.Payload["content"], `[{"type":"text","text":""}]`)
	if reply.Payload["stop_reason"] != "end_turn" {
		t.Errorf("stop_reason = %v，期望 end_turn", reply.Payload["stop_reason"])
	}
}
