package platform

import "testing"

func TestBrowserURLIsLocalHTTP(t *testing.T) {
	for _, raw := range []string{"http://127.0.0.1:18788/#bootstrap=synthetic", "http://[::1]:18788", "http://localhost:18788"} {
		if err := validateBrowserURL(raw); err != nil {
			t.Errorf("拒绝本机地址: %v", err)
		}
	}
	for _, raw := range []string{"file:///etc/passwd", "https://example.com", "http://localhost.evil.test", "http://user@localhost", "javascript:alert(1)", "http://0.0.0.0:18788"} {
		if err := validateBrowserURL(raw); err == nil {
			t.Errorf("允许非本机浏览器地址: %q", raw)
		}
	}
}
