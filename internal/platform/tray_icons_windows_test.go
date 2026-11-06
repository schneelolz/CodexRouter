//go:build windows

package platform

import (
	"context"
	"os"
	"syscall"
	"testing"
	"time"
	"unsafe"
)

func TestBrandTrayAndMenuResourcesAtCommonScales(t *testing.T) {
	for _, size := range []int{16, 20, 24, 32} {
		pixels := iconPixels(size, 0)
		if len(pixels) != size*size*4 {
			t.Fatal("像素长度错误")
		}
		opaque, transparent := 0, 0
		for i := 3; i < len(pixels); i += 4 {
			if pixels[i] == 255 {
				opaque++
			}
			if pixels[i] == 0 {
				transparent++
			}
		}
		if opaque == 0 || transparent == 0 {
			t.Fatal("品牌图标缺少实色或透明边界")
		}
		icon, err := brandTrayIcon(size)
		if err != nil {
			t.Fatal(err)
		}
		var info iconInfo
		ok, _, _ := user32.NewProc("GetIconInfo").Call(icon, uintptr(unsafe.Pointer(&info)))
		user32.NewProc("DestroyIcon").Call(icon)
		if ok == 0 || info.IsIcon == 0 || info.Color == 0 {
			t.Fatal("原生托盘资源无效")
		}
		gdi32.NewProc("DeleteObject").Call(info.Mask)
		gdi32.NewProc("DeleteObject").Call(info.Color)
		menu, _, _ := user32.NewProc("CreatePopupMenu").Call()
		for kind := 1; kind <= 3; kind++ {
			bitmap, err := iconBitmap(size, kind)
			if err != nil {
				t.Fatal(err)
			}
			user32.NewProc("AppendMenuW").Call(menu, 0, uintptr(kind), 0)
			ok, _, _ = user32.NewProc("SetMenuItemBitmaps").Call(menu, uintptr(kind), 0, bitmap, bitmap)
			gdi32.NewProc("DeleteObject").Call(bitmap)
			if ok == 0 {
				t.Fatal("菜单位图绑定失败")
			}
		}
		user32.NewProc("DestroyMenu").Call(menu)
	}
}

func TestIsolatedTrayWindowLifecycle(t *testing.T) {
	if os.Getenv("CODEXROUTER_TRAY_PROBE") != "1" {
		t.Skip("仅显式启用隔离托盘窗口验收")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	opened := make(chan struct{}, 2)
	done := make(chan error, 1)
	go func() { done <- windowsTray(ctx, func() error { opened <- struct{}{}; return nil }, cancel) }()
	var ownWindow uintptr
	callback := syscall.NewCallback(func(window, unused uintptr) uintptr {
		var pid uint32
		user32.NewProc("GetWindowThreadProcessId").Call(window, uintptr(unsafe.Pointer(&pid)))
		if pid == uint32(os.Getpid()) {
			var name [128]uint16
			user32.NewProc("GetClassNameW").Call(window, uintptr(unsafe.Pointer(&name[0])), 128)
			if syscall.UTF16ToString(name[:]) == "CodexRouterLocalTray" {
				ownWindow = window
				return 0
			}
		}
		return 1
	})
	deadline := time.Now().Add(5 * time.Second)
	for ownWindow == 0 && time.Now().Before(deadline) {
		user32.NewProc("EnumWindows").Call(callback, 0)
		if ownWindow == 0 {
			time.Sleep(20 * time.Millisecond)
		}
	}
	if ownWindow == 0 {
		cancel()
		select {
		case err := <-done:
			t.Fatalf("托盘未创建: %v", err)
		case <-time.After(time.Second):
			t.Fatal("托盘未创建")
		}
	}
	// 仅给本测试进程的准确窗口投递Explorer恢复事件，不重启Explorer。
	name, _ := syscall.UTF16PtrFromString("TaskbarCreated")
	message, _, _ := user32.NewProc("RegisterWindowMessageW").Call(uintptr(unsafe.Pointer(name)))
	postMessage.Call(ownWindow, message, 0, 0)
	postMessage.Call(ownWindow, 0x8001, 0, 0x202)
	select {
	case <-opened:
	case <-time.After(3 * time.Second):
		t.Fatal("托盘打开动作未调用")
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("隔离托盘未正常退出")
	}
	alive, _, _ := user32.NewProc("IsWindow").Call(ownWindow)
	if alive != 0 {
		t.Fatal("隔离托盘窗口未释放")
	}
}
