//go:build darwin && cgo

package platform

/*
#cgo CFLAGS: -Wno-deprecated-declarations
#cgo LDFLAGS: -framework Security -framework Foundation -framework AppKit
#include <stdlib.h>
#include "native_darwin.h"
*/
import "C"

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"sync"
	"syscall"
	"unsafe"
)

type darwinCredentials struct {
	service  string
	keychain unsafe.Pointer
	mu       sync.Mutex
}

func keychainError(operation string, status int) error {
	if status == 0 {
		return nil
	}
	if status == -25300 {
		return ErrNotFound
	}
	return fmt.Errorf("Keychain %s: %w", operation, &CredentialError{Status: status})
}

func OpenCredentials(dataDir string) (Credentials, error) {
	service, err := credentialService(dataDir)
	if err != nil {
		return nil, err
	}
	return openProfileCredentials(service)
}

func newDarwinCredentials(service string) (Credentials, error) {
	c := &darwinCredentials{service: service}
	if err := keychainError("打开", int(C.cr_keychain_open(&c.keychain))); err != nil {
		return nil, err
	}
	runtime.SetFinalizer(c, func(c *darwinCredentials) { C.cr_release(c.keychain) })
	return c, nil
}

func (c *darwinCredentials) names(name string) (*C.char, *C.char, error) {
	if err := validateCredentialName(name); err != nil {
		return nil, nil, err
	}
	return C.CString(c.service), C.CString(name), nil
}
func (c *darwinCredentials) Get(name string) (string, error) {
	service, account, err := c.names(name)
	if err != nil {
		return "", err
	}
	defer C.free(unsafe.Pointer(service))
	defer C.free(unsafe.Pointer(account))
	c.mu.Lock()
	defer c.mu.Unlock()
	var data unsafe.Pointer
	var size C.size_t
	status := C.cr_keychain_get(c.keychain, service, account, &data, &size)
	runtime.KeepAlive(c)
	if err := keychainError("读取", int(status)); err != nil {
		return "", err
	}
	defer C.cr_free(data)
	return string(C.GoBytes(data, C.int(size))), nil
}
func (c *darwinCredentials) checkAccess(name string, authorize bool) error {
	service, account, err := c.names(name)
	if err != nil {
		return err
	}
	defer C.free(unsafe.Pointer(service))
	defer C.free(unsafe.Pointer(account))
	c.mu.Lock()
	defer c.mu.Unlock()
	mode := C.int(0)
	if authorize {
		mode = 1
	}
	status := C.cr_keychain_check(c.keychain, service, account, mode)
	runtime.KeepAlive(c)
	return keychainError("检查访问", int(status))
}

func (c *darwinCredentials) Check(name string) error     { return c.checkAccess(name, false) }
func (c *darwinCredentials) Authorize(name string) error { return c.checkAccess(name, true) }

func (c *darwinCredentials) Set(name, value string) error {
	if len(value) == 0 || len(value) > 2560 {
		return errors.New("凭据长度必须为 1 到 2560 字节")
	}
	service, account, err := c.names(name)
	if err != nil {
		return err
	}
	defer C.free(unsafe.Pointer(service))
	defer C.free(unsafe.Pointer(account))
	data := C.CBytes([]byte(value))
	defer C.free(data)
	c.mu.Lock()
	defer c.mu.Unlock()
	status := C.cr_keychain_set(c.keychain, service, account, data, C.size_t(len(value)))
	runtime.KeepAlive(c)
	return keychainError("保存", int(status))
}
func (c *darwinCredentials) Delete(name string) error {
	service, account, err := c.names(name)
	if err != nil {
		return err
	}
	defer C.free(unsafe.Pointer(service))
	defer C.free(unsafe.Pointer(account))
	c.mu.Lock()
	defer c.mu.Unlock()
	status := C.cr_keychain_delete(c.keychain, service, account)
	runtime.KeepAlive(c)
	if status == -25300 {
		return nil
	}
	return keychainError("删除", int(status))
}

// privateKeychain creates only an explicitly named, disposable test keychain.
func privateKeychain(path, service string) (*darwinCredentials, func(), error) {
	p := C.CString(path)
	defer C.free(unsafe.Pointer(p))
	c := &darwinCredentials{service: service}
	if err := keychainError("创建测试钥匙串", int(C.cr_keychain_fixture(p, &c.keychain))); err != nil {
		return nil, nil, err
	}
	return c, func() { C.cr_keychain_remove_fixture(c.keychain); C.cr_release(c.keychain) }, nil
}

func (c *darwinCredentials) lockFixture() error {
	return keychainError("锁定测试钥匙串", int(C.cr_keychain_lock_fixture(c.keychain)))
}

var trayCallbacks struct {
	sync.Mutex
	open func() error
	quit func()
}

func init() {
	openProfileCredentials = newDarwinCredentials
	trayImpl = trayDarwin
	canonicalCredentialPath = func(path string) (string, error) {
		p := C.CString(path)
		defer C.free(unsafe.Pointer(p))
		buffer := make([]byte, 1024) // MAXPATHLEN on Darwin; F_GETPATH returns the on-disk spelling.
		status := C.cr_canonical_path(p, (*C.char)(unsafe.Pointer(&buffer[0])))
		if status != 0 {
			return "", syscall.Errno(status)
		}
		return C.GoString((*C.char)(unsafe.Pointer(&buffer[0]))), nil
	}
}
func trayDarwin(ctx context.Context, open func() error, quit func()) error {
	trayCallbacks.Lock()
	if trayCallbacks.quit != nil {
		trayCallbacks.Unlock()
		return errors.New("菜单栏已运行")
	}
	trayCallbacks.open, trayCallbacks.quit = open, quit
	trayCallbacks.Unlock()
	defer func() {
		trayCallbacks.Lock()
		trayCallbacks.open, trayCallbacks.quit = nil, nil
		trayCallbacks.Unlock()
	}()
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			C.cr_tray_stop()
		case <-done:
		}
	}()
	if C.cr_tray_run() != 0 {
		return errors.New("macOS 菜单栏必须在主线程运行")
	}
	return nil
}

//export cr_tray_open
func cr_tray_open() {
	trayCallbacks.Lock()
	open := trayCallbacks.open
	trayCallbacks.Unlock()
	if open != nil {
		go func() {
			if open() != nil {
				Message("CodexRouter", "打开管理页面失败", true)
			}
		}()
	}
}

//export cr_tray_quit
func cr_tray_quit() {
	trayCallbacks.Lock()
	quit := trayCallbacks.quit
	trayCallbacks.Unlock()
	if quit != nil {
		quit()
	}
	C.cr_tray_stop()
}
