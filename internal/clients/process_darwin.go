//go:build darwin

package clients

import (
	"context"
	"debug/macho"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

func init() {
	detect = detectDarwin
	detectCLI = detectCLIDarwin
	resolveProbeExecutable = nativeCLIDarwin
	startOwned = startOwnedDarwin
	isolateEnvironment = darwinEnvironment
	clientPolicy = func(target, action string) error {
		if action == "stop" {
			return nil
		}
		if target == "desktop" {
			return errors.New("CLIENT_DESKTOP_ISOLATION_UNVERIFIED")
		}
		if action != "start" && action != "check" {
			return errors.New("CLIENT_MACOS_PROBE_ONLY")
		}
		return nil
	}
	describeClient = func(c *Client) {
		if c.Target == "desktop" && c.Installed {
			c.State = "isolation_unverified"
			c.Message = "已发现安装；桌面多实例隔离未验证，禁止启动，不会复用或结束现有实例"
		} else if c.Target == "cli" && c.Installed && c.State != "running" {
			c.State = "probe_only"
			c.Message = "仅支持隔离启动检查（--version）和专用目录登录状态检查；不启动交互代理"
		}
	}
	clientCompleted = func(string, string) string {
		return "隔离启动检查通过（--version）；未启动交互代理，不代表真实上游验收"
	}
	testCommand = func(_ string, install installation, _ string) *exec.Cmd {
		return exec.Command(install.Path, "--version")
	}
}

func darwinEnvironment(parent []string, profile string) []string {
	env := make([]string, 0, len(parent)+6)
	for _, entry := range parent {
		key, _, _ := strings.Cut(entry, "=")
		upper := strings.ToUpper(key)
		// Rebuild a small environment; no inherited cloud, SSH, shell or model credentials.
		switch upper {
		case "PATH", "LANG", "LC_ALL", "LC_CTYPE", "TERM", "COLORTERM", "CODEX_HOME", "CODEX_ELECTRON_USER_DATA_PATH", "CODEX_APP_SERVER_OPENAI_BASE_URL", "OTEL_SDK_DISABLED":
			env = append(env, entry)
		}
	}
	return append(env, "HOME="+filepath.Join(profile, "home"), "CFFIXED_USER_HOME="+filepath.Join(profile, "home"), "XDG_CONFIG_HOME="+filepath.Join(profile, "home", ".config"), "XDG_DATA_HOME="+filepath.Join(profile, "home", ".local", "share"), "XDG_CACHE_HOME="+filepath.Join(profile, "home", ".cache"), "TMPDIR="+filepath.Join(profile, "work"))
}

func nativeCLIDarwin(candidate string) (string, error) {
	p, err := filepath.EvalSymlinks(candidate)
	if err != nil || !filepath.IsAbs(p) {
		return "", errors.New("CLIENT_NATIVE_EXECUTABLE_REQUIRED")
	}
	candidates := []string{p}
	if filepath.Base(p) == "codex.js" && filepath.Base(filepath.Dir(p)) == "bin" {
		root := filepath.Dir(filepath.Dir(p))
		data, err := os.ReadFile(filepath.Join(root, "package.json"))
		var meta struct {
			Name string `json:"name"`
		}
		if err == nil && len(data) < 1<<20 && json.Unmarshal(data, &meta) == nil && meta.Name == "@openai/codex" {
			arch, triple := "arm64", "aarch64-apple-darwin"
			if runtime.GOARCH == "amd64" {
				arch, triple = "x64", "x86_64-apple-darwin"
			}
			vendor := filepath.Join(root, "node_modules", "@openai", "codex-darwin-"+arch, "vendor", triple)
			candidates = append(candidates, filepath.Join(vendor, "bin", "codex"), filepath.Join(vendor, "codex", "codex"), filepath.Join(root, "vendor", triple, "codex", "codex"))
		}
	}
	if filepath.Base(p) == "codex" && filepath.Base(filepath.Dir(p)) == "bin" && filepath.Base(filepath.Dir(filepath.Dir(p))) == "codex-cli" {
		candidates = append(candidates, filepath.Join(filepath.Dir(filepath.Dir(p)), "CodexCLI.app", "Contents", "MacOS", "codex"))
	}
	for _, candidate := range candidates {
		info, err := os.Stat(candidate)
		if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0111 == 0 {
			continue
		}
		if binary, err := macho.Open(candidate); err == nil {
			executable := binary.Type == macho.TypeExec
			_ = binary.Close()
			if executable {
				return filepath.EvalSymlinks(candidate)
			}
		}
		if binary, err := macho.OpenFat(candidate); err == nil {
			executable := false
			for _, arch := range binary.Arches {
				if arch.Type == macho.TypeExec {
					executable = true
				}
			}
			_ = binary.Close()
			if executable {
				return filepath.EvalSymlinks(candidate)
			}
		}
	}
	return "", errors.New("CLIENT_NATIVE_EXECUTABLE_REQUIRED")
}

func detectCLIDarwin() installation {
	candidates := []string{"/opt/homebrew/bin/codex", "/usr/local/bin/codex", "/Applications/Codex.app/Contents/Resources/codex-cli/bin/codex", "/Applications/ChatGPT.app/Contents/Resources/codex-cli/bin/codex"}
	if p, err := exec.LookPath("codex"); err == nil {
		candidates = append(candidates, p)
	}
	for _, candidate := range candidates {
		p, err := nativeCLIDarwin(candidate)
		if err != nil {
			continue
		}
		info, err := os.Stat(p)
		if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0111 == 0 {
			continue
		}
		root, err := os.MkdirTemp("", "codexrouter-discovery-*")
		if err != nil {
			continue
		}
		for _, d := range []string{"home", "work", "codex-home"} {
			_ = os.Mkdir(filepath.Join(root, d), 0700)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		cmd := exec.CommandContext(ctx, p, "--version")
		cmd.Dir = filepath.Join(root, "work")
		cmd.Env = Environment(os.Environ(), root, "")
		cmd.WaitDelay = time.Second
		output, err := cmd.Output()
		cancel()
		_ = os.RemoveAll(root)
		text := strings.TrimSpace(string(output))
		if err == nil && len(text) <= 128 && strings.HasPrefix(text, "codex-cli ") && !strings.ContainsAny(text, "\r\n") {
			return installation{p, text}
		}
	}
	return installation{}
}

func plistValue(plist, key string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	b, err := exec.CommandContext(ctx, "/usr/bin/plutil", "-extract", key, "raw", "-o", "-", plist).Output()
	if err != nil || len(b) > 256 {
		return ""
	}
	return strings.TrimSpace(string(b))
}
func desktopAt(app string) installation {
	plist := filepath.Join(app, "Contents", "Info.plist")
	id := plistValue(plist, "CFBundleIdentifier")
	if id != "com.openai.codex" {
		return installation{}
	}
	binary := plistValue(plist, "CFBundleExecutable")
	if binary == "" || filepath.Base(binary) != binary {
		return installation{}
	}
	p := filepath.Join(app, "Contents", "MacOS", binary)
	if info, err := os.Stat(p); err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0111 == 0 {
		return installation{}
	}
	return installation{p, plistValue(plist, "CFBundleShortVersionString") + " (" + id + ")"}
}
func detectDarwin() map[string]installation {
	out := map[string]installation{"cli": detectCLIDarwin()}
	home, _ := os.UserHomeDir()
	for _, base := range []string{"/Applications", filepath.Join(home, "Applications")} {
		for _, name := range []string{"Codex.app", "ChatGPT.app"} {
			if app := desktopAt(filepath.Join(base, name)); app.Path != "" {
				out["desktop"] = app
				return out
			}
		}
	}
	return out
}

func startOwnedDarwin(cmd *exec.Cmd, _ bool) (func(), error) {
	// Hold the os.Process handle until Wait completes; never signal by name or recycled PID.
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	var once sync.Once
	return func() { once.Do(func() { _ = cmd.Process.Kill() }) }, nil
}
