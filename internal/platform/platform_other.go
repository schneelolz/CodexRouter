//go:build !windows && !darwin

package platform

import (
	"fmt"
	"os"
	"path/filepath"
)

// DefaultDataDir 返回当前用户的 CodexRouter 数据目录。
func DefaultDataDir() (string, error) {
	base, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("获取用户配置目录: %w", err)
	}
	return filepath.Join(base, "CodexRouter"), nil
}

// OpenCredentials 明确报告尚未实现的系统凭据后端。
func OpenCredentials(dataDir string) (Credentials, error) {
	return nil, ErrUnsupported
}

// OpenBrowser 明确报告尚未实现的浏览器平台适配。
func OpenBrowser(raw string) error {
	if err := validateBrowserURL(raw); err != nil {
		return err
	}
	return fmt.Errorf("当前平台尚未实现浏览器启动")
}
