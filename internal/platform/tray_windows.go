//go:build windows

package platform

import (
	"context"
	"errors"
	"runtime"
	"syscall"
	"unsafe"
)

var user32 = syscall.NewLazyDLL("user32.dll")
var registerClass = user32.NewProc("RegisterClassExW")
var createWindow = user32.NewProc("CreateWindowExW")
var defWindow = user32.NewProc("DefWindowProcW")
var destroyWindow = user32.NewProc("DestroyWindow")
var postMessage = user32.NewProc("PostMessageW")
var getMessage = user32.NewProc("GetMessageW")
var translateMessage = user32.NewProc("TranslateMessage")
var dispatchMessage = user32.NewProc("DispatchMessageW")
var postQuit = user32.NewProc("PostQuitMessage")
var messageBox = user32.NewProc("MessageBoxW")
var notifyIcon = syscall.NewLazyDLL("shell32.dll").NewProc("Shell_NotifyIconW")

type windowClass struct {
	Size        uint32
	Style       uint32
	Proc        uintptr
	ClassExtra  int32
	WindowExtra int32
	Instance    uintptr
	Icon        uintptr
	Cursor      uintptr
	Background  uintptr
	Menu        *uint16
	Name        *uint16
	SmallIcon   uintptr
}
type point struct{ X, Y int32 }
type windowMessage struct {
	Window         uintptr
	ID             uint32
	WParam, LParam uintptr
	Time           uint32
	Point          point
	Private        uint32
}
type notifyData struct {
	Size        uint32
	Window      uintptr
	ID          uint32
	Flags       uint32
	Callback    uint32
	Icon        uintptr
	Tip         [128]uint16
	State       uint32
	StateMask   uint32
	Info        [256]uint16
	Version     uint32
	Title       [64]uint16
	InfoFlags   uint32
	GUID        [16]byte
	BalloonIcon uintptr
}

// Message 显示不包含机密的中文运行提示或错误。
func init() { messageImpl = windowsMessage; trayImpl = windowsTray }
func windowsMessage(title, text string, isError bool) {
	a, _ := syscall.UTF16PtrFromString(title)
	b, _ := syscall.UTF16PtrFromString(text)
	flags := uintptr(0x40)
	if isError {
		flags = 0x10
	}
	messageBox.Call(0, uintptr(unsafe.Pointer(b)), uintptr(unsafe.Pointer(a)), flags)
}

// Tray 运行本进程托盘，只能打开设置、查看状态和触发退出。
func windowsTray(ctx context.Context, open func() error, quit func()) error {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	name, _ := syscall.UTF16PtrFromString("CodexRouterLocalTray")
	instance, _, _ := syscall.NewLazyDLL("kernel32.dll").NewProc("GetModuleHandleW").Call(0)
	icon, err := brandTrayIcon(iconMetric(49, 16))
	if err != nil {
		return err
	}
	defer user32.NewProc("DestroyIcon").Call(icon)
	var menuIcons [3]uintptr
	for i := range menuIcons {
		menuIcons[i], err = iconBitmap(iconMetric(49, 16), i+1)
		if err != nil {
			for _, bitmap := range menuIcons {
				if bitmap != 0 {
					gdi32.NewProc("DeleteObject").Call(bitmap)
				}
			}
			return err
		}
	}
	defer func() {
		for _, bitmap := range menuIcons {
			gdi32.NewProc("DeleteObject").Call(bitmap)
		}
	}()
	var hwnd uintptr
	var data notifyData
	taskbarName, _ := syscall.UTF16PtrFromString("TaskbarCreated")
	taskbarCreated, _, _ := user32.NewProc("RegisterWindowMessageW").Call(uintptr(unsafe.Pointer(taskbarName)))
	callback := syscall.NewCallback(func(w uintptr, msg uint32, wp, lp uintptr) uintptr {
		if taskbarCreated != 0 && uintptr(msg) == taskbarCreated {
			notifyIcon.Call(0, uintptr(unsafe.Pointer(&data)))
			return 0
		}
		switch msg {
		case 0x8001:
			if lp == 0x203 || lp == 0x202 {
				if err := open(); err != nil {
					go Message("CodexRouter", "打开管理页面失败，请重试。", true)
				}
			}
			if lp == 0x205 {
				menu, _, _ := user32.NewProc("CreatePopupMenu").Call()
				for _, item := range []struct {
					id   uintptr
					text string
				}{{1, "打开管理页面"}, {2, "运行状态"}, {3, "退出 CodexRouter"}} {
					text, _ := syscall.UTF16PtrFromString(item.text)
					user32.NewProc("AppendMenuW").Call(menu, 0, item.id, uintptr(unsafe.Pointer(text)))
					user32.NewProc("SetMenuItemBitmaps").Call(menu, item.id, 0, menuIcons[item.id-1], menuIcons[item.id-1])
				}
				var p point
				user32.NewProc("GetCursorPos").Call(uintptr(unsafe.Pointer(&p)))
				user32.NewProc("SetForegroundWindow").Call(w)
				chosen, _, _ := user32.NewProc("TrackPopupMenu").Call(menu, 0x100|2, uintptr(p.X), uintptr(p.Y), 0, w, 0)
				user32.NewProc("DestroyMenu").Call(menu)
				switch chosen {
				case 1:
					if err := open(); err != nil {
						go Message("CodexRouter", "打开管理页面失败，请重试。", true)
					}
				case 2:
					go Message("CodexRouter 运行成功", "服务正在运行，仅本机可访问。\n管理页面关闭后，服务仍在托盘运行。\n退出服务会停止本程序启动的专用测试客户端。", false)
				case 3:
					go quit()
				}
			}
			return 0
		case 0x10:
			destroyWindow.Call(w)
			return 0
		case 2:
			notifyIcon.Call(2, uintptr(unsafe.Pointer(&data)))
			postQuit.Call(0)
			return 0
		}
		value, _, _ := defWindow.Call(w, uintptr(msg), wp, lp)
		return value
	})
	class := windowClass{Size: uint32(unsafe.Sizeof(windowClass{})), Proc: callback, Instance: instance, Icon: icon, Name: name}
	atom, _, err := registerClass.Call(uintptr(unsafe.Pointer(&class)))
	if atom == 0 {
		return err
	}
	defer user32.NewProc("UnregisterClassW").Call(uintptr(unsafe.Pointer(name)), instance)
	hwnd, _, err = createWindow.Call(0, uintptr(unsafe.Pointer(name)), uintptr(unsafe.Pointer(name)), 0, 0, 0, 0, 0, 0, 0, instance, 0)
	if hwnd == 0 {
		return err
	}
	data = notifyData{Size: uint32(unsafe.Sizeof(notifyData{})), Window: hwnd, ID: 1, Flags: 1 | 2 | 4 | 16, Callback: 0x8001, Icon: icon, InfoFlags: 1}
	copy(data.Tip[:], syscall.StringToUTF16("CodexRouter · 运行成功 · 仅本机"))
	copy(data.Title[:], syscall.StringToUTF16("CodexRouter 运行成功"))
	copy(data.Info[:], syscall.StringToUTF16("管理页面已打开。右键托盘图标可再次打开页面或退出。"))
	ok, _, err := notifyIcon.Call(0, uintptr(unsafe.Pointer(&data)))
	if ok == 0 {
		destroyWindow.Call(hwnd)
		return err
	}
	go func() { <-ctx.Done(); postMessage.Call(hwnd, 0x10, 0, 0) }()
	var msg windowMessage
	for {
		v, _, _ := getMessage.Call(uintptr(unsafe.Pointer(&msg)), 0, 0, 0)
		if int32(v) == -1 {
			return errors.New("托盘消息循环失败")
		}
		if v == 0 {
			break
		}
		translateMessage.Call(uintptr(unsafe.Pointer(&msg)))
		dispatchMessage.Call(uintptr(unsafe.Pointer(&msg)))
	}
	return nil
}
