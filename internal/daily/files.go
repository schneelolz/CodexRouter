package daily

import (
	"bytes"
	"os"
	"path/filepath"
)

func safePath(path string, directory bool) error {
	for p := path; ; p = filepath.Dir(p) {
		f, err := os.Lstat(p)
		if err != nil || (f.Mode()&os.ModeSymlink != 0 && !systemPathAlias(p)) || isReparse(f) {
			return failure("DAILY_UNSUPPORTED")
		}
		if p == path {
			if directory && !f.IsDir() || !directory && !f.Mode().IsRegular() {
				return failure("DAILY_UNSUPPORTED")
			}
		} else if !f.IsDir() && !systemPathAlias(p) {
			return failure("DAILY_UNSUPPORTED")
		}
		if filepath.Dir(p) == p {
			break
		}
	}
	return nil
}
func safeOptionalDir(path string) error {
	if _, err := os.Lstat(path); os.IsNotExist(err) {
		return nil
	}
	return safePath(path, true)
}
func replaceConfig(path string, before, after []byte) error {
	info, err := os.Stat(path)
	if err != nil {
		return failure("DAILY_UNAVAILABLE")
	}
	temp, err := os.CreateTemp(filepath.Dir(path), ".codexrouter-config-*")
	if err != nil {
		return failure("DAILY_WRITE_FAILED")
	}
	name := temp.Name()
	defer os.Remove(name)
	if err = temp.Chmod(info.Mode().Perm()); err == nil {
		_, err = temp.Write(after)
	}
	if err == nil {
		err = temp.Sync()
	}
	closeErr := temp.Close()
	if err != nil || closeErr != nil {
		return failure("DAILY_WRITE_FAILED")
	}
	if err = safePath(path, false); err != nil {
		return err
	}
	current, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(current, before) {
		return failure("DAILY_CONFIG_CHANGED")
	}
	if err = atomicConfigReplace(name, path); err != nil {
		return failure("DAILY_WRITE_FAILED")
	}
	return nil
}
