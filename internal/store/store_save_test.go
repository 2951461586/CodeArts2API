package store

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// TestConcurrentSaveIsSafe 锁住「并发 Save 不能损坏 / 丢失状态文件」。
//
// 回归背景：Save 曾经把所有 goroutine 的写入都塞进同一个 `<path>.tmp`。
// Windows 上并发写会直接报「文件被占用」而失败（调用方忽略错误 → 状态静默
// 不落盘），Linux 上则可能把写了一半的临时文件 rename 成正式状态文件 ——
// 含 refresh_token 的账号会全部丢失。
func TestConcurrentSaveIsSafe(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	// 把状态撑大，让单次写入需要多次系统调用，放大交错窗口
	for i := 0; i < 4000; i++ {
		st.AddKey("key-name-padding-0123456789")
	}

	var wg sync.WaitGroup
	errs := make(chan error, 64)
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 25; j++ {
				if err := st.Save(); err != nil {
					errs <- err
					return
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("并发 Save 返回错误: %v", err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读回状态文件失败: %v", err)
	}
	var probe State
	if err := json.Unmarshal(raw, &probe); err != nil {
		t.Fatalf("状态文件被并发 Save 损坏（%d 字节）: %v", len(raw), err)
	}
	if len(probe.Keys) != 4000 {
		t.Errorf("状态文件内容不完整: keys=%d，期望 4000", len(probe.Keys))
	}
}

// TestSaveLeavesNoTempFiles 确认临时文件不会堆积（成功路径上会被 rename 掉）。
func TestSaveLeavesNoTempFiles(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if err := st.Save(); err != nil {
			t.Fatal(err)
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() != "state.json" {
			t.Errorf("残留文件: %s", e.Name())
		}
	}
}
