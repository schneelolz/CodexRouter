package store

import (
	"bytes"
	"errors"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
)

func privateTempDir(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "private")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	return dir
}

type testDocument struct {
	Version int      `json:"version"`
	Values  []string `json:"values"`
}

func TestRoundTripAndExclusiveLock(t *testing.T) {
	dir := privateTempDir(t)
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := Open(dir); !errors.Is(err, ErrLocked) {
		t.Fatalf("同目录第二实例未被阻止: %v", err)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestLockSubprocess$")
	cmd.Env = append(os.Environ(), "CODEXROUTER_TEST_LOCK_DIR="+dir)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("跨进程锁验证失败: %v %s", err, output)
	}
	if err := s.Read("state.json", new(testDocument)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("不存在的文件错误不明确: %v", err)
	}
	for i := 1; i <= 3; i++ {
		want := testDocument{Version: i, Values: []string{"中文", "stream"}}
		if err := s.Write("state.json", want); err != nil {
			t.Fatal(err)
		}
		var got testDocument
		if err := s.Read("state.json", &got); err != nil || got.Version != i || len(got.Values) != 2 || got.Values[0] != "中文" {
			t.Fatalf("原子替换后的状态错误: %+v %v", got, err)
		}
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if err := s.Write("state.json", testDocument{}); !errors.Is(err, ErrClosed) {
		t.Fatalf("关闭后仍可写入: %v", err)
	}
	next, err := Open(dir)
	if err != nil {
		t.Fatalf("锁释放后无法重开: %v", err)
	}
	defer next.Close()
	var restored testDocument
	if err := next.Read("state.json", &restored); err != nil || restored.Version != 3 {
		t.Fatalf("重启后状态错误: %+v %v", restored, err)
	}
}

func TestLockSubprocess(t *testing.T) {
	dir := os.Getenv("CODEXROUTER_TEST_LOCK_DIR")
	if dir == "" {
		return
	}
	s, err := Open(dir)
	if s != nil {
		s.Close()
	}
	if !errors.Is(err, ErrLocked) {
		t.Fatalf("跨进程独占失败: %v", err)
	}
}

func TestCorruptionNeverReplaced(t *testing.T) {
	dir := privateTempDir(t)
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for _, content := range []string{`{`, `{"version":1} {"version":2}`, `{"version":1,"unknown":true}`, `null garbage`} {
		path := filepath.Join(dir, "broken.json")
		if err := os.WriteFile(path, []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
		if err := s.Read("broken.json", new(testDocument)); err == nil {
			t.Fatalf("损坏状态未被拒绝: %q", content)
		}
		got, err := os.ReadFile(path)
		if err != nil || string(got) != content {
			t.Fatal("读取损坏状态时修改了原文件")
		}
	}
}

func TestRejectedWritesPreserveLastState(t *testing.T) {
	dir := privateTempDir(t)
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.Write("state.json", testDocument{Version: 7}); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Write("state.json", math.NaN()); err == nil {
		t.Fatal("不可编码的状态应写入失败")
	}
	if err := s.Write("state.json", bytes.Repeat([]byte{'x'}, maxJSONBytes)); err == nil {
		t.Fatal("超限状态应写入失败")
	}
	after, err := os.ReadFile(filepath.Join(dir, "state.json"))
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("写入失败破坏了原状态")
	}
	if err := os.Mkdir(filepath.Join(dir, "blocked.json"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := s.Write("blocked.json", testDocument{}); err == nil {
		t.Fatal("目录目标应写入失败")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if filepath.Ext(entry.Name()) == ".tmp" {
			t.Fatal("写入后遗留状态临时文件")
		}
	}
}

func TestPathAndSizeBounds(t *testing.T) {
	dir := privateTempDir(t)
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for _, name := range []string{"../outside.json", `..\outside.json`, "C:\\outside.json", "/outside.json", "state.json:stream", "state", ".json", "CON.json/child", "CON.json", "nul.json", "COM1.json", "LPT9.json"} {
		if err := s.Write(name, testDocument{}); err == nil {
			t.Errorf("允许危险文件名 %q", name)
		}
		if err := s.Read(name, new(testDocument)); err == nil {
			t.Errorf("允许危险读取 %q", name)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "large.json"), bytes.Repeat([]byte{' '}, maxJSONBytes+1), 0600); err != nil {
		t.Fatal(err)
	}
	if err := s.Read("large.json", new(testDocument)); err == nil {
		t.Fatal("超限读取未失败")
	}
}

func TestConcurrentReadWriteAndClose(t *testing.T) {
	s, err := Open(privateTempDir(t))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 10 {
				if err := s.Write("state.json", testDocument{Version: i}); err != nil && !errors.Is(err, ErrClosed) {
					t.Error(err)
				}
				var got testDocument
				if err := s.Read("state.json", &got); err != nil && !errors.Is(err, ErrClosed) {
					t.Error(err)
				}
			}
		}()
	}
	wg.Wait()
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
}
