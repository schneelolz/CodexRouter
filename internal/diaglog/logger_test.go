package diaglog

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestRotationRestartAndDrain(t *testing.T) {
	dir := t.TempDir()
	for range 2 {
		l, err := open(dir, "synthetic", 1500, 1024)
		if err != nil {
			t.Fatal(err)
		}
		for i := range 50 {
			l.Emit("sample", Fields{"sequence": i})
		}
		l.Close()
		l.Close()
		if st := l.Snapshot(); st.Written != 50 || st.Dropped != 0 || st.WriteFailures != 0 {
			t.Fatal(st)
		}
	}
	files, err := filepath.Glob(filepath.Join(dir, "*.log"))
	if err != nil || len(files) != 5 {
		t.Fatal(files, err)
	}
	for _, path := range files {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if len(data) > 1500 {
			t.Fatal("轮转超限", len(data))
		}
		for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
			var r record
			if json.Unmarshal([]byte(line), &r) != nil || r.Run == "" || r.Time == "" || r.Format != 1 || r.Version != "synthetic" {
				t.Fatal("日志字段或JSON无效")
			}
		}
	}
}

func TestQueueOverflowAndConcurrentClose(t *testing.T) {
	// 不启动消费者，使满队列场景可确定复现。
	l := &Logger{queue: make(chan []byte, 1), done: make(chan struct{})}
	l.Emit("one", nil)
	l.Emit("two", nil)
	if l.Snapshot().Dropped != 1 {
		t.Fatal("队列满未统计")
	}
	if len(l.queue) != 1 {
		t.Fatal("投递阻塞或队列无界")
	}
	close(l.done)
	var wg sync.WaitGroup
	for range 10 {
		wg.Add(1)
		go func() { defer wg.Done(); l.Emit("concurrent", nil); l.Close() }()
	}
	wg.Wait()
}

func TestUnavailableDirectoryRecoversWithoutStopping(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "logs")
	if err := os.WriteFile(dir, []byte("blocked"), 0600); err != nil {
		t.Fatal(err)
	}
	l, err := open(dir, "test", 10<<20, 1024)
	if err == nil || l == nil {
		t.Fatal("应返回降级日志器")
	}
	if l.Snapshot().WriteFailures != 1 {
		t.Fatal(l.Snapshot())
	}
	if err := os.Remove(dir); err != nil {
		t.Fatal(err)
	}
	l.Emit("recovered", nil)
	l.Close()
	if st := l.Snapshot(); st.Written != 1 || st.WriteFailures != 1 {
		t.Fatal(st)
	}
	data, err := os.ReadFile(filepath.Join(dir, "router.log"))
	if err != nil || !strings.Contains(string(data), "recovered") {
		t.Fatal(err)
	}
}

func TestWriteFailureCountAndDiscard(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "blocked")
	if err := os.WriteFile(dir, nil, 0600); err != nil {
		t.Fatal(err)
	}
	l, _ := open(dir, "test", 10<<20, 4)
	l.Emit("event", nil)
	l.Close()
	if st := l.Snapshot(); st.WriteFailures < 2 || st.Dropped != 1 || st.Written != 0 {
		t.Fatal(st)
	}
}

func TestRestartRemovesOnlyIncompleteLastRecord(t *testing.T) {
	dir := t.TempDir()
	l, err := Open(dir, "before")
	if err != nil {
		t.Fatal(err)
	}
	l.Emit("retained", nil)
	l.Close()
	path := filepath.Join(dir, "router.log")
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.WriteString(`{"incomplete":`)
	_ = f.Close()
	l, err = Open(dir, "after")
	if err != nil {
		t.Fatal(err)
	}
	l.Emit("restarted", nil)
	l.Close()
	if l.Snapshot().Dropped != 1 {
		t.Fatal("丢弃半行未计数")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 2 {
		t.Fatal("完整历史被修改或半行未清理")
	}
	for _, line := range lines {
		var row record
		if err := json.Unmarshal([]byte(line), &row); err != nil {
			t.Fatal(err)
		}
	}
}
