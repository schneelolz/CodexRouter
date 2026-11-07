//go:build darwin

package router

import (
	"codexrouter/internal/clients"
	"path/filepath"
	"strings"
	"testing"
)

func TestDarwinClientAPIFailsClosedWithActionableError(t *testing.T) {
	s, _, _ := newSecurityServer(t, nil)
	manager, err := clients.New(filepath.Join(t.TempDir(), "profiles"), "http://127.0.0.1:19999")
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	s.clientManager = manager
	cookie, csrf := securityLogin(t, s)
	for _, test := range []struct{ body, code string }{
		{`{"target":"desktop","action":"start"}`, "CLIENT_DESKTOP_ISOLATION_UNVERIFIED"},
		{`{"target":"cli","action":"login"}`, "CLIENT_MACOS_PROBE_ONLY"},
	} {
		w := securityRequest(s, securityRequestOptions{method: "POST", path: "/api/v1/clients/action", body: test.body, cookie: cookie, csrf: csrf, origin: "http://" + s.authority})
		if w.Code != 409 || !strings.Contains(w.Body.String(), test.code) {
			t.Fatal("platform gate not exposed safely", w.Code)
		}
	}
}
