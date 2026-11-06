//go:build windows

package platform

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"unsafe"
)

var (
	credentialDLL = syscall.NewLazyDLL("advapi32.dll")
	credRead      = credentialDLL.NewProc("CredReadW")
	credWrite     = credentialDLL.NewProc("CredWriteW")
	credDelete    = credentialDLL.NewProc("CredDeleteW")
	credFree      = credentialDLL.NewProc("CredFree")
	shellExecute  = syscall.NewLazyDLL("shell32.dll").NewProc("ShellExecuteW")
)

type windowsCredentials struct {
	// 按规范化数据目录划分凭据命名空间。
	prefix string
}

type credential struct {
	flags          uint32
	typeID         uint32
	targetName     *uint16
	comment        *uint16
	lastWritten    syscall.Filetime
	blobSize       uint32
	blob           *byte
	persist        uint32
	attributeCount uint32
	attributes     uintptr
	targetAlias    *uint16
	userName       *uint16
}

// DefaultDataDir 返回当前用户的 CodexRouter 数据目录。
func DefaultDataDir() (string, error) {
	base, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("获取用户配置目录: %w", err)
	}
	return filepath.Join(base, "CodexRouter"), nil
}

// OpenCredentials 打开由 Windows Credential Manager 保护的独立凭据命名空间。
func OpenCredentials(dataDir string) (Credentials, error) {
	if dataDir == "" {
		return nil, errors.New("凭据数据目录不能为空")
	}
	abs, err := filepath.Abs(dataDir)
	if err != nil {
		return nil, fmt.Errorf("解析凭据数据目录: %w", err)
	}
	// 已存在的目录按实际路径归一化，避免目录别名产生不同凭据。
	if resolved, resolveErr := filepath.EvalSymlinks(abs); resolveErr == nil {
		abs = resolved
	} else if !errors.Is(resolveErr, os.ErrNotExist) {
		return nil, fmt.Errorf("解析凭据目录别名: %w", resolveErr)
	}
	for _, proc := range []*syscall.LazyProc{credRead, credWrite, credDelete, credFree} {
		if err := proc.Find(); err != nil {
			return nil, fmt.Errorf("Windows Credential Manager 不可用: %w", err)
		}
	}
	hash := sha256.Sum256([]byte(strings.ToLower(filepath.Clean(abs))))
	return &windowsCredentials{prefix: "CodexRouter/" + hex.EncodeToString(hash[:]) + "/"}, nil
}

func (c *windowsCredentials) target(name string) (*uint16, error) {
	if err := validateCredentialName(name); err != nil {
		return nil, err
	}
	return syscall.UTF16PtrFromString(c.prefix + name)
}

func (c *windowsCredentials) Get(name string) (string, error) {
	target, err := c.target(name)
	if err != nil {
		return "", err
	}
	var stored *credential
	ok, _, callErr := credRead.Call(uintptr(unsafe.Pointer(target)), 1, 0, uintptr(unsafe.Pointer(&stored)))
	runtime.KeepAlive(target)
	if ok == 0 {
		if errors.Is(callErr, syscall.Errno(1168)) {
			return "", ErrNotFound
		}
		return "", fmt.Errorf("读取系统凭据失败: %w", callErr)
	}
	defer credFree.Call(uintptr(unsafe.Pointer(stored)))
	if stored.blobSize > 2560 || (stored.blobSize != 0 && stored.blob == nil) {
		return "", errors.New("系统凭据内容无效")
	}
	return string(unsafe.Slice(stored.blob, int(stored.blobSize))), nil
}

func (c *windowsCredentials) Set(name, value string) error {
	target, err := c.target(name)
	if err != nil {
		return err
	}
	if len(value) == 0 || len(value) > 2560 {
		return errors.New("凭据长度必须为 1 到 2560 字节")
	}
	blob := []byte(value)
	userName, _ := syscall.UTF16PtrFromString("CodexRouter")
	entry := credential{typeID: 1, targetName: target, blobSize: uint32(len(blob)), blob: &blob[0], persist: 2, userName: userName}
	ok, _, callErr := credWrite.Call(uintptr(unsafe.Pointer(&entry)), 0)
	runtime.KeepAlive(entry)
	runtime.KeepAlive(blob)
	if ok == 0 {
		return fmt.Errorf("保存系统凭据失败: %w", callErr)
	}
	return nil
}

func (c *windowsCredentials) Delete(name string) error {
	target, err := c.target(name)
	if err != nil {
		return err
	}
	ok, _, callErr := credDelete.Call(uintptr(unsafe.Pointer(target)), 1, 0)
	runtime.KeepAlive(target)
	if ok == 0 && !errors.Is(callErr, syscall.Errno(1168)) {
		return fmt.Errorf("删除系统凭据失败: %w", callErr)
	}
	return nil
}

// OpenBrowser 使用系统默认浏览器打开本机管理页面。
func OpenBrowser(raw string) error {
	if err := validateBrowserURL(raw); err != nil {
		return err
	}
	action, _ := syscall.UTF16PtrFromString("open")
	address, err := syscall.UTF16PtrFromString(raw)
	if err != nil {
		return errors.New("浏览器地址包含无效字符")
	}
	result, _, _ := shellExecute.Call(0, uintptr(unsafe.Pointer(action)), uintptr(unsafe.Pointer(address)), 0, 0, 1)
	runtime.KeepAlive(action)
	runtime.KeepAlive(address)
	if result <= 32 {
		return fmt.Errorf("打开系统浏览器失败，系统代码 %d", result)
	}
	return nil
}
