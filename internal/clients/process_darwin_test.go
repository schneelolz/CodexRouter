//go:build darwin

package clients

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestDarwinEnvironmentDoesNotCarryUserIdentity(t *testing.T) {
	env := Environment([]string{"HOME=/real-user", "CFFIXED_USER_HOME=/real-user", "AWS_SECRET_ACCESS_KEY=synthetic-forbidden", "GITHUB_TOKEN=synthetic-forbidden", "SSH_AUTH_SOCK=/real-agent", "DYLD_INSERT_LIBRARIES=synthetic-forbidden", "ZDOTDIR=/real-shell", "PATH=/usr/bin:/bin", "CODEX_HOME=/daily", "OPENAI_API_KEY=synthetic-forbidden", "HTTPS_PROXY=synthetic-forbidden", "PI_CONFIG=synthetic-forbidden"}, "/isolated", "http://127.0.0.1:19999")
	text := strings.Join(env, "\n")
	for _, forbidden := range []string{"real-user", "synthetic-forbidden", "real-agent", "real-shell", "/daily"} {
		if strings.Contains(text, forbidden) {
			t.Fatal("environment leaked", forbidden)
		}
	}
	for _, required := range []string{"HOME=/isolated/home", "CODEX_HOME=/isolated/codex-home", "TMPDIR=/isolated/work"} {
		if !strings.Contains(text, required) {
			t.Fatal("missing", required)
		}
	}
}
func TestDarwinDesktopDiscoveryAndStartupGate(t *testing.T) {
	app := filepath.Join(t.TempDir(), "Renamed.app")
	if err := os.MkdirAll(filepath.Join(app, "Contents", "MacOS"), 0700); err != nil {
		t.Fatal(err)
	}
	plist := `<?xml version="1.0"?><plist version="1.0"><dict><key>CFBundleIdentifier</key><string>com.openai.codex</string><key>CFBundleExecutable</key><string>Synthetic</string><key>CFBundleShortVersionString</key><string>0.synthetic</string></dict></plist>`
	if err := os.WriteFile(filepath.Join(app, "Contents", "Info.plist"), []byte(plist), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(app, "Contents", "MacOS", "Synthetic"), []byte("synthetic fixture; never executed"), 0700); err != nil {
		t.Fatal(err)
	}
	if got := desktopAt(app); got.Path == "" || !strings.Contains(got.Version, "com.openai.codex") {
		t.Fatal("bundle metadata not discovered")
	}
	m, err := New(filepath.Join(t.TempDir(), "profiles"), "http://127.0.0.1:19999")
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	for _, action := range []string{"start", "login", "direct", "routed", "check"} {
		if _, err := m.Action(Input{"desktop", action}); err == nil || err.Error() != "CLIENT_DESKTOP_ISOLATION_UNVERIFIED" {
			t.Fatal("desktop gate failed", action, err)
		}
	}
	if _, err := os.Stat(m.profile("desktop")); !os.IsNotExist(err) {
		t.Fatal("gate created a desktop profile")
	}
}
func TestDarwinOwnedProcessCannotKillUnrelatedProcess(t *testing.T) {
	unrelated := exec.Command("/bin/sleep", "20")
	if err := unrelated.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = unrelated.Process.Kill(); _ = unrelated.Wait() }()
	owned := exec.Command("/bin/sleep", "20")
	stop, err := startOwnedDarwin(owned, false)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { _ = owned.Wait(); close(done) }()
	stop()
	stop()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("owned process not reaped")
	}
	if unrelated.ProcessState != nil {
		t.Fatal("unrelated process affected")
	}
	// os.Process.Signal uses the handle retained for exactly this spawned child.
	if err := unrelated.Process.Signal(os.Interrupt); err != nil {
		t.Fatal("unrelated process was already stopped", err)
	}
}
func TestDarwinNativeWrapperResolution(t *testing.T) {
	root := filepath.Join(t.TempDir(), "codex")
	if err := os.MkdirAll(filepath.Join(root, "bin"), 0700); err != nil {
		t.Fatal(err)
	}
	wrapper := filepath.Join(root, "bin", "codex.js")
	if err := os.WriteFile(wrapper, []byte("synthetic wrapper; must not execute"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "package.json"), []byte(`{"name":"@openai/codex"}`), 0600); err != nil {
		t.Fatal(err)
	}
	arch, triple := "arm64", "aarch64-apple-darwin"
	if runtime.GOARCH == "amd64" {
		arch, triple = "x64", "x86_64-apple-darwin"
	}
	native := filepath.Join(root, "node_modules", "@openai", "codex-darwin-"+arch, "vendor", triple, "bin", "codex")
	if err := os.MkdirAll(filepath.Dir(native), 0700); err != nil {
		t.Fatal(err)
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(exe, native); err != nil {
		t.Fatal(err)
	}
	resolved, err := nativeCLIDarwin(wrapper)
	expected, _ := filepath.EvalSymlinks(exe)
	if err != nil || resolved != expected {
		t.Fatal("native npm resolution failed", err)
	}
	if err := os.Remove(native); err != nil {
		t.Fatal(err)
	}
	if _, err := nativeCLIDarwin(wrapper); err == nil {
		t.Fatal("wrapper was accepted as native")
	}
}

func TestDarwinDiscoveryReadOnly(t *testing.T) {
	if os.Getenv("CODEXROUTER_MACOS_DISCOVERY") != "1" {
		t.Skip("opt-in read-only installed-client discovery")
	}
	found := detectDarwin()
	for _, target := range []string{"cli", "desktop"} {
		t.Logf("%s installed=%t version=%s", target, found[target].Path != "", found[target].Version)
	}
	if found["desktop"].Path != "" {
		bundled := filepath.Join(filepath.Dir(filepath.Dir(found["desktop"].Path)), "Resources", "codex-cli", "bin", "codex")
		native, err := nativeCLIDarwin(bundled)
		if err != nil {
			t.Fatal("bundled CLI not safely resolvable", err)
		}
		profile := t.TempDir()
		for _, name := range []string{"home", "work", "codex-home"} {
			if err := os.Mkdir(filepath.Join(profile, name), 0700); err != nil {
				t.Fatal(err)
			}
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, native, "--version")
		cmd.Dir = filepath.Join(profile, "work")
		cmd.Env = Environment(os.Environ(), profile, "")
		output, err := cmd.Output()
		if err != nil {
			t.Fatal("bundled CLI version-only probe failed", err)
		}
		version := strings.TrimSpace(string(output))
		if len(version) > 128 || !strings.HasPrefix(version, "codex-cli ") || strings.ContainsAny(version, "\r\n") {
			t.Fatal("invalid version output")
		}
		t.Logf("desktop embedded CLI version=%s", version)
	}
}
