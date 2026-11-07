//go:build darwin

package main

import (
	"path/filepath"
	"testing"
)

func TestDarwinBundleUIResources(t *testing.T) {
	app := "/synthetic/CodexRouter.app/Contents/MacOS/codexrouter"
	if got := defaultUIDir(app); got != "/synthetic/CodexRouter.app/Contents/Resources/ui" {
		t.Fatal(got)
	}
	if got := defaultUIDir("/synthetic/codexrouter"); got != filepath.Join("/synthetic", "ui") {
		t.Fatal(got)
	}
}
