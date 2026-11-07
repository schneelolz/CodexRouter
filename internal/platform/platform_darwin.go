//go:build darwin

package platform

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

func DefaultDataDir() (string, error) {
	base, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "CodexRouter"), nil
}

var canonicalCredentialPath = filepath.EvalSymlinks

func credentialService(dataDir string) (string, error) {
	identity, err := DataDirectoryIdentity(dataDir)
	if err != nil {
		return "", err
	}
	profile, err := readCredentialProfile(identity, profileActiveFile)
	if err != nil {
		return "", err
	}
	id := ""
	if profile != nil {
		id = profile.ID
	}
	return credentialServiceForProfile(identity, id)
}

func credentialServiceForProfile(dataDir, id string) (string, error) {
	if id != "" && !profileID.MatchString(id) {
		return "", profileError()
	}
	identity, err := DataDirectoryIdentity(dataDir)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256([]byte(identity))
	service := "CodexRouter/" + hex.EncodeToString(sum[:])
	if id != "" {
		service += "/profile/" + id
	}
	return service, nil
}

// DataDirectoryIdentity preserves case and resolves aliases, including absent leaf directories.
func DataDirectoryIdentity(dataDir string) (string, error) {
	if dataDir == "" {
		return "", errors.New("凭据数据目录不能为空")
	}
	abs, err := filepath.Abs(dataDir)
	if err != nil {
		return "", err
	}
	parent := abs
	var tail []string
	for {
		resolved, e := canonicalCredentialPath(parent)
		if e == nil {
			for i := len(tail) - 1; i >= 0; i-- {
				resolved = filepath.Join(resolved, tail[i])
			}
			return filepath.Clean(resolved), nil
		}
		if !errors.Is(e, os.ErrNotExist) || filepath.Dir(parent) == parent {
			return "", fmt.Errorf("解析凭据目录: %w", e)
		}
		tail = append(tail, filepath.Base(parent))
		parent = filepath.Dir(parent)
	}
}

func OpenBrowser(raw string) error {
	if err := validateBrowserURL(raw); err != nil {
		return err
	}
	// No shell, and no URL/token in errors or logs.
	if err := exec.Command("/usr/bin/open", raw).Run(); err != nil {
		return errors.New("打开系统浏览器失败")
	}
	return nil
}
