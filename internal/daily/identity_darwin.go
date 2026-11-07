//go:build darwin

package daily

import (
	"codexrouter/internal/platform"
	"os"
)

func systemPathAlias(path string) bool {
	target := map[string]string{"/var": "private/var", "/tmp": "private/tmp", "/etc": "private/etc"}[path]
	if target == "" {
		return false
	}
	link, err := os.Readlink(path)
	return err == nil && (link == target || link == "/"+target)
}
func ownerIdentity(owner string) string {
	if identity, err := platform.DataDirectoryIdentity(owner); err == nil {
		return identity
	}
	return owner
}
