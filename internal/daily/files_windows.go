//go:build windows

package daily

import (
	"os"
	"syscall"
	"unsafe"
)

func isReparse(info os.FileInfo) bool {
	data, ok := info.Sys().(*syscall.Win32FileAttributeData)
	return !ok || data.FileAttributes&syscall.FILE_ATTRIBUTE_REPARSE_POINT != 0
}

// ReplaceFile保留目标ACL；只替换内容，不用MoveFile覆盖原权限。
func atomicConfigReplace(from, to string) error {
	dst, err := syscall.UTF16PtrFromString(to)
	if err != nil {
		return err
	}
	src, err := syscall.UTF16PtrFromString(from)
	if err != nil {
		return err
	}
	ok, _, err := syscall.NewLazyDLL("kernel32.dll").NewProc("ReplaceFileW").Call(uintptr(unsafe.Pointer(dst)), uintptr(unsafe.Pointer(src)), 0, 0, 0, 0)
	if ok == 0 {
		return err
	}
	return nil
}
