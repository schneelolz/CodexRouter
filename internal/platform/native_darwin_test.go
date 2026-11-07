//go:build darwin && cgo

package platform

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestDarwinKeychainCRUDAndIsolation(t *testing.T) {
	before, err := exec.Command("/usr/bin/security", "list-keychains", "-d", "user").Output()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		after, err := exec.Command("/usr/bin/security", "list-keychains", "-d", "user").Output()
		if err != nil || !bytes.Equal(before, after) {
			t.Error("user keychain search list changed", err)
		}
	})
	root := t.TempDir()
	a, err := credentialService(filepath.Join(root, "A"))
	if err != nil {
		t.Fatal(err)
	}
	b, err := credentialService(filepath.Join(root, "B"))
	if err != nil {
		t.Fatal(err)
	}
	c, cleanup, err := privateKeychain(filepath.Join(root, "synthetic.keychain-db"), a)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	during, err := exec.Command("/usr/bin/security", "list-keychains", "-d", "user").Output()
	if err != nil || !bytes.Equal(before, during) {
		t.Fatal("fixture changed user keychain search list", err)
	}
	other := &darwinCredentials{service: b, keychain: c.keychain}
	if _, err := c.Get("synthetic"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing: %v", err)
	}
	if err := c.Set("synthetic", "synthetic-first"); err != nil {
		t.Fatal(err)
	}
	if err := c.Check("synthetic"); err != nil {
		t.Fatal("noninteractive access check failed", err)
	}
	// An already authorized item requires no interaction or ACL changes.
	if err := c.Authorize("synthetic"); err != nil {
		t.Fatal("explicit access check failed", err)
	}
	if err := c.Check("missing"); !errors.Is(err, ErrNotFound) {
		t.Fatal("check must not create missing credential", err)
	}
	if got, err := c.Get("synthetic"); err != nil || got != "synthetic-first" {
		t.Fatal("read failed", err)
	}
	if err := c.Set("synthetic", "synthetic-replaced"); err != nil {
		t.Fatal(err)
	}
	if _, err := other.Get("synthetic"); !errors.Is(err, ErrNotFound) {
		t.Fatal("namespace leaked", err)
	}
	if err := other.Delete("synthetic"); err != nil {
		t.Fatal(err)
	}
	if got, err := c.Get("synthetic"); err != nil || got != "synthetic-replaced" {
		t.Fatal("namespace delete leaked", err)
	}
	for range 2 {
		if err := c.Delete("synthetic"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := c.Get("synthetic"); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	if err := c.Set("bad/name", "synthetic"); err == nil {
		t.Fatal("invalid name accepted")
	}
	for _, value := range []string{"", string(make([]byte, 2561))} {
		if err := c.Set("synthetic", value); err == nil {
			t.Fatal("invalid length accepted")
		}
	}
}
func TestDarwinKeychainErrors(t *testing.T) {
	for _, code := range []int{-25293, -25308, -25291, -50} {
		// Fixed native status mapping must fail closed without exposing native response text.
		err := keychainError("test", code)
		var native *CredentialError
		if !errors.As(err, &native) || native.Status != code || errors.Is(err, ErrNotFound) {
			t.Fatal("native failure hidden or misclassified")
		}
	}
}
func TestDarwinKeychainLockedFailsClosed(t *testing.T) {
	c, cleanup, err := privateKeychain(filepath.Join(t.TempDir(), "locked.keychain-db"), "CodexRouter/synthetic-locked")
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	if err := c.Set("synthetic", "synthetic-lock-test"); err != nil {
		t.Fatal(err)
	}
	if err := c.lockFixture(); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Get("synthetic"); err == nil || errors.Is(err, ErrNotFound) {
		t.Fatal("locked keychain must report failure", err)
	}
	if err := c.Check("synthetic"); err == nil || errors.Is(err, ErrNotFound) {
		t.Fatal("locked check must not prompt or claim missing", err)
	}
	if err := c.Set("synthetic", "synthetic-write"); err == nil {
		t.Fatal("locked keychain accepted write")
	}
}

func TestDarwinCredentialDirectoryIdentity(t *testing.T) {
	root := t.TempDir()
	a, _ := credentialService(filepath.Join(root, "Case"))
	b, _ := credentialService(filepath.Join(root, "case"))
	if a == b {
		t.Fatal("case was folded")
	}
	if err := os.Symlink(root, filepath.Join(root, "alias")); err != nil {
		t.Fatal(err)
	}
	alias, err := credentialService(filepath.Join(root, "alias", "Case"))
	if err != nil || alias != a {
		t.Fatal("alias identity mismatch", err)
	}
	if _, err := credentialService(""); err == nil {
		t.Fatal("empty accepted")
	}
	exact := filepath.Join(root, "OnDiskCase")
	if err := os.Mkdir(exact, 0700); err != nil {
		t.Fatal(err)
	}
	lower := filepath.Join(root, "ondiskcase")
	if info, err := os.Stat(lower); err == nil {
		original, _ := os.Stat(exact)
		if os.SameFile(original, info) {
			one, _ := credentialService(exact)
			two, _ := credentialService(lower)
			if one != two {
				t.Fatal("case-insensitive alias split the namespace")
			}
		}
	}
}
