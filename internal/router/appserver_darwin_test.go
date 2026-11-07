//go:build darwin

package router

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"codexrouter/internal/clients"
)

// Explicit synthetic app-server probe. Never launch the desktop or attach to an existing daemon.
func TestIsolatedDarwinAppServerForkIdentity(t *testing.T) {
	if os.Getenv("CODEXROUTER_APP_SERVER_PROBE") != "1" {
		t.Skip("opt-in isolated bundled app-server probe")
	}
	exe, err := clients.ProbeExecutable(os.Getenv("CODEX_PROBE_EXE"))
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	for _, dir := range []string{"home", "work", "codex-home"} {
		if err := os.Mkdir(filepath.Join(root, dir), 0700); err != nil {
			t.Fatal(err)
		}
	}
	env := clients.Environment(os.Environ(), root, "")
	auth := []byte(`{"OPENAI_API_KEY":"synthetic-app-server-only"}`)
	if err := os.WriteFile(filepath.Join(root, "codex-home", "auth.json"), auth, 0600); err != nil {
		t.Fatal(err)
	}
	type shape struct{ Thread, Session, MetaThread, MetaSession, Parent, History string }
	var mu sync.Mutex
	var shapes []shape
	var seq, officialCalls, companyCalls int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/models" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"models":[]}`)
			return
		}
		if r.Method == "GET" {
			w.WriteHeader(426)
			return
		}
		if r.URL.Path != "/responses" {
			http.NotFound(w, r)
			return
		}
		_, _ = io.Copy(io.Discard, io.LimitReader(r.Body, 16<<20))
		mu.Lock()
		switch r.Header.Get("Authorization") {
		case "Bearer synthetic-app-server-only":
			officialCalls++
		case "Bearer synthetic-company-only":
			companyCalls++
			for _, name := range []string{"Cookie", "Chatgpt-Account-Id", "Thread-Id", "Session-Id", "X-Codex-Turn-Metadata"} {
				if r.Header.Get(name) != "" {
					t.Error("third-party identity leak", name)
				}
			}
		default:
			t.Error("unexpected synthetic upstream credential")
		}
		seq++
		n := seq
		mu.Unlock()
		writeSyntheticCodexSSE(w, n)
	}))
	defer upstream.Close()
	router, host := liveRouter(t, upstream.URL)
	if err := router.creds.Set(BuiltinCompanyID, "synthetic-company-only"); err != nil {
		t.Fatal(err)
	}
	config := router.state.config()
	config.ConnectTimeoutMS, config.HeaderTimeoutMS, config.IdleTimeoutMS, config.WriteTimeoutMS = 5000, 5000, 5000, 5000
	if err := router.state.saveConfig(config); err != nil {
		t.Fatal(err)
	}
	var stateMu sync.RWMutex
	host.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		stateMu.RLock()
		defer stateMu.RUnlock()
		if r.Method == http.MethodPost && r.URL.Path == "/responses" {
			var m threadEvidence
			_ = json.Unmarshal([]byte(r.Header.Get("X-Codex-Turn-Metadata")), &m)
			mu.Lock()
			shapes = append(shapes, shape{r.Header.Get("Thread-Id"), r.Header.Get("Session-Id"), m.ThreadID, m.SessionID, m.Parent, m.History})
			mu.Unlock()
		}
		router.ServeHTTP(w, r)
	})
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, exe, "app-server", "--stdio", "-c", `model_provider="openai"`, "-c", fmt.Sprintf("openai_base_url=%q", host.URL), "-c", fmt.Sprintf("chatgpt_base_url=%q", host.URL), "-c", `cli_auth_credentials_store="file"`, "-c", `web_search="disabled"`, "-c", "features.shell_tool=false", "-c", "features.unified_exec=false", "-c", "features.code_mode=false", "-c", "features.apply_patch_freeform=false", "-c", "analytics.enabled=false", "-c", "feedback.enabled=false")
	cmd.Dir = filepath.Join(root, "work")
	cmd.Env = env
	cmd.Stderr = io.Discard
	input, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	output, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = input.Close(); _ = cmd.Process.Kill(); _ = cmd.Wait() }()
	messages := make(chan map[string]json.RawMessage, 128)
	go func() {
		defer close(messages)
		scanner := bufio.NewScanner(output)
		scanner.Buffer(make([]byte, 4096), 8<<20)
		for scanner.Scan() {
			var message map[string]json.RawMessage
			if json.Unmarshal(scanner.Bytes(), &message) == nil {
				select {
				case messages <- message:
				case <-ctx.Done():
					return
				}
			}
		}
	}()
	next := 0
	completed := map[string]string{}
	handle := func(msg map[string]json.RawMessage) {
		var method string
		_ = json.Unmarshal(msg["method"], &method)
		if method == "turn/completed" {
			var params struct{ Turn struct{ ID, Status string } }
			_ = json.Unmarshal(msg["params"], &params)
			completed[params.Turn.ID] = params.Turn.Status
		}
		if msg["method"] != nil && msg["id"] != nil { // Deny every server-requested tool/approval, even for a broken fixture.
			data, _ := json.Marshal(map[string]any{"id": json.RawMessage(msg["id"]), "error": map[string]any{"code": -32601, "message": "tools disabled in synthetic probe"}})
			_, _ = fmt.Fprintln(input, string(data))
		}
	}
	receive := func() map[string]json.RawMessage {
		select {
		case msg, ok := <-messages:
			if !ok {
				t.Fatal("isolated app-server exited; raw output withheld")
			}
			handle(msg)
			return msg
		case <-ctx.Done():
			t.Fatal("isolated app-server probe timeout; raw output withheld")
		}
		return nil
	}
	rpc := func(method string, params any) json.RawMessage {
		next++
		id := next
		data, _ := json.Marshal(map[string]any{"id": id, "method": method, "params": params})
		if _, err := fmt.Fprintln(input, string(data)); err != nil {
			t.Fatal("isolated RPC write failed")
		}
		for {
			msg := receive()
			var got int
			_ = json.Unmarshal(msg["id"], &got)
			if got == id {
				if msg["error"] != nil {
					var e struct{ Code int }
					_ = json.Unmarshal(msg["error"], &e)
					t.Fatalf("isolated RPC %s rejected code=%d (message withheld)", method, e.Code)
				}
				return msg["result"]
			}
		}
	}
	rpc("initialize", map[string]any{"clientInfo": map[string]string{"name": "codexrouter_synthetic_probe", "version": "1.0.0"}, "capabilities": map[string]bool{"experimentalApi": true}})
	_, _ = fmt.Fprintln(input, `{"method":"initialized","params":{}}`)
	threadID := func(result json.RawMessage) string {
		var r struct{ Thread struct{ ID string } }
		if json.Unmarshal(result, &r) != nil || r.Thread.ID == "" {
			t.Fatal("missing synthetic thread ID")
		}
		return r.Thread.ID
	}
	parent := threadID(rpc("thread/start", map[string]any{"cwd": cmd.Dir, "modelProvider": "openai", "approvalPolicy": "never", "sandbox": "read-only"}))
	turn := func(thread string) {
		result := rpc("turn/start", map[string]any{"threadId": thread, "input": []map[string]string{{"type": "text", "text": "Return synthetic result only. Do not use tools."}}})
		var r struct{ Turn struct{ ID string } }
		if json.Unmarshal(result, &r) != nil || r.Turn.ID == "" {
			t.Fatal("missing synthetic turn ID")
		}
		for completed[r.Turn.ID] == "" {
			receive()
		}
		if completed[r.Turn.ID] != "completed" {
			t.Fatal("isolated turn failed; raw message withheld")
		}
	}
	turn(parent)
	config = router.state.config()
	config.DefaultProviderID = BuiltinCompanyID
	if err := router.state.saveConfig(config); err != nil {
		t.Fatal(err)
	}
	child := threadID(rpc("thread/fork", map[string]any{"threadId": parent, "cwd": cmd.Dir, "approvalPolicy": "never", "sandbox": "read-only"}))
	if child == parent {
		t.Fatal("fork did not create an independent thread")
	}
	turn(child)
	turn(child)
	ephemeral := threadID(rpc("thread/fork", map[string]any{"threadId": parent, "cwd": cmd.Dir, "approvalPolicy": "never", "sandbox": "read-only", "ephemeral": true, "excludeTurns": true}))
	if ephemeral == parent || ephemeral == child {
		t.Fatal("ephemeral fork reused an existing thread")
	}
	turn(ephemeral)
	turn(ephemeral)
	// Recreate routing state between turns: the synthetic app-server stays independent.
	stateMu.Lock()
	reloaded, err := loadState(router.state.store)
	if err != nil {
		stateMu.Unlock()
		t.Fatal(err)
	}
	router.state = reloaded
	stateMu.Unlock()
	turn(ephemeral)
	turn(parent)
	other := threadID(rpc("thread/start", map[string]any{"cwd": cmd.Dir, "modelProvider": "openai", "approvalPolicy": "never", "sandbox": "read-only"}))
	turn(other)
	for _, id := range []string{parent, child, ephemeral} {
		if route, err := router.state.resolve(id); err != nil || route != BuiltinOfficialID {
			t.Fatal("fork changed provider after default switch or reload", err)
		}
	}
	if route, err := router.state.resolve(other); err != nil || route != BuiltinCompanyID {
		t.Fatal("independent new thread ignored default", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(shapes) != 8 || seq != 8 || officialCalls != 7 || companyCalls != 1 {
		t.Fatal("missing synthetic requests")
	}
	for _, s := range shapes {
		label := "unknown"
		if s.Thread == parent {
			label = "parent"
		}
		if s.Thread == child {
			label = "child"
		}
		if s.Thread == ephemeral {
			label = "ephemeral-child"
		}
		if s.Thread == other {
			label = "independent-new"
		}
		history := s.History
		if !map[string]bool{"new": true, "cold_fork": true, "warm_fork": true, "cold_resume": true, "ephemeral_fork": true}[history] {
			history = "other"
		}
		t.Logf("thread=%s session_eq_thread=%t session_eq_parent=%t metadata_thread_eq_header=%t metadata_session_eq_header=%t explicit_parent_eq_parent=%t history=%s", label, s.Session == s.Thread, s.Session == parent, s.MetaThread == s.Thread, s.MetaSession == s.Session, s.Parent == parent, history)
		if label == "unknown" || s.MetaThread != s.Thread || s.MetaSession != s.Thread {
			t.Fatal("synthetic identity evidence inconsistent")
		}
		if label == "ephemeral-child" {
			if s.Session != parent || s.Parent != parent || history != "warm_fork" {
				t.Fatal("expected desktop temporary side-chat identity changed")
			}
		} else if s.Session != s.Thread {
			t.Fatal("unexpected session header split")
		}
		if label == "child" && !strings.Contains(history, "fork") {
			t.Fatal("child history marker unproven")
		}
	}
}
