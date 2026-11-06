package main

import (
	"path/filepath"
	"testing"
)

func TestDoubleClickOptionsUseExecutableUIAndStableData(t *testing.T) {
	exe := filepath.Join(t.TempDir(), "app", "CodexRouter.exe")
	o, err := parseOptions(nil, exe)
	if err != nil {
		t.Fatal(err)
	}
	if !o.gui || !o.open || o.ui != filepath.Join(filepath.Dir(exe), "ui") || !filepath.IsAbs(o.data) || o.port != 18788 {
		t.Fatal("双击默认路径或本机端口错误")
	}
	explicit := t.TempDir()
	o, err = parseOptions([]string{"launch", "--data-dir", explicit, "--port", "19911"}, exe)
	if err != nil || o.data != explicit || o.port != 19911 {
		t.Fatal("专用测试选项失败")
	}
	o, err = parseOptions([]string{"serve", "--api-only", "--data-dir", explicit}, exe)
	if err != nil || o.gui || o.open || o.ui != "" {
		t.Fatal("前台API-only入口回归")
	}
	for _, args := range [][]string{{"serve", "--port", "0"}, {"launch", "--api-only"}, {"serve", "--api-only", "--ui-dir", "ui"}, {"unknown"}} {
		if _, err = parseOptions(args, exe); err == nil {
			t.Fatal("非法选项被接受")
		}
	}
}
