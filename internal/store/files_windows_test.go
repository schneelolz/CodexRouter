//go:build windows

package store

import (
	"bytes"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func TestAtomicReplaceFailurePreservesOldFile(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.Write("state.json", testDocument{Version: 1}); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "state.json")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	name, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		t.Fatal(err)
	}
	// 不允许 FILE_SHARE_DELETE，模拟实际文件系统拒绝原子替换。
	handle, err := syscall.CreateFile(name, syscall.GENERIC_READ, syscall.FILE_SHARE_READ, nil, syscall.OPEN_EXISTING, syscall.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer syscall.CloseHandle(handle)
	if err := s.Write("state.json", testDocument{Version: 2}); err == nil {
		t.Fatal("系统拒绝替换时写入却返回成功")
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("原子替换失败破坏了原文件")
	}
	matches, err := filepath.Glob(filepath.Join(dir, ".state-*.tmp"))
	if err != nil || len(matches) != 0 {
		t.Fatal("替换失败后遗留临时文件")
	}
}
