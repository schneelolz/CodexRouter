// Package diaglog 提供有界异步的本机诊断日志，不接收请求正文或认证信息。
package diaglog

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

// Fields 只承载调用方挑选的结构化诊断字段；禁止传递原始错误、请求或响应。
type Fields map[string]any

// Stats 表示日志写入情况；累计计数不因恢复写盘而清零。
type Stats struct {
	// Dropped 是未能排入队列或写入文件的记录数量。
	Dropped uint64
	// Written 是已写入文件的记录数量。
	Written uint64
	// WriteFailures 是打开、写入及刷新文件失败次数。
	WriteFailures uint64
}

type record struct {
	Format   int    `json:"format_version"`
	Time     string `json:"time"`
	Version  string `json:"program_version"`
	Run      string `json:"run_id"`
	Event    string `json:"event"`
	Fields   Fields `json:"fields,omitempty"`
	Dropped  uint64 `json:"log_dropped,omitempty"`
	Failures uint64 `json:"log_write_failures,omitempty"`
}

// Logger 通过固定容量队列将诊断写入轮转文件。
type Logger struct {
	mu                sync.RWMutex // 防止关闭队列与并发投递冲突。
	closed            bool
	queue             chan []byte
	done              chan struct{}
	dir, version, run string
	limit             int64
	file              *os.File // 仅写盘协程访问。
	size              int64
	dropped           atomic.Uint64
	written           atomic.Uint64
	failures          atomic.Uint64
}

// Open 打开日志目录，失败时仍返回可供服务继续运行的降级日志器。
func Open(dir, version string) (*Logger, error) { return open(dir, version, 10<<20, 1024) }

func open(dir, version string, limit int64, capacity int) (*Logger, error) {
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		return nil, err
	}
	l := &Logger{dir: dir, version: version, run: hex.EncodeToString(id[:]), limit: limit, queue: make(chan []byte, capacity), done: make(chan struct{})}
	err := l.ensureFile()
	if err != nil {
		l.failures.Add(1)
	}
	go l.loop()
	return l, err
}

// Emit 非阻塞投递快照；队列满或记录过大时累计缺失数量。
func (l *Logger) Emit(event string, fields Fields) {
	if l == nil {
		return
	}
	data, err := json.Marshal(record{Format: 1, Time: time.Now().UTC().Format(time.RFC3339Nano), Version: l.version, Run: l.run, Event: event, Fields: fields, Dropped: l.dropped.Load(), Failures: l.failures.Load()})
	if err != nil || len(data) > 16<<10 {
		l.dropped.Add(1)
		return
	}
	data = append(data, '\n')
	l.mu.RLock()
	defer l.mu.RUnlock()
	if l.closed {
		return
	}
	select {
	case l.queue <- data:
	default:
		l.dropped.Add(1)
	}
}

// Snapshot 返回并发安全的日志计数。
func (l *Logger) Snapshot() Stats {
	if l == nil {
		return Stats{}
	}
	return Stats{l.dropped.Load(), l.written.Load(), l.failures.Load()}
}

// Close 停止投递，排空队列并刷新到磁盘；允许并发重复调用。
func (l *Logger) Close() {
	if l == nil {
		return
	}
	l.mu.Lock()
	if !l.closed {
		l.closed = true
		close(l.queue)
	}
	l.mu.Unlock()
	<-l.done
}

func (l *Logger) ensureFile() error {
	if l.file != nil {
		return nil
	}
	if err := os.MkdirAll(l.dir, 0700); err != nil {
		return err
	}
	f, err := os.OpenFile(filepath.Join(l.dir, "router.log"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return err
	}
	// 异常终止可能留下半行，恢复时只移除没有换行符的末尾记录。
	size := info.Size()
	if size > 0 {
		length := min(size, int64(16<<10)+1)
		tail := make([]byte, int(length))
		if _, err = f.ReadAt(tail, size-length); err != nil {
			f.Close()
			return err
		}
		if tail[len(tail)-1] != '\n' {
			size = size - length + int64(bytes.LastIndexByte(tail, '\n')+1)
			if err = f.Truncate(size); err != nil {
				f.Close()
				return err
			}
			l.dropped.Add(1)
		}
	}
	if _, err = f.Seek(0, io.SeekEnd); err != nil {
		f.Close()
		return err
	}
	l.file = f
	l.size = size
	return nil
}

func (l *Logger) resetFile() {
	if l.file != nil {
		_ = l.file.Close()
		l.file = nil
	}
}

func (l *Logger) rotate() error {
	l.sync()
	l.resetFile()
	if err := os.Remove(filepath.Join(l.dir, "router.4.log")); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	for i := 3; i >= 0; i-- {
		from := "router.log"
		if i > 0 {
			from = "router." + strconv.Itoa(i) + ".log"
		}
		to := "router." + strconv.Itoa(i+1) + ".log"
		if err := os.Rename(filepath.Join(l.dir, from), filepath.Join(l.dir, to)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return l.ensureFile()
}

func (l *Logger) sync() {
	if l.file != nil {
		if err := l.file.Sync(); err != nil {
			l.failures.Add(1)
			l.resetFile()
		}
	}
}

func (l *Logger) loop() {
	defer close(l.done)
	defer l.resetFile()
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case data, ok := <-l.queue:
			if !ok {
				l.sync()
				return
			}
			if err := l.ensureFile(); err != nil {
				l.failures.Add(1)
				l.dropped.Add(1)
				continue
			}
			if l.size+int64(len(data)) > l.limit {
				if err := l.rotate(); err != nil {
					l.failures.Add(1)
					l.dropped.Add(1)
					continue
				}
			}
			n, err := l.file.Write(data)
			l.size += int64(n)
			if err != nil || n != len(data) {
				l.failures.Add(1)
				l.dropped.Add(1)
				l.resetFile()
			} else {
				l.written.Add(1)
			}
		case <-ticker.C:
			l.sync()
		}
	}
}
