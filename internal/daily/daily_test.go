package daily

import (
	"bytes"
	"codexrouter/internal/store"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func fixture(t *testing.T, text string) *Manager {
	t.Helper()
	home := t.TempDir()
	if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte(text), 0600); err != nil {
		t.Fatal(err)
	}
	m, err := New(home, "http://127.0.0.1:19971", "synthetic-owner")
	if err != nil {
		t.Fatal(err)
	}
	return m
}
func content(t *testing.T, m *Manager) string {
	t.Helper()
	b, err := os.ReadFile(m.config)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
func apply(t *testing.T, m *Manager, action string) {
	t.Helper()
	p, err := m.Preview(action)
	if err != nil {
		t.Fatal(err)
	}
	if err = m.Apply(p.Token); err != nil {
		t.Fatal(err)
	}
}
func TestRouteRestorePreservesUnrelatedBytesAndFiles(t *testing.T) {
	for _, original := range []string{"# comment\r\nmodel = 'keep'\r\n[projects.'C:/work']\r\ntrust_level = 'trusted'\r\n", "# comment\nopenai_base_url = 'https://original.example.test/api' # retain me\nmodel='keep'\n", "model='keep'"} {
		m := fixture(t, original)
		// 合成哨兵不是认证内容；不使用实际用户文件。
		auth := filepath.Join(m.home, "auth.json")
		history := filepath.Join(m.home, "history.jsonl")
		_ = os.WriteFile(auth, []byte("synthetic-auth-sentinel"), 0600)
		_ = os.WriteFile(history, []byte("synthetic-history-sentinel"), 0600)
		apply(t, m, "route")
		if !strings.Contains(content(t, m), m.base) {
			t.Fatal("未接入")
		}
		if m.Status().State != "configured" || !m.Status().HasBackup {
			t.Fatal("状态不符")
		}
		current := content(t, m) + "\n# external new setting\n"
		_ = os.WriteFile(m.config, []byte(current), 0600)
		apply(t, m, "restore")
		if content(t, m) != original+"\n# external new setting\n" {
			t.Fatalf("恢复改变其他字节: %q", content(t, m))
		}
		for path, want := range map[string]string{auth: "synthetic-auth-sentinel", history: "synthetic-history-sentinel"} {
			raw, _ := os.ReadFile(path)
			if string(raw) != want {
				t.Fatal("触碰非配置文件")
			}
		}
	}
}
func TestPreviewCASExpiryAndOwnership(t *testing.T) {
	m := fixture(t, "model='keep'\n")
	p, err := m.Preview("route")
	if err != nil {
		t.Fatal(err)
	}
	original := content(t, m) + "# external\n"
	_ = os.WriteFile(m.config, []byte(original), 0600)
	if err = m.Apply(p.Token); err == nil || err.Error() != "DAILY_CONFIG_CHANGED" {
		t.Fatal("没有检测修改", err)
	}
	if content(t, m) != original {
		t.Fatal("覆盖了外部修改")
	}
	if err = m.Apply(p.Token); err == nil {
		t.Fatal("token可重复使用")
	}
	apply(t, m, "route")
	other, _ := New(m.home, "http://127.0.0.1:19972", "different-owner")
	if _, err = other.Preview("route"); err == nil {
		t.Fatal("跨实例覆盖")
	}
	updated, _ := New(m.home, "http://127.0.0.1:19973", "synthetic-owner")
	if updated.Status().State != "port_changed" {
		t.Fatal("未发现端口变化")
	}
	apply(t, updated, "route")
	apply(t, updated, "restore")
	if content(t, m) != original {
		t.Fatal("多次接入丢失原始字段")
	}
}
func TestUnsupportedConfigsAndForeignChanges(t *testing.T) {
	for _, text := range []string{"model_provider='other'", "profile='work'", "[profiles.work]\nmodel='x'", "openai_base_url=123", "[openai_base_url]\nx=1", "openai_base_url='https://user:secret@x.test'", "[model_providers.openai]\nbase_url='https://x.test'", "openai_base_url='https://x.test/?token=secret'", "invalid=[", "openai_base_url='https://x.test'\nopenai_base_url='https://y.test'"} {
		m := fixture(t, text)
		if _, err := m.Preview("route"); err == nil {
			t.Fatalf("未拒绝配置 %q", text)
		}
		if content(t, m) != text {
			t.Fatal("拒绝时改了文件")
		}
	}
	m := fixture(t, "model='x'\n")
	apply(t, m, "route")
	raw := strings.ReplaceAll(content(t, m), m.base, "https://externally.changed.test")
	_ = os.WriteFile(m.config, []byte(raw), 0600)
	if _, err := m.Preview("restore"); err == nil {
		t.Fatal("恢复覆盖外部字段")
	}
}
func TestJournalRecoveryAndBusy(t *testing.T) {
	m := fixture(t, "model='keep'\n")
	raw, s, err := m.read()
	if err != nil {
		t.Fatal(err)
	}
	next, rec, err := m.prepare("route", raw, s, record{})
	if err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(m.dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = m.Preview("route"); err == nil {
		t.Fatal("未拒绝锁冲突")
	}
	if err = db.Write("journal.json", journal{Pending: true, Before: sum(raw), After: sum(next), Next: rec}); err != nil {
		t.Fatal(err)
	}
	db.Close()
	if err = replaceConfig(m.config, raw, next); err != nil {
		t.Fatal(err)
	}
	if m.Status().State != "configured" {
		t.Fatal("替换后日志未识别")
	}
	apply(t, m, "restore")
	if !bytes.Equal([]byte(content(t, m)), raw) {
		t.Fatal("日志恢复失败")
	}
}
func TestJournalBeforeReplacementCanRetryWithoutInvalidRecord(t *testing.T) {
	m := fixture(t, "model='keep'\n")
	raw, s, err := m.read()
	if err != nil {
		t.Fatal(err)
	}
	next, rec, err := m.prepare("route", raw, s, record{})
	if err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(m.dir)
	if err != nil {
		t.Fatal(err)
	}
	if err = db.Write("journal.json", journal{Pending: true, Before: sum(raw), After: sum(next), Next: rec}); err != nil {
		t.Fatal(err)
	}
	db.Close()
	if m.Status().State != "direct" {
		t.Fatal("未替换时不应接入")
	}
	apply(t, m, "route")
	apply(t, m, "restore")
	if content(t, m) != string(raw) {
		t.Fatal("首次写入前崩溃重试损坏了原配置")
	}
}

func TestModelAndCommentPreservedByValuePatch(t *testing.T) {
	text := "'openai_base_url' = \"https://x.test\" # original\r\n[projects.'C:/a']\r\nopenai_base_url='nested-not-touched'\r\n"
	m := fixture(t, text)
	apply(t, m, "route")
	apply(t, m, "restore")
	if content(t, m) != text {
		t.Fatal("注释/嵌套字段被改写")
	}
}
