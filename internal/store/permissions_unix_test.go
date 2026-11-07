//go:build darwin || linux

package store

import (
	"os"
	"path/filepath"
	"testing"
)

func TestPrivateDirectoryPermissions(t *testing.T) {
	root := t.TempDir()
	info, _ := os.Stat(root)
	t.Logf("temporary root mode=%o uid=%d stat=%T", info.Mode().Perm(), os.Geteuid(), info.Sys())
	dir := filepath.Join(root, "private")
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	info, err = os.Stat(dir)
	if err != nil || info.Mode().Perm() != 0700 {
		t.Fatal("directory not private", err)
	}
	if err := s.Write("private.json", map[string]bool{"synthetic": true}); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"private.json", ".instance.lock"} {
		info, err := os.Stat(filepath.Join(dir, name))
		if err != nil || info.Mode().Perm() != 0600 {
			t.Fatal("file not private", name, err)
		}
	}
	unsafe := filepath.Join(root, "public")
	if err := os.Mkdir(unsafe, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(unsafe, 0755); err != nil {
		t.Fatal(err)
	}
	if s, err := Open(unsafe); err == nil {
		s.Close()
		t.Fatal("unsafe existing directory accepted")
	}
}
