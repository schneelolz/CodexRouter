// Package store 提供进程互斥及有界、原子 JSON 持久化。
package store

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
)

// ErrNotFound 表示状态文件尚不存在。
var ErrNotFound = errors.New("状态文件不存在")

// ErrLocked 表示数据目录已被另一个实例使用。
var ErrLocked = errors.New("数据目录已被另一个实例锁定")

// ErrClosed 表示持久化实例已关闭。
var ErrClosed = errors.New("持久化实例已关闭")

const maxJSONBytes = 16 << 20

var stateName = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]{0,63}\.json$`)

// Store 在存续期间独占一个数据目录，并串行化文件读写。
type Store struct {
	// 目录保存绝对路径，不能由文件名覆盖。
	dir string
	// 锁文件句柄必须在整个实例生命周期内保持打开。
	lock *os.File
	// 互斥锁保护文件读写与关闭的并发。
	mu sync.Mutex
}

// Open 创建或打开数据目录，并获得独占进程锁。
func Open(dir string) (*Store, error) {
	if dir == "" {
		return nil, errors.New("数据目录不能为空")
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, fmt.Errorf("解析数据目录: %w", err)
	}
	if err := os.MkdirAll(abs, 0700); err != nil {
		return nil, fmt.Errorf("创建数据目录: %w", err)
	}
	info, err := os.Lstat(abs)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("数据目录必须是普通目录，不能是符号链接")
	}
	if err := validateDataDirectory(info); err != nil {
		return nil, err
	}
	lock, err := acquireLock(filepath.Join(abs, ".instance.lock"))
	if err != nil {
		return nil, err
	}
	return &Store{dir: abs, lock: lock}, nil
}

func (s *Store) path(name string) (string, error) {
	if !stateName.MatchString(name) {
		return "", errors.New("状态文件名必须是安全的 .json 基础文件名")
	}
	stem := strings.ToUpper(strings.TrimSuffix(name, ".json"))
	if stem == "CON" || stem == "PRN" || stem == "AUX" || stem == "NUL" || (len(stem) == 4 && (strings.HasPrefix(stem, "COM") || strings.HasPrefix(stem, "LPT")) && stem[3] >= '0' && stem[3] <= '9') {
		return "", errors.New("状态文件名不能使用系统保留设备名")
	}
	path := filepath.Join(s.dir, name)
	info, err := os.Lstat(path)
	if err == nil && !info.Mode().IsRegular() {
		return "", errors.New("状态文件必须是普通文件")
	}
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("检查状态文件: %w", err)
	}
	return path, nil
}

// Read 读取单个 JSON 文档；不存在、损坏和超限均返回明确错误。
func (s *Store) Read(name string, out any) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.lock == nil {
		return ErrClosed
	}
	path, err := s.path(name)
	if err != nil {
		return err
	}
	file, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("读取状态文件: %w", err)
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, maxJSONBytes+1))
	if err != nil {
		return fmt.Errorf("读取状态文件: %w", err)
	}
	if len(data) > maxJSONBytes {
		return errors.New("状态文件超过 16 MiB 上限")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(out); err != nil {
		return fmt.Errorf("状态文件损坏或格式不兼容: %w", err)
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return errors.New("状态文件包含多余内容")
	}
	return nil
}

// Write 在同目录临时文件中完成落盘后原子替换，不删除原有有效文件。
func (s *Store) Write(name string, value any) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.lock == nil {
		return ErrClosed
	}
	path, err := s.path(name)
	if err != nil {
		return err
	}
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return fmt.Errorf("编码状态: %w", err)
	}
	if len(data)+1 > maxJSONBytes {
		return errors.New("状态文件超过 16 MiB 上限")
	}
	temp, err := os.CreateTemp(s.dir, ".state-*.tmp")
	if err != nil {
		return fmt.Errorf("创建状态临时文件: %w", err)
	}
	tempName := temp.Name()
	defer os.Remove(tempName)
	if _, err = temp.Write(append(data, '\n')); err == nil {
		err = temp.Sync()
	}
	closeErr := temp.Close()
	if err != nil {
		return fmt.Errorf("写入状态临时文件: %w", err)
	}
	if closeErr != nil {
		return fmt.Errorf("关闭状态临时文件: %w", closeErr)
	}
	if err := atomicReplace(tempName, path); err != nil {
		return fmt.Errorf("原子替换状态文件: %w", err)
	}
	return nil
}

// Close 释放实例锁；重复关闭不会影响其他实例。
func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.lock == nil {
		return nil
	}
	err := s.lock.Close()
	s.lock = nil
	return err
}
