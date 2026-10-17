//go:build windows

package codearts

import (
	"bytes"
	"encoding/json"
	"testing"
)

// 本文件只包含依赖 Windows DPAPI 的测试。
//
// 背景：`readSecretValue` / `writeBackSession` 的真实实现与 `ErrWriteBackSkipped`
// 都只在 session_windows.go（`//go:build windows`）里定义，非 Windows 平台没有
// 等价实现。把这些测试留在通用的 codearts_test.go 里，Linux/macOS 上
// **整个测试包都编译不过** —— `go test` 与 `go vet` 会直接失败
// （CI 的 linux job 就是这样挂掉的）。
//
// 因此凡是直接调用 Windows 专属符号的测试，都必须放进 `*_windows_test.go`。

// TestReadItemValueFindsSecret 校验能真正读出 secret 行（含溢出页）。
func TestReadItemValueFindsSecret(t *testing.T) {
	dir := localUserData(t)
	if dir == "" {
		t.Skip("本机没有 CodeArts Agent User Data")
	}
	plain, err := readSecretValue(dir, SecretItemKey)
	if err != nil {
		t.Fatalf("读取 secret 失败: %v", err)
	}
	var probe map[string]any
	if err := json.Unmarshal(plain, &probe); err != nil {
		t.Fatalf("secret 不是合法 JSON: %v", err)
	}
	if probe["accessKey"] == nil {
		t.Errorf("secret 里没有 accessKey 字段")
	}
}

// TestWriteBackSessionRoundTrip 在临时 User Data 目录上跑完整的回写流程：
// 复制 Local State + state.vscdb → 回写 → 重新解密 → 内容必须一致。
func TestWriteBackSessionRoundTrip(t *testing.T) {
	dir := localUserData(t)
	if dir == "" {
		t.Skip("本机没有 CodeArts Agent User Data")
	}
	tmp := t.TempDir()
	if err := copyTree(dir, tmp); err != nil {
		t.Fatalf("复制 User Data 失败: %v", err)
	}

	sess, err := ReadSession(tmp)
	if err != nil {
		t.Fatalf("ReadSession(tmp): %v", err)
	}
	before, _ := sess.RawJSON()

	if err := writeBackSession(tmp, sess); err != nil {
		if err == ErrWriteBackSkipped {
			t.Skip("本次会话长度变化，回写按设计跳过")
		}
		t.Fatalf("writeBackSession: %v", err)
	}

	after, err := ReadSession(tmp)
	if err != nil {
		t.Fatalf("回写后读取失败: %v", err)
	}
	got, _ := after.RawJSON()

	// 回写会在明文尾部补空格（JSON 尾随空白合法），比较解析后的字段而非原始字节
	var a, b map[string]any
	if err := json.Unmarshal(before, &a); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(bytes.TrimRight(got, " "), &b); err != nil {
		t.Fatalf("回写后的 JSON 不合法: %v", err)
	}
	if a["accessKey"] != b["accessKey"] || a["securitytoken"] != b["securitytoken"] {
		t.Errorf("回写后凭据不一致: %v vs %v", a["accessKey"], b["accessKey"])
	}
}
