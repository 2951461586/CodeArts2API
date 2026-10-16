package codearts

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/url"
	"sort"
	"strings"
	"time"
)

// 华为云 SDK-HMAC-SHA256 签名器。
//
// 逐字对齐 @huaweicloud/huaweicloud-sdk-core 的 JS 实现
// （见 extensions/vscode-codebot/out/extension.js 里打包的 signer）。
// 有两个和「标准」文档不同的坑，必须一起复刻，否则必然 APIG.0301：
//
//  1. CanonicalURI 每段单独 urlEncode 后**强制补尾斜杠**：
//     /api/v2/chat/completions → /api/v2/chat/completions/
//  2. CanonicalHeaders 每个 "k:v.trim()" 用 \n 连接后自带尾 \n，
//     拼装 CanonicalRequest 时又补了一个 \n —— 于是 SignedHeaders 前多一个空行。
const (
	sdkSigningAlgorithm = "SDK-HMAC-SHA256"
	sdkDateFormat       = "20060102T150405Z"
)

// signRequest 为一次请求生成完整的签名请求头。
//
// headers 中的键会统一小写；返回的 map 可直接作为 HTTP 请求头使用。
func signRequest(ak, sk, securityToken, method, rawURL string, body []byte, headers map[string]string) (map[string]string, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, err
	}

	out := map[string]string{}
	for k, v := range headers {
		out[strings.ToLower(k)] = v
	}
	out["host"] = u.Host
	if _, ok := out["x-sdk-date"]; !ok {
		out["x-sdk-date"] = time.Now().UTC().Format(sdkDateFormat)
	}
	if securityToken != "" {
		out["x-security-token"] = securityToken
	}

	// 非 PUT/PATCH/POST 一律按空 body 参与签名（与 JS signer 一致）
	payload := body
	switch method {
	case "PUT", "PATCH", "POST":
	default:
		payload = nil
	}

	names := make([]string, 0, len(out))
	for k := range out {
		names = append(names, k)
	}
	sort.Strings(names)

	var ch strings.Builder
	for _, k := range names {
		ch.WriteString(k)
		ch.WriteByte(':')
		ch.WriteString(strings.TrimSpace(out[k]))
		ch.WriteByte('\n')
	}
	signedHeaders := strings.Join(names, ";")

	payloadHash := sha256Hex(payload)
	canonicalRequest := strings.Join([]string{
		method,
		canonicalURI(u),
		canonicalQuery(u),
		ch.String(),
		signedHeaders,
		payloadHash,
	}, "\n")

	stringToSign := strings.Join([]string{
		sdkSigningAlgorithm,
		out["x-sdk-date"],
		sha256Hex([]byte(canonicalRequest)),
	}, "\n")

	mac := hmac.New(sha256.New, []byte(sk))
	mac.Write([]byte(stringToSign))
	signature := hex.EncodeToString(mac.Sum(nil))

	out["Authorization"] = sdkSigningAlgorithm + " Access=" + ak +
		", SignedHeaders=" + signedHeaders + ", Signature=" + signature
	return out, nil
}

// canonicalURI 复刻 JS signer 的 p(O)：逐段 urlEncode 并补尾斜杠。
func canonicalURI(u *url.URL) string {
	path := u.EscapedPath()
	if path == "" {
		path = "/"
	}
	segs := strings.Split(path, "/")
	for i, s := range segs {
		// EscapedPath 已经做过一次转义，先解回原文再按 JS 规则重编码
		if dec, err := url.PathUnescape(s); err == nil {
			segs[i] = jsURLEncode(dec)
		} else {
			segs[i] = jsURLEncode(s)
		}
	}
	out := strings.Join(segs, "/")
	if !strings.HasSuffix(out, "/") {
		out += "/"
	}
	return out
}

// canonicalQuery 复刻 JS signer 的 g(O)：按键排序后 k=v 逐对编码。
func canonicalQuery(u *url.URL) string {
	if u.RawQuery == "" {
		return ""
	}
	q, err := url.ParseQuery(u.RawQuery)
	if err != nil {
		return ""
	}
	keys := make([]string, 0, len(q))
	for k := range q {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var parts []string
	for _, k := range keys {
		vals := q[k]
		sort.Strings(vals)
		for _, v := range vals {
			parts = append(parts, jsURLEncode(k)+"="+jsURLEncode(v))
		}
	}
	return strings.Join(parts, "&")
}

// sha256Hex 返回字节串的 SHA-256 十六进制摘要。
func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
