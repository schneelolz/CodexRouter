//go:build windows

package clients

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"
)

func init() {
	detect = detectWindows
	detectCLI = detectCLIWindows
	startOwned = startOwnedWindows
	openConsole = openConsoleWindows
	hideCommand = hideCommandWindows
	replace = replaceWindows
}
func detectCLIWindows() installation {
	candidates := []string{filepath.Join(os.Getenv("LOCALAPPDATA"), "Programs", "OpenAI", "Codex", "bin", "codex.exe")}
	for _, p := range candidates {
		p, err := filepath.Abs(p)
		if err != nil || strings.HasPrefix(p, `\\`) || !strings.EqualFold(filepath.Ext(p), ".exe") {
			continue
		}
		info, err := os.Stat(p)
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		cmd := exec.CommandContext(ctx, p, "--version")
		cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
		cmd.Env = Environment(os.Environ(), filepath.Join(os.TempDir(), "CodexRouter-version-only"), "")
		b, err := cmd.Output()
		cancel()
		if err == nil && strings.HasPrefix(strings.TrimSpace(string(b)), "codex-cli ") {
			return installation{p, strings.TrimSpace(string(b))}
		}
	}
	return installation{}
}
func detectWindows() map[string]installation {
	out := map[string]installation{"cli": detectCLIWindows()}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	exe := filepath.Join(os.Getenv("SystemRoot"), "System32", "WindowsPowerShell", "v1.0", "powershell.exe")
	// 固定只读查询，不接收网页命令、路径或其他脚本输入。
	script := "[Console]::OutputEncoding=[System.Text.UTF8Encoding]::new(); $p=Get-AppxPackage -Name 'OpenAI.Codex' | Select-Object -First 1; if($p){[pscustomobject]@{path=[IO.Path]::Combine($p.InstallLocation,'app','ChatGPT.exe');version=[string]$p.Version}|ConvertTo-Json -Compress}"
	cmd := exec.CommandContext(ctx, exe, "-NoLogo", "-NoProfile", "-NonInteractive", "-Command", script)
	cmd.WaitDelay = time.Second
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	b, err := cmd.Output()
	if err == nil {
		var item struct {
			Path    string `json:"path"`
			Version string `json:"version"`
		}
		if json.Unmarshal(b, &item) == nil && item.Path != "" && filepath.IsAbs(item.Path) && !strings.HasPrefix(item.Path, `\\`) {
			if f, e := os.Stat(item.Path); e == nil && f.Mode().IsRegular() {
				out["desktop"] = installation{item.Path, item.Version}
			}
		}
	}
	return out
}

var kernel = syscall.NewLazyDLL("kernel32.dll")
var createJob = kernel.NewProc("CreateJobObjectW")
var setJob = kernel.NewProc("SetInformationJobObject")
var assignJob = kernel.NewProc("AssignProcessToJobObject")
var allocConsole = kernel.NewProc("AllocConsole")
var getConsoleWindow = kernel.NewProc("GetConsoleWindow")

type jobLimits struct {
	PerProcess    int64
	PerJob        int64
	Flags         uint32
	MinWork       uintptr
	MaxWork       uintptr
	Active        uint32
	Affinity      uintptr
	Priority      uint32
	Scheduling    uint32
	IO            [6]uint64
	ProcessMemory uintptr
	JobMemory     uintptr
	PeakProcess   uintptr
	PeakJob       uintptr
}

func startOwnedWindows(cmd *exec.Cmd, visible bool) (func(), error) {
	handle, _, err := createJob.Call(0, 0)
	if handle == 0 {
		return nil, err
	}
	var once sync.Once
	stop := func() { once.Do(func() { _ = syscall.CloseHandle(syscall.Handle(handle)) }) }
	limits := jobLimits{Flags: 0x2000} // JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE，只约束本次创建的进程树。
	ok, _, err := setJob.Call(handle, 9, uintptr(unsafe.Pointer(&limits)), unsafe.Sizeof(limits))
	if ok == 0 {
		stop()
		return nil, err
	}
	flags := uint32(0x200 | 0x4) // CREATE_NEW_PROCESS_GROUP | CREATE_SUSPENDED
	if visible {
		flags |= 0x10
	} // CREATE_NEW_CONSOLE
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: flags, HideWindow: false}
	if err = cmd.Start(); err != nil {
		stop()
		return nil, err
	}
	ph, err := syscall.OpenProcess(0x0101, false, uint32(cmd.Process.Pid))
	if err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		stop()
		return nil, err
	}
	ok, _, err = assignJob.Call(handle, uintptr(ph))
	_ = syscall.CloseHandle(ph)
	if ok == 0 {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		stop()
		return nil, err
	}
	if err = resumeOwned(cmd.Process.Pid); err != nil {
		stop()
		_ = cmd.Wait()
		return nil, err
	}
	return stop, nil
}

// resumeOwned 在进程加入Job后才恢复其唯一初始线程，避免子进程抢先逃离归属。
func resumeOwned(pid int) error {
	snapshot, err := syscall.CreateToolhelp32Snapshot(0x4, 0)
	if err != nil {
		return err
	}
	defer syscall.CloseHandle(snapshot)
	type threadEntry struct {
		Size, Usage, ID, Owner uint32
		Base, Delta            int32
		Flags                  uint32
	}
	entry := threadEntry{Size: uint32(unsafe.Sizeof(threadEntry{}))}
	first := kernel.NewProc("Thread32First")
	next := kernel.NewProc("Thread32Next")
	ok, _, _ := first.Call(uintptr(snapshot), uintptr(unsafe.Pointer(&entry)))
	for ok != 0 {
		if entry.Owner == uint32(pid) {
			handle, _, err := kernel.NewProc("OpenThread").Call(2, 0, uintptr(entry.ID))
			if handle == 0 {
				return err
			}
			value, _, resumeErr := kernel.NewProc("ResumeThread").Call(handle)
			_ = syscall.CloseHandle(syscall.Handle(handle))
			if uint32(value) == 0xffffffff {
				return resumeErr
			}
			return nil
		}
		ok, _, _ = next.Call(uintptr(snapshot), uintptr(unsafe.Pointer(&entry)))
	}
	return errors.New("无法恢复专用客户端线程")
}
func openConsoleWindows() error {
	hwnd, _, _ := getConsoleWindow.Call()
	if hwnd == 0 {
		ok, _, err := allocConsole.Call()
		if ok == 0 && err != syscall.ERROR_ACCESS_DENIED {
			return err
		}
	}
	in, err := os.OpenFile("CONIN$", os.O_RDWR, 0)
	if err != nil {
		return err
	}
	// 交互TUI需要读取屏幕缓冲区和设置控制台模式，不能只给输出写权限。
	out, err := os.OpenFile("CONOUT$", os.O_RDWR, 0)
	if err != nil {
		in.Close()
		return err
	}
	os.Stdin = in
	os.Stdout = out
	os.Stderr = out
	_ = setStandardHandle(uint32(0xfffffff6), in.Fd())
	_ = setStandardHandle(uint32(0xfffffff5), out.Fd())
	_ = setStandardHandle(uint32(0xfffffff4), out.Fd())
	return nil
}
func hideCommandWindows(cmd *exec.Cmd) { cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true} }
func setStandardHandle(id uint32, handle uintptr) error {
	ok, _, err := kernel.NewProc("SetStdHandle").Call(uintptr(id), handle)
	if ok == 0 {
		return err
	}
	return nil
}
func replaceWindows(from, to string) error {
	a, err := syscall.UTF16PtrFromString(from)
	if err != nil {
		return err
	}
	b, err := syscall.UTF16PtrFromString(to)
	if err != nil {
		return err
	}
	ok, _, err := kernel.NewProc("MoveFileExW").Call(uintptr(unsafe.Pointer(a)), uintptr(unsafe.Pointer(b)), 3)
	if ok == 0 {
		return errors.New("专用配置原子保存失败")
	}
	return nil
}
