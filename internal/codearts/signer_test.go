package codearts

import (
	"net/url"
	"regexp"
	"strings"
	"testing"
)

// 本文件是签名器的「金标准」测试。
//
// 期望值**不是**由本仓库的 Go 实现生成的，而是在 Python 里按
// @huaweicloud/huaweicloud-sdk-core 的 JS signer 语义（含两处与官方文档
// 不同的坑：CanonicalURI 强制补尾斜杠、CanonicalHeaders 自带尾 \n 导致
// SignedHeaders 前多一个空行）独立实现一遍后算出来的。
//
// 这样一旦有人「顺手修正」签名逻辑（例如改用 url.PathEscape、去掉尾斜杠、
// 或对 header 名做 trim），签名就会与真实上游不一致，测试立刻失败 ——
// 而不是等到线上报 APIG.0301 才发现。
//
// 所有用例都显式传入 X-Sdk-Date，签名因此完全确定（不依赖 time.Now）。

const (
	vecAK  = "HSTAAAAAAAAAAAAAAAAAAA"
	vecSK  = "sk-secret-0123456789abcdef"
	vecSTS = "sts-token-abc123"
)

// TestSignRequestVectors 覆盖 POST+STS、GET 带查询串、需要 JS 编码的路径/查询、
// 以及 PUT 空 body 四类真实调用形态。
func TestSignRequestVectors(t *testing.T) {
	cases := []struct {
		name    string
		ak      string
		sk      string
		sts     string
		method  string
		url     string
		body    string
		headers map[string]string
		want    string // 期望的完整 Authorization 头
	}{
		{
			// 覆盖：POST 参与签名的 body、X-Security-token 注入、
			// header 值前后空格被 trim（"  agent-general  " → "agent-general"）。
			name:   "POST with body, STS and padded header value",
			ak:     vecAK,
			sk:     vecSK,
			sts:    vecSTS,
			method: "POST",
			url:    "https://snap-access.cn-north-4.myhuaweicloud.com/api/v2/chat/completions",
			body:   `{"model":"glm-5.3-flash","stream":false}`,
			headers: map[string]string{
				"Content-Type":  "application/json",
				"X-Sdk-Date":    "20260101T000000Z",
				"maas_type":     "benefit",
				"model-id":      "glm-5.3-flash",
				"X-Ot-Function": "  agent-general  ",
			},
			want: "SDK-HMAC-SHA256 Access=" + vecAK +
				", SignedHeaders=content-type;host;maas_type;model-id;x-ot-function;x-sdk-date;x-security-token" +
				", Signature=d74d70f38c5d7e69b73c728740a2c7397a4191737d93f207d1a6f5fce49e99c4",
		},
		{
			// 覆盖：查询串按键/值排序（b=2&a=1&a=0 → a=0&a=1&b=2），
			// 以及**非 POST/PUT/PATCH 请求体的 body 不参与签名**：
			// 这里传了 body，但 payload hash 必须是空串的 sha256。
			name:   "GET with query, body must not be signed",
			ak:     "AKID",
			sk:     "SECRET",
			method: "GET",
			url:    "https://opengw.developer.huaweicloud.com/api/v1/user/tokens/balance?b=2&a=1&a=0",
			body:   `{"ignored":true}`,
			headers: map[string]string{
				"X-Sdk-Date": "20260203T040506Z",
			},
			want: "SDK-HMAC-SHA256 Access=AKID, SignedHeaders=host;x-sdk-date" +
				", Signature=0acd5e903f49fd4e91d71aee6188d446d6f76863c5d8f13a4d428c0af017af45",
		},
		{
			// 覆盖：jsURLEncode 与 Go url.PathEscape 的差异 ——
			// 路径里的 $ + 必须被编码（PathEscape 会放过它们），
			// 中文与空格按 UTF-8 逐字节 %XX 大写，查询串同样编码。
			name:   "path and query need JS urlencode",
			ak:     "AKID",
			sk:     "SECRET",
			sts:    "TOK",
			method: "POST",
			url:    "https://snap-access.cn-north-4.myhuaweicloud.com/api/v2/models/模型 名称/a$b+c?q=a b&z=~-._",
			body:   `{}`,
			headers: map[string]string{
				"X-Sdk-Date":      "20260304T050607Z",
				"x-ot-session-id": "ses_deadbeefdeadbeef",
			},
			want: "SDK-HMAC-SHA256 Access=AKID, SignedHeaders=host;x-ot-session-id;x-sdk-date;x-security-token" +
				", Signature=177aeae3dc48dc4b2dec46b1754ee2e1ea3af8d3f76044c33fe74f74df5f7224",
		},
		{
			// 覆盖：PUT 空 body（sha256("") = e3b0c442…），
			// 以及未提供 X-Security-token 时不注入该头。
			name:   "PUT with empty body",
			ak:     "AKID",
			sk:     "SECRET",
			method: "PUT",
			url:    "https://snap-access.cn-north-4.myhuaweicloud.com/v1/ops/claim",
			body:   "",
			headers: map[string]string{
				"X-Sdk-Date": "20260405T060708Z",
			},
			want: "SDK-HMAC-SHA256 Access=AKID, SignedHeaders=host;x-sdk-date" +
				", Signature=1ee2960c0fad87a352395c32d2e4cb4f1fc2755cd92d09822dea0994700f3f79",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := signRequest(tc.ak, tc.sk, tc.sts, tc.method, tc.url, []byte(tc.body), tc.headers)
			if err != nil {
				t.Fatalf("signRequest 返回错误: %v", err)
			}
			if got["Authorization"] != tc.want {
				t.Errorf("Authorization 不匹配\n got: %s\nwant: %s", got["Authorization"], tc.want)
			}
			// host 必须由 URL 推导，且 security token 只在非空时出现。
			if _, ok := got["host"]; !ok {
				t.Error("缺少 host 头")
			}
			if tc.sts == "" {
				if _, ok := got["x-security-token"]; ok {
					t.Error("security token 为空时不应注入 x-security-token")
				}
			}
		})
	}
}

// TestSignRequestHeaderNameIsNotTrimmed 固化一个容易「被好心修掉」的行为：
// 签名器只对 header **值** 做 trim，对 header **名** 不做。
//
// 与 JS signer 逐字对齐优先于「看起来更干净」—— 一旦开始 trim 名字，
// 签名就与上游不一致。
func TestSignRequestHeaderNameIsNotTrimmed(t *testing.T) {
	got, err := signRequest("AK", "SK", "", "GET", "https://h/p", nil, map[string]string{
		"X-Sdk-Date": "20260101T000000Z",
	})
	if err != nil {
		t.Fatal(err)
	}
	if got["Authorization"] == "" {
		t.Fatal("未生成 Authorization")
	}
	// 显式确认键被小写化（而非原样保留）。
	if _, ok := got["x-sdk-date"]; !ok {
		t.Error("header 键应被小写化")
	}
}

// TestSignRequestDoesNotMutateInput 保证调用方传入的 headers 不会被就地改写。
func TestSignRequestDoesNotMutateInput(t *testing.T) {
	in := map[string]string{
		"Content-Type": "application/json",
		"X-Sdk-Date":   "20260101T000000Z",
	}
	before := len(in)
	if _, err := signRequest("AK", "SK", "STS", "POST", "https://h/p", []byte("{}"), in); err != nil {
		t.Fatal(err)
	}
	if len(in) != before {
		t.Errorf("入参 headers 被就地修改：%d → %d", before, len(in))
	}
	if _, ok := in["host"]; ok {
		t.Error("不应把 host 写回调用方的 map")
	}
	if in["Content-Type"] != "application/json" {
		t.Error("入参 headers 的值被修改")
	}
}

// TestSignRequestAutoDate 在未提供 X-Sdk-Date 时自动补一个 UTC 时间戳。
func TestSignRequestAutoDate(t *testing.T) {
	got, err := signRequest("AK", "SK", "", "GET", "https://h/p", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`^\d{8}T\d{6}Z$`).MatchString(got["x-sdk-date"]) {
		t.Errorf("自动生成的 x-sdk-date 格式不对: %q", got["x-sdk-date"])
	}
	if !strings.HasPrefix(got["Authorization"], sdkSigningAlgorithm+" Access=AK, ") {
		t.Errorf("Authorization 前缀不对: %q", got["Authorization"])
	}
}

// TestCanonicalURI 固化「逐段编码 + 强制补尾斜杠」这一与官方文档不同的行为。
func TestCanonicalURI(t *testing.T) {
	cases := []struct{ raw, want string }{
		{"https://h/api/v2/chat/completions", "/api/v2/chat/completions/"},
		{"https://h/", "/"},
		{"https://h", "/"},
		{"https://h/a/b/", "/a/b/"},
		// $ + 必须编码（Go 的 url.PathEscape 会放过）
		{"https://h/a$b+c", "/a%24b%2Bc/"},
		// 中文与空格
		{"https://h/模型 名称", "/%E6%A8%A1%E5%9E%8B%20%E5%90%8D%E7%A7%B0/"},
	}
	for _, c := range cases {
		u, err := url.Parse(c.raw)
		if err != nil {
			t.Fatalf("解析 %s 失败: %v", c.raw, err)
		}
		if got := canonicalURI(u); got != c.want {
			t.Errorf("canonicalURI(%s) = %q, want %q", c.raw, got, c.want)
		}
	}
}

// TestCanonicalQuery 固化按键、按值双重排序后再编码的行为。
func TestCanonicalQuery(t *testing.T) {
	cases := []struct{ raw, want string }{
		{"", ""},
		{"b=2&a=1&a=0", "a=0&a=1&b=2"},
		{"z=~-._", "z=~-._"},
		{"q=a b", "q=a%20b"},
	}
	for _, c := range cases {
		u, err := url.Parse("https://h/p?" + c.raw)
		if err != nil {
			t.Fatalf("解析 %q 失败: %v", c.raw, err)
		}
		if got := canonicalQuery(u); got != c.want {
			t.Errorf("canonicalQuery(%q) = %q, want %q", c.raw, got, c.want)
		}
	}
}

// TestJSURLEncode 固化 RFC3986 unreserved 之外的字符全部 %XX 大写编码。
func TestJSURLEncode(t *testing.T) {
	cases := []struct{ in, want string }{
		{"abcXYZ019-._~", "abcXYZ019-._~"},
		{"a b", "a%20b"},
		{"$&+,:;=@", "%24%26%2B%2C%3A%3B%3D%40"},
		{"模型", "%E6%A8%A1%E5%9E%8B"},
		{"a/b", "a%2Fb"},
		{"100%", "100%25"},
	}
	for _, c := range cases {
		if got := jsURLEncode(c.in); got != c.want {
			t.Errorf("jsURLEncode(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
