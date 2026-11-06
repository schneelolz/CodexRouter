//go:build windows

package platform

import (
	"errors"
	"path/filepath"
	"testing"
)

func TestCredentialManagerSyntheticLifecycle(t *testing.T) {
	dir := t.TempDir()
	backend, err := OpenCredentials(dir)
	if err != nil {
		t.Fatal(err)
	}
	const name = "test-synthetic"
	// 合成内容仅存入本测试的独立命名空间，退出前删除。
	t.Cleanup(func() {
		if err := backend.Delete(name); err != nil {
			t.Error(err)
		}
	})
	if _, err := backend.Get(name); !errors.Is(err, ErrNotFound) {
		t.Fatalf("不存在的凭据应明确报错: %v", err)
	}
	if err := backend.Set(name, "synthetic-company-secret-中文"); err != nil {
		t.Fatal(err)
	}
	value, err := backend.Get(name)
	if err != nil || value != "synthetic-company-secret-中文" {
		t.Fatal("凭据读写不一致")
	}
	same, err := OpenCredentials(filepath.Join(dir, "."))
	if err != nil {
		t.Fatal(err)
	}
	if value, err := same.Get(name); err != nil || value == "" {
		t.Fatal("规范化后的同目录无法读取原凭据")
	}
	other, err := OpenCredentials(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := other.Get(name); !errors.Is(err, ErrNotFound) {
		t.Fatal("不同数据目录发生凭据串用")
	}
	if err := backend.Set(name, "synthetic-rotated"); err != nil {
		t.Fatal(err)
	}
	if value, err := backend.Get(name); err != nil || value != "synthetic-rotated" {
		t.Fatal("凭据轮换失败")
	}
	if err := backend.Delete(name); err != nil {
		t.Fatal(err)
	}
	if _, err := backend.Get(name); !errors.Is(err, ErrNotFound) {
		t.Fatal("删除凭据后仍可读取")
	}
	if err := backend.Delete(name); err != nil {
		t.Fatal("重复删除应保持幂等")
	}
	if err := backend.Set("../invalid", "synthetic"); err == nil {
		t.Fatal("不安全凭据名称未被拒绝")
	}
	if err := backend.Set(name, ""); err == nil {
		t.Fatal("空凭据未被拒绝")
	}
}
