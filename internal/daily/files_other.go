//go:build !windows

package daily

import (
	"os"
	"path/filepath"
)

func isReparse(os.FileInfo) bool { return false }
func atomicConfigReplace(from, to string) error {
	if err := os.Rename(from, to); err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(to))
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}
