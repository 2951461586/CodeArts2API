// Package codearts 复刻 CodeArts Agent（华为云码道）桌面端的上游模型服务。
//
// 只依赖 Go 标准库：
//   - 凭据：%APPDATA%\codearts-agent 下的 Local State（DPAPI 主密钥）
//   - User\globalStorage\state.vscdb（AES-256-GCM 逐条加密的 secret）
//   - 鉴权：华为云 SDK-HMAC-SHA256（STS 临时 AK/SK + X-Security-token）
//   - 上游：https://snap-access.cn-north-4.myhuaweicloud.com/api/v2/chat/completions
package codearts

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"math"
	"os"
	"strings"
)

// float64FromBits 避免为单一用途引入 math 包的转换函数别名。
func float64FromBits(b uint64) float64 { return math.Float64frombits(b) }

// readFileAll 读取整个文件。
func readFileAll(path string) ([]byte, error) { return os.ReadFile(path) }

// randRead 填充随机字节（薄封装，便于统一错误处理）。
func randRead(b []byte) (int, error) { return rand.Read(b) }

// RandomHex 生成 n 字节的随机十六进制串（用于 ID）。
func RandomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return strings.Repeat("0", n*2)
	}
	return hex.EncodeToString(b)
}

// RandomDigits 生成 n 位随机十进制数字串。
func RandomDigits(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return strings.Repeat("1", n)
	}
	var sb strings.Builder
	sb.WriteByte(byte('1' + b[0]%9)) // 首位非 0
	for i := 1; i < n; i++ {
		sb.WriteByte(byte('0' + b[i]%10))
	}
	return sb.String()
}

// NewUUID 生成标准 UUID v4 字符串。
func NewUUID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return RandomHex(16)
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// shortID 取字符串前 8 位，用于生成可读的名称后缀。
func shortID(s string) string {
	if len(s) <= 8 {
		return s
	}
	return s[:8]
}

// Mask 对敏感串做脱敏，仅保留首尾片段。
func Mask(s string) string {
	if s == "" {
		return ""
	}
	if len(s) <= 10 {
		return s[:1] + "***"
	}
	return s[:6] + "..." + s[len(s)-4:]
}

// jsURLEncode 复刻华为云 JS signer 的 urlEncode：
// 仅保留 RFC3986 unreserved 字符（A-Za-z0-9-._~），其余一律 %XX 大写。
//
// 不能用 url.PathEscape —— Go 会放过 $&+,:;=@ 这些字符，导致签名不一致。
func jsURLEncode(s string) string {
	const upperhex = "0123456789ABCDEF"
	var sb strings.Builder
	sb.Grow(len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9',
			c == '-', c == '.', c == '_', c == '~':
			sb.WriteByte(c)
		default:
			sb.WriteByte('%')
			sb.WriteByte(upperhex[c>>4])
			sb.WriteByte(upperhex[c&0x0f])
		}
	}
	return sb.String()
}
