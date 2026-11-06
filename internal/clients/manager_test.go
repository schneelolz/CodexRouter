package clients

import (
	"encoding/json"
	"github.com/pelletier/go-toml/v2"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func testManager(t *testing.T) *Manager {
	t.Helper()
	return &Manager{root: t.TempDir(), base: "http://127.0.0.1:19910", installed: map[string]installation{}, active: map[string]*child{}, last: map[string]string{}}
}
func TestEnvironmentRemovesInheritedIdentityAndProxies(t *testing.T) {
	env := Environment([]string{"PATH=C:/Windows", "CODEX_HOME=daily", "CODEX_APP_SERVER_WS_URL=bad", "OPENAI_API_KEY=synthetic", "Pi_SESSION_ID=private", "hTTps_proxy=bad", "NODE_OPTIONS=bad", "ELECTRON_RUN_AS_NODE=1"}, "C:/isolated", "http://127.0.0.1:19910")
	text := strings.Join(env, "\n")
	for _, bad := range []string{"daily", "synthetic", "private", "bad"} {
		if strings.Contains(text, bad) {
			t.Fatal("继承了未授权身份或代理")
		}
	}
	if !strings.Contains(text, "CODEX_HOME=") || !strings.Contains(text, "CODEX_ELECTRON_USER_DATA_PATH=") || !strings.Contains(text, "CODEX_APP_SERVER_OPENAI_BASE_URL=") {
		t.Fatal("缺少隔离环境")
	}
	direct := strings.Join(Environment(env, "C:/isolated", ""), "\n")
	if strings.Contains(direct, "BASE_URL=") {
		t.Fatal("直连仍有覆盖地址")
	}
}
func TestManagedProfileSwitchAndConflict(t *testing.T) {
	m := testManager(t)
	if mode, err := m.ensureProfile("cli", ""); err != nil || mode != "routed" {
		t.Fatalf("初始化: %s %v", mode, err)
	}
	p := filepath.Join(m.profile("cli"), "codex-home", "config.toml")
	first, _ := os.ReadFile(p)
	if !strings.Contains(string(first), m.base) {
		t.Fatal("路由地址未写入")
	}
	if _, err := m.ensureProfile("cli", "direct"); err != nil {
		t.Fatal(err)
	}
	direct, _ := os.ReadFile(p)
	if strings.Contains(string(direct), "openai_base_url") {
		t.Fatal("直连未撤销地址")
	}
	backup, _ := os.ReadFile(filepath.Join(m.profile("cli"), "config.previous.toml"))
	if string(backup) != string(first) {
		t.Fatal("未备份本程序配置")
	}
	if _, err := m.ensureProfile("cli", "direct"); err != nil {
		t.Fatal("恢复不幂等")
	}
	var external map[string]any
	if err := toml.Unmarshal(direct, &external); err != nil {
		t.Fatal(err)
	}
	external["model_provider"] = "unexpected"
	modified, err := toml.Marshal(external)
	if err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(p, modified, 0600)
	if _, err := m.ensureProfile("cli", "routed"); err == nil {
		t.Fatal("覆盖了外部更改")
	}
	current, _ := os.ReadFile(p)
	if string(current) != string(modified) {
		t.Fatal("冲突后配置改变")
	}
}
func TestForeignProfileAndActionsRejected(t *testing.T) {
	m := testManager(t)
	dir := filepath.Join(m.profile("cli"), "codex-home")
	_ = os.MkdirAll(dir, 0700)
	_ = os.WriteFile(filepath.Join(dir, "config.toml"), []byte("foreign"), 0600)
	if _, err := m.ensureProfile("cli", ""); err == nil {
		t.Fatal("接管了非托管配置")
	}
	for _, in := range []Input{{"../daily", "start"}, {"cli", "powershell"}, {"desktop;cmd", "start"}} {
		if _, err := m.Action(in); err == nil {
			t.Fatal("允许任意动作")
		}
	}
	m.active["desktop"] = &child{}
	if _, err := m.Action(Input{"desktop", "direct"}); err == nil || err.Error() != "CLIENT_RUNNING" {
		t.Fatal("活动中更改模式")
	}
}
func TestProfilesAreIndependentAndStatusHasNoCredentials(t *testing.T) {
	m := testManager(t)
	for _, target := range []string{"cli", "desktop"} {
		if _, err := m.ensureProfile(target, ""); err != nil {
			t.Fatal(err)
		}
	}
	if m.profile("cli") == m.profile("desktop") {
		t.Fatal("共享profile")
	}
	raw, _ := json.Marshal(m.Status())
	if strings.Contains(string(raw), "auth.json") || strings.Contains(string(raw), "token") {
		t.Fatal("状态泄露认证")
	}
}
func TestManagedPortChangeRefreshesOnlyUnmodifiedConfig(t *testing.T) {
	m := testManager(t)
	if _, err := m.ensureProfile("cli", ""); err != nil {
		t.Fatal(err)
	}
	m.base = "http://127.0.0.1:19920"
	if _, err := m.ensureProfile("cli", ""); err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(filepath.Join(m.profile("cli"), "codex-home", "config.toml"))
	if err != nil || !strings.Contains(string(content), ":19920") || strings.Contains(string(content), ":19910") {
		t.Fatal("继续使用旧端口")
	}
}
func TestStatusDoesNotExposeInvalidMode(t *testing.T) {
	m := testManager(t)
	_ = os.MkdirAll(m.profile("cli"), 0700)
	_ = os.WriteFile(filepath.Join(m.profile("cli"), "managed-profile.json"), []byte(`{"version":1,"mode":"bad"}`), 0600)
	if m.Status().Items[0].Mode != "routed" {
		t.Fatal("公开契约返回非法mode")
	}
}

func TestDesktopAddedSettingsSurviveRoutingModeChanges(t *testing.T) {
	m := testManager(t)
	if _, err := m.ensureProfile("desktop", ""); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(m.profile("desktop"), "codex-home", "config.toml")
	original, _ := os.ReadFile(p)
	original = append(original, []byte("\n[projects.'C:/Synthetic/work']\ntrust_level = 'trusted'\n")...)
	if err := os.WriteFile(p, original, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := m.ensureProfile("desktop", ""); err != nil {
		t.Fatal("桌面正常补充设置被误拒绝", err)
	}
	if _, err := m.ensureProfile("desktop", "direct"); err != nil {
		t.Fatal(err)
	}
	updated, _ := os.ReadFile(p)
	if !strings.Contains(string(updated), "trust_level") || !strings.Contains(string(updated), "trusted") || strings.Contains(string(updated), "openai_base_url") {
		t.Fatal("切模式丢失客户端设置")
	}
}

func TestNewRejectsExternalOrMalformedAddress(t *testing.T) {
	for _, base := range []string{"https://127.0.0.1:1", "http://example.test:1", "http://127.0.0.1:1/v1", "http://x:y@127.0.0.1:1", "http://127.0.0.1"} {
		if _, err := New(t.TempDir(), base); err == nil {
			t.Fatal("错误基址被接受")
		}
	}
}
