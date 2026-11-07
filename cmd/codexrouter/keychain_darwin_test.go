//go:build darwin

package main

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestKeychainNamesOnlyExistingConfiguredAccounts(t *testing.T) {
	root := t.TempDir()
	names, err := keychainNames(root)
	if err != nil || !reflect.DeepEqual(names, []string{"control"}) {
		t.Fatal("missing state check failed", err)
	}
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) != 0 {
		t.Fatal("read-only check created files")
	}
	state := filepath.Join(root, "state.json")
	if err := os.WriteFile(state, []byte(`{"version":2,"providers":[{"id":"official","kind":"official"},{"id":"company","kind":"api_key"},{"id":"p-synthetic","kind":"api_key"}]}`), 0600); err != nil {
		t.Fatal(err)
	}
	names, err = keychainNames(root)
	if err != nil || !reflect.DeepEqual(names, []string{"control", "company", "p-synthetic"}) {
		t.Fatal("wrong accounts selected", err)
	}
	for _, raw := range []string{`{`, `{"providers":[{"id":"../bad","kind":"api_key"}]}`, `{"providers":[{"id":"control","kind":"api_key"}]}`, `{"providers":[{"id":"duplicate","kind":"api_key"},{"id":"duplicate","kind":"api_key"}]}`} {
		if err := os.WriteFile(state, []byte(raw), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := keychainNames(root); err == nil {
			t.Fatal("invalid account selection accepted")
		}
	}
	if err := os.Remove(state); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(root, "original.json")
	if err := os.WriteFile(target, []byte(`{"providers":[]}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, state); err != nil {
		t.Fatal(err)
	}
	if _, err := keychainNames(root); err == nil {
		t.Fatal("state symlink accepted")
	}
}

func TestKeychainProfilePreviewAndConfirmGuard(t *testing.T) {
	root := filepath.Join(t.TempDir(), "private")
	if err := os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	if err := runKeychainProfileCommand([]string{"preview", "--data-dir", root}); err != nil {
		t.Fatal(err)
	}
	for _, action := range []string{"prepare", "activate", "rollback"} {
		if err := runKeychainProfileCommand([]string{action, "--data-dir", root}); err == nil {
			t.Fatal("mutating command accepted without explicit confirm", action)
		}
	}
	for _, args := range [][]string{{}, {"unsupported"}, {"preview", "--data-dir", root, "unexpected"}} {
		if err := runKeychainProfileCommand(args); err == nil {
			t.Fatal("invalid profile command accepted")
		}
	}
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) != 0 {
		t.Fatal("preview or denied mutation wrote files")
	}
}

func TestKeychainCommandDoesNotCreateAbsentDirectory(t *testing.T) {
	root := filepath.Join(t.TempDir(), "missing")
	for _, action := range []string{"keychain-check", "keychain-authorize"} {
		handled, err := runKeychainCommand([]string{action, "--data-dir", root})
		if !handled || err == nil {
			t.Fatal("absent directory should fail before Keychain", action)
		}
		if _, err := os.Stat(root); !os.IsNotExist(err) {
			t.Fatal("command created data directory")
		}
	}
}
