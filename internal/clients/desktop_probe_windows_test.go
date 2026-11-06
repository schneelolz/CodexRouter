//go:build windows

package clients

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// TestDesktopIsolationProbe 仅在显式指定本轮临时目录时启动空认证桌面。
// 不读日常配置，不登录、不复制认证，仅核对新进程和独立目录的隔离事实。
func TestDesktopIsolationProbe(t *testing.T) {
	root := os.Getenv("CODEXROUTER_DESKTOP_PROBE_ROOT")
	if root == "" {
		t.Skip("需要显式桌面隔离探针目录")
	}
	parent := filepath.Join(os.Getenv("LOCALAPPDATA"), "CodexRouter", "acceptance")
	rel, err := filepath.Rel(parent, root)
	if err != nil || rel == "." || strings.HasPrefix(rel, "..") || !filepath.IsAbs(root) {
		t.Fatal("拒绝非专用临时目录")
	}
	if _, err := os.Stat(root); !os.IsNotExist(err) {
		t.Fatal("探针必须使用全新目录")
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(401)
		_, _ = w.Write([]byte(`{"error":{"code":"PROBE_LOGIN_REQUIRED"}}`))
	}))
	defer server.Close()
	manager, err := New(root, server.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	if manager.installed["desktop"].Path == "" {
		t.Fatal("未找到桌面可执行文件")
	}
	before, err := desktopProcessSnapshot(0, server.URL)
	if err != nil {
		t.Fatal(err)
	}
	_, err = manager.Action(Input{"desktop", "start"})
	if err != nil {
		t.Fatal(err)
	}
	manager.mu.Lock()
	owned := manager.active["desktop"]
	manager.mu.Unlock()
	if owned == nil {
		t.Fatal("桌面未保持独立进程")
	}
	pid := owned.cmd.Process.Pid
	var snapshot map[string]any
	for n := 0; n < 4; n++ {
		time.Sleep(500 * time.Millisecond)
		snapshot, err = desktopProcessSnapshot(pid, server.URL)
		if err != nil {
			t.Fatal(err)
		}
		if snapshot["has_routed_app_server"] == true {
			break
		}
	}
	userData := filepath.Join(manager.profile("desktop"), "user-data")
	localState := false
	for _, file := range []string{"Local State", "Preferences", filepath.Join("Default", "Preferences")} {
		if _, err := os.Stat(filepath.Join(userData, file)); err == nil {
			localState = true
		}
	}
	dailySame := reflect.DeepEqual(before["daily_desktop_processes"], snapshot["daily_desktop_processes"])
	_, stopErr := manager.Action(Input{"desktop", "stop"})
	_, configErr := manager.ensureProfile("desktop", "")
	report := map[string]any{"daily_pid_and_start_times_unchanged": dailySame, "owned_process_stopped": stopErr == nil, "managed_config_unchanged": configErr == nil, "desktop_version": manager.installed["desktop"].Version, "owned_root_pid": pid, "before": before, "after": snapshot, "user_data_created": localState, "profile": manager.profile("desktop"), "real_login_or_generation_attempted": false}
	raw, _ := json.MarshalIndent(report, "", "  ")
	if err = os.WriteFile(filepath.Join(root, "isolation-evidence.json"), raw, 0600); err != nil {
		t.Fatal(err)
	}
	t.Logf("隔离探针PID=%d；userData=%v；独立app-server路由参数=%v", pid, localState, snapshot["has_routed_app_server"])
	if !dailySame || stopErr != nil || configErr != nil || !localState || snapshot["has_routed_app_server"] != true {
		t.Fatal("四隔离证据尚不齐备，不能据此声称桌面兼容")
	}
}
func desktopProcessSnapshot(rootPID int, base string) (map[string]any, error) {
	script := `$ErrorActionPreference='Stop'; $all=@(Get-CimInstance Win32_Process -Filter "Name='ChatGPT.exe' OR Name='codex.exe'"); $rootPIDValue=` + fmtInt(rootPID) + `; $owned=@(); if($rootPIDValue -gt 0){$ids=@($rootPIDValue);for($i=0;$i -lt 6;$i++){$next=@($all|Where-Object{$ids -contains $_.ParentProcessId -and $ids -notcontains $_.ProcessId}|ForEach-Object{$_.ProcessId});if(!$next.Count){break};$ids+=$next};$owned=@($all|Where-Object{$ids -contains $_.ProcessId})}; $daily=@($all|Where-Object{$_.Name -eq 'ChatGPT.exe' -and $null -ne $_.CreationDate -and ($owned.ProcessId -notcontains $_.ProcessId)}|ForEach-Object{[pscustomobject]@{pid=$_.ProcessId;started=$_.CreationDate.ToUniversalTime().ToString('o')}}); $routed=@($owned|Where-Object{$_.Name -eq 'codex.exe' -and $null -ne $_.CommandLine -and $_.CommandLine.Contains('app-server') -and $_.CommandLine.Contains('` + base + `')}); [pscustomobject]@{daily_desktop_processes=$daily;owned_process_count=$owned.Count;has_routed_app_server=($routed.Count -gt 0);owned_root_still_running=(@($owned|Where-Object{$_.ProcessId -eq $rootPIDValue}).Count -eq 1)}|ConvertTo-Json -Depth 4 -Compress`
	exe := filepath.Join(os.Getenv("ProgramFiles"), "PowerShell", "7", "pwsh.exe")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, exe, "-NoLogo", "-NoProfile", "-NonInteractive", "-Command", script)
	cmd.WaitDelay = time.Second
	hideCommand(cmd)
	b, err := cmd.CombinedOutput()
	if err != nil {
		if strings.Contains(string(b), "null-valued") {
			return nil, fmt.Errorf("进程查询遇到退出中的空对象: %w", err)
		}
		return nil, err
	}
	var out map[string]any
	err = json.Unmarshal(b, &out)
	return out, err
}
func fmtInt(value int) string { b, _ := json.Marshal(value); return string(b) }
