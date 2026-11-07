//go:build !darwin

package daily

import "strings"

func systemPathAlias(string) bool       { return false }
func ownerIdentity(owner string) string { return strings.ToLower(owner) }
