//go:build windows

package clients

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
	"unsafe"
)

// TestRealVisibleTUIProbe 显式启用才打开真实CLI；仅观察新建空认证环境的登录选择页。
func TestRealVisibleTUIProbe(t *testing.T) {
	root := os.Getenv("CODEXROUTER_REAL_TUI_CHILD")
	if root != "" {
		if err := openConsole(); err != nil {
			os.Exit(10)
		}
		installed := detectCLIWindows()
		if installed.Path == "" {
			os.Exit(11)
		}
		profile := filepath.Join(root, "cli")
		cmd := exec.Command(installed.Path, "--no-daemon", "-C", filepath.Join(profile, "work"))
		cmd.Env = Environment(os.Environ(), profile, "")
		cmd.Stdin = os.Stdin
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		if err := cmd.Start(); err != nil {
			os.Exit(12)
		}
		done := make(chan error, 1)
		go func() { done <- cmd.Wait() }()
		deadline := time.Now().Add(25 * time.Second)
		for time.Now().Before(deadline) {
			var count uint32
			buffer := make([]uint16, 16000)
			ok, _, _ := kernel.NewProc("ReadConsoleOutputCharacterW").Call(os.Stdout.Fd(), uintptr(unsafe.Pointer(&buffer[0])), uintptr(len(buffer)), 0, uintptr(unsafe.Pointer(&count)))
			if ok != 0 {
				text := strings.ToLower(syscall.UTF16ToString(buffer[:count]))
				if strings.Contains(text, "sign in") || strings.Contains(text, "log in") || strings.Contains(text, "welcome to codex") {
					_ = os.WriteFile(filepath.Join(root, "ready"), []byte("real_tui_login_screen_rendered"), 0600)
					<-done
					os.Exit(0)
				}
			}
			select {
			case <-done:
				os.Exit(13)
			default:
			}
			time.Sleep(200 * time.Millisecond)
		}
		os.Exit(14)
	}
	if os.Getenv("CODEXROUTER_REAL_TUI_PROBE") != "1" {
		t.Skip("仅显式启用独立真实CLI窗口探针")
	}
	root = t.TempDir()
	profile := filepath.Join(root, "cli")
	for _, dir := range []string{profile, filepath.Join(profile, "codex-home"), filepath.Join(profile, "work")} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(profile, "codex-home", "config.toml"), []byte("model_provider='openai'\ncli_auth_credentials_store='file'\n[analytics]\nenabled=false\n[feedback]\nenabled=false\n"), 0600); err != nil {
		t.Fatal(err)
	}
	exe, _ := os.Executable()
	cmd := exec.Command(exe, "-test.run=^TestRealVisibleTUIProbe$")
	cmd.Env = append(Environment(os.Environ(), profile, ""), "CODEXROUTER_REAL_TUI_CHILD="+root)
	stop, err := startOwned(cmd, true)
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(filepath.Join(root, "ready")); err == nil {
			stop()
			<-done
			t.Log("真实CLI登录选择画面已渲染；只回收本次Job，不触碰日常实例")
			return
		}
		select {
		case err := <-done:
			t.Fatalf("真实CLI窗口探针提前结束 %v", err)
		default:
		}
		time.Sleep(100 * time.Millisecond)
	}
	stop()
	<-done
	t.Fatal("真实CLI登录界面未在限时内出现")
}
