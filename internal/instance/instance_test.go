package instance

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestOpenAuthenticatesBothSidesAndRejectsFakePort(t *testing.T) {
	key := strings.Repeat("synthetic", 8)
	calls := 0
	server := httptest.NewServer(Handler(key, func() error { calls++; return nil }))
	defer server.Close()
	if err := Open(server.URL, key); err != nil || calls != 1 {
		t.Fatalf("可信实例未打开: %v", err)
	}
	if err := Open(server.URL, strings.Repeat("wrong", 16)); err == nil || calls != 1 {
		t.Fatal("错误密钥能控制实例")
	}
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"nonce":"fake","proof":"fake"}`))
	}))
	defer fake.Close()
	if err := Open(fake.URL, key); err == nil {
		t.Fatal("假端口被当作可信实例")
	}
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, server.URL+Path, 307) }))
	defer redirect.Close()
	if err := Open(redirect.URL, key); err == nil || calls != 1 {
		t.Fatal("跟随了本地控制重定向")
	}
}
func TestControlRejectsReplayOriginAndInvalidBodies(t *testing.T) {
	key := strings.Repeat("s", 64)
	calls := 0
	handler := Handler(key, func() error { calls++; return nil })
	nonce := strconv.FormatInt(time.Now().Unix(), 10) + "-" + strings.Repeat("a", 64)
	raw, _ := json.Marshal(message{nonce, sign(key, "open", nonce)})
	for i := 0; i < 2; i++ {
		r := httptest.NewRequest("POST", Path, bytes.NewReader(raw))
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		if (i == 0 && w.Code != 200) || (i == 1 && w.Code != 403) {
			t.Fatal("防重放失败")
		}
	}
	r := httptest.NewRequest("POST", Path, bytes.NewReader(raw))
	r.Header.Set("Origin", "http://evil.test")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	if w.Code != 403 || calls != 1 {
		t.Fatal("接受了浏览器跨站控制")
	}
	failed := httptest.NewServer(Handler(key, func() error { return errors.New("synthetic") }))
	defer failed.Close()
	if err := Open(failed.URL, key); err == nil {
		t.Fatal("页面打开失败伪报成功")
	}
}
