//go:build darwin

package daily

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDarwinPermissionsAliasesAndOwnerCase(t *testing.T) {
	m := fixture(t, "model = 'synthetic'\n[mcp_servers.fixture]\nurl='https://example.invalid'\n")
	if err := os.Chmod(m.config, 0640); err != nil {
		t.Fatal(err)
	}
	original := content(t, m)
	apply(t, m, "route")
	apply(t, m, "restore")
	if content(t, m) != original {
		t.Fatal("unrelated bytes changed")
	}
	info, err := os.Stat(m.config)
	if err != nil || info.Mode().Perm() != 0640 {
		t.Fatal("permissions changed", err)
	}
	a, _ := New(m.home, m.base, "/synthetic/Case")
	b, _ := New(m.home, m.base, "/synthetic/case")
	if a.owner == b.owner {
		t.Fatal("macOS owner identity was case folded")
	}
	if !systemPathAlias("/var") || systemPathAlias(filepath.Join(m.home, "alias")) {
		t.Fatal("only system aliases may be accepted")
	}
}
func TestDarwinConfigSymlinkAndAncestorRejected(t *testing.T) {
	m := fixture(t, "model='synthetic'\n")
	target := filepath.Join(m.home, "real-config")
	if err := os.Rename(m.config, target); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, m.config); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Preview("route"); err == nil {
		t.Fatal("config symlink accepted")
	}
	root := t.TempDir()
	alias := filepath.Join(root, "alias")
	if err := os.Symlink(m.home, alias); err != nil {
		t.Fatal(err)
	}
	other, _ := New(alias, m.base, "synthetic")
	if _, err := other.Preview("route"); err == nil {
		t.Fatal("ancestor symlink accepted")
	}
}
