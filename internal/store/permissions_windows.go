//go:build windows

package store

import "os"

func validateDataDirectory(os.FileInfo) error { return nil }
