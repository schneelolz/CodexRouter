//go:build windows

package store

import (
	"errors"
	"fmt"
	"os"
	"runtime"
	"syscall"
	"unsafe"
)

var moveFileEx = syscall.NewLazyDLL("kernel32.dll").NewProc("MoveFileExW")

func acquireLock(path string) (*os.File, error) {
	name, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	handle, err := syscall.CreateFile(name, syscall.GENERIC_READ|syscall.GENERIC_WRITE, 0, nil, syscall.OPEN_ALWAYS, syscall.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		if errors.Is(err, syscall.Errno(32)) || errors.Is(err, syscall.Errno(33)) {
			return nil, ErrLocked
		}
		return nil, fmt.Errorf("获取实例锁: %w", err)
	}
	return os.NewFile(uintptr(handle), path), nil
}

func atomicReplace(source, target string) error {
	from, err := syscall.UTF16PtrFromString(source)
	if err != nil {
		return err
	}
	to, err := syscall.UTF16PtrFromString(target)
	if err != nil {
		return err
	}
	// 同卷替换与 write-through 避免先删后写造成空窗。
	ok, _, callErr := moveFileEx.Call(uintptr(unsafe.Pointer(from)), uintptr(unsafe.Pointer(to)), 0x1|0x8)
	runtime.KeepAlive(from)
	runtime.KeepAlive(to)
	if ok == 0 {
		return callErr
	}
	return nil
}
