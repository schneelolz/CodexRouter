//go:build windows

package clients

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
	"unsafe"
)

func TestVisibleConsoleHandles(t *testing.T) {
	if root := os.Getenv("CODEXROUTER_CONSOLE_TEST_ROOT"); root != "" {
		if err := openConsole(); err != nil {
			_ = os.WriteFile(filepath.Join(root, "failure"), []byte("console_open_failed"), 0600)
			os.Exit(7)
		}
		mode := uint32(0)
		getMode := kernel.NewProc("GetConsoleMode")
		ok, _, _ := getMode.Call(os.Stdin.Fd(), uintptr(unsafe.Pointer(&mode)))
		if ok == 0 {
			os.Exit(8)
		}
		var screen [64]byte
		ok, _, _ = kernel.NewProc("GetConsoleScreenBufferInfo").Call(os.Stdout.Fd(), uintptr(unsafe.Pointer(&screen[0])))
		if ok == 0 {
			os.Exit(9)
		}
		_ = os.WriteFile(filepath.Join(root, "ready"), []byte("console_input_ready"), 0600)
		time.Sleep(30 * time.Second)
		os.Exit(0)
	}
	exe, _ := os.Executable()
	root := t.TempDir()
	cmd := exec.Command(exe, "-test.run=^TestVisibleConsoleHandles$")
	cmd.Env = append(os.Environ(), "CODEXROUTER_CONSOLE_TEST_ROOT="+root)
	stop, err := startOwned(cmd, true)
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(filepath.Join(root, "ready")); err == nil {
			stop()
			<-done
			return
		}
		select {
		case err := <-done:
			t.Fatalf("控制台提前退出: %v", err)
		default:
		}
		time.Sleep(50 * time.Millisecond)
	}
	stop()
	<-done
	t.Fatal("控制台未就绪")
}

func TestOwnedProcessJobStartsAndStops(t *testing.T) {
	if os.Getenv("CODEXROUTER_OWNED_TEST_CHILD") == "1" {
		time.Sleep(30 * time.Second)
		return
	}
	exe, _ := os.Executable()
	cmd := exec.Command(exe, "-test.run=^TestOwnedProcessJobStartsAndStops$")
	cmd.Env = append(os.Environ(), "CODEXROUTER_OWNED_TEST_CHILD=1")
	stop, err := startOwned(cmd, false)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		t.Fatalf("受控子进程提前退出:%v", err)
	case <-time.After(time.Second):
	}
	stop()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Job关闭后进程未退出")
	}
}
