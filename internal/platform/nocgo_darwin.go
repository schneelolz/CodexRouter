//go:build darwin && !cgo

package platform

import "errors"

func OpenCredentials(string) (Credentials, error) {
	return nil, errors.New("macOS Keychain 和菜单栏需要 CGO_ENABLED=1 及 Apple Command Line Tools；不提供明文回退")
}
