package router

import (
	"net/http"
	"testing"
)

func TestClientManagementRequiresSameAuthAndCSRF(t *testing.T) {
	s, _, _ := newSecurityServer(t, nil)
	for _, path := range []string{"/api/v1/clients", "/api/v1/clients/action"} {
		method := "GET"
		body := ""
		if path == "/api/v1/clients/action" {
			method = "POST"
			body = `{"target":"cli","action":"start"}`
		}
		if w := securityRequest(s, securityRequestOptions{method: method, path: path, body: body}); w.Code != 401 {
			t.Fatal("未认证访问客户端能力")
		}
	}
	cookie, csrf := securityLogin(t, s)
	in := securityRequestOptions{method: "POST", path: "/api/v1/clients/action", body: `{"target":"cli","action":"start"}`, cookie: cookie, origin: "http://" + s.authority}
	if w := securityRequest(s, in); w.Code != 403 {
		t.Fatal("无CSRF启动客户端")
	}
	in.csrf = csrf
	if w := securityRequest(s, in); w.Code != http.StatusServiceUnavailable {
		t.Fatal("无管理器未显式拒绝")
	}
	in.body = `{"target":"cli","action":"start","command":"arbitrary"}`
	if w := securityRequest(s, in); w.Code != 400 {
		t.Fatal("接收任意命令字段")
	}
}
