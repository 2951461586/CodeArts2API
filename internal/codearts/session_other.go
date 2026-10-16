//go:build !windows

package codearts

import (
	"errors"
	"os"
	"path/filepath"
)

// 非 Windows 平台没有 DPAPI，无法解密 CodeArts Agent 的 secret storage。
//
// 网关本身可以在 Linux / macOS 上跑（Docker 场景），此时必须通过
// 手工录入 AK/SK/STS（控制台「手工录入」）或环境变量提供凭据。

// ErrUnsupportedPlatform 表示当前平台无法自动读取桌面端凭据。
var ErrUnsupportedPlatform = errors.New("当前平台不支持自动读取 CodeArts Agent 凭据（仅 Windows 支持 DPAPI），请在控制台手工录入凭据")

// ReadSession 在非 Windows 平台不可用。
func ReadSession(string) (*Session, error) { return nil, ErrUnsupportedPlatform }

// writeBackSession 在非 Windows 平台不可用。
func writeBackSession(string, *Session) error { return ErrUnsupportedPlatform }

// DefaultUserDataDirs 返回本机 CodeArts Agent 的 User Data 目录候选。
func DefaultUserDataDirs(explicit string) []string {
	if explicit != "" {
		return []string{explicit}
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	return []string{filepath.Join(home, ".config", "codearts-agent")}
}
