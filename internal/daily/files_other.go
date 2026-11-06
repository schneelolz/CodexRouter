//go:build !windows

package daily

import "os"

func isReparse(os.FileInfo) bool                { return false }
func atomicConfigReplace(from, to string) error { return os.Rename(from, to) }
