package protocol

import (
	"crypto/rand"
	"encoding/hex"
	"strings"
)

// EstimatePromptLen 粗略估算输入规模（上游不返回 prompt token 数时使用）。
//
// 只是一个日志与 usage 兜底用的量级估计，不参与任何计费或鉴权判断。
func EstimatePromptLen(body []byte) int {
	return len(body) / 4
}

// randomHex 生成 n 字节的随机十六进制串，用于拼 Anthropic 的
// msg_ / toolu_ 前缀 id。随机失败时退回全零串（保证 id 非空即可）。
func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return strings.Repeat("0", n*2)
	}
	return hex.EncodeToString(b)
}
