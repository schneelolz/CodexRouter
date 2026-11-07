package router

import (
	"bufio"
	"bytes"
	"codexrouter/internal/clients"
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
	"sync/atomic"
	"testing"
	"time"
)

func TestIsolatedCodexCLIRoutesNewResumeForkAndRestart(t *testing.T) {
	if os.Getenv("CODEXROUTER_CODEX_INTEGRATION") != "1" {
		t.Skip("设置 CODEXROUTER_CODEX_INTEGRATION=1 才执行隔离 Codex CLI 集成测试")
	}
	exe := os.Getenv("CODEX_PROBE_EXE")
	if exe == "" {
		var err error
		exe, err = exec.LookPath("codex")
		if err != nil {
			t.Fatal("隔离集成测试需要本机 Codex CLI")
		}
	}
	var resolveErr error
	exe, resolveErr = clients.ProbeExecutable(exe)
	if resolveErr != nil {
		t.Fatal(resolveErr)
	}
	root := t.TempDir()
	home, work := filepath.Join(root, "codex-home"), filepath.Join(root, "work")
	for _, dir := range []string{home, work, filepath.Join(root, "home")} {
		if err := os.Mkdir(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	isolatedEnv := clients.Environment(os.Environ(), root, "")
	versionCtx, versionCancel := context.WithTimeout(context.Background(), 5*time.Second)
	versionCmd := exec.CommandContext(versionCtx, exe, "--version")
	versionCmd.Env, versionCmd.Dir = isolatedEnv, work
	version, err := versionCmd.Output()
	versionCancel()
	if err != nil || !strings.Contains(string(version), "0.166.0") {
		t.Fatal("此协议集成测试仅验收 Codex CLI 0.166.0")
	}
	const officialKey = "synthetic-codex-integration-official"
	auth, _ := json.Marshal(map[string]string{"OPENAI_API_KEY": officialKey})
	if err := os.WriteFile(filepath.Join(home, "auth.json"), auth, 0600); err != nil {
		t.Fatal(err)
	}
	var officialCalls, companyCalls, responseID atomic.Int64
	var leakedIdentity atomic.Bool
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, io.LimitReader(r.Body, 16<<20))
		if r.Method == http.MethodGet && r.URL.Path == "/models" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"models":[]}`)
			return
		}
		if r.Method != http.MethodPost || r.URL.Path != "/responses" {
			http.NotFound(w, r)
			return
		}
		switch r.Header.Get("Authorization") {
		case "Bearer synthetic-company":
			companyCalls.Add(1)
			for _, name := range []string{"Cookie", "Chatgpt-Account-Id", "Openai-Organization", "Openai-Project", "Thread-Id", "Session-Id", "X-Codex-Turn-Metadata", "Originator", "X-Codex-Turn-State", "Version"} {
				if r.Header.Get(name) != "" {
					leakedIdentity.Store(true)
				}
			}
		case "Bearer " + officialKey:
			officialCalls.Add(1)
		default:
			leakedIdentity.Store(true)
			http.Error(w, "synthetic authentication mismatch", http.StatusUnauthorized)
			return
		}
		writeSyntheticCodexSSE(w, responseID.Add(1))
	}))
	defer upstream.Close()
	s, host := liveRouter(t, upstream.URL)
	var shapeMu sync.Mutex
	var lastShape []string
	host.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/responses" {
			body, _ := io.ReadAll(io.LimitReader(r.Body, (16<<20)+1))
			r.Body.Close()
			r.Body = io.NopCloser(bytes.NewReader(body))
			var request struct {
				Input []struct {
					Type string          `json:"type"`
					Role string          `json:"role"`
					ID   json.RawMessage `json:"id"`
				} `json:"input"`
				Previous json.RawMessage `json:"previous_response_id"`
			}
			_ = json.Unmarshal(body, &request)
			shape := []string{fmt.Sprintf("fresh=%t previous_present=%t", hasFreshInput(body), len(request.Previous) > 0)}
			for _, item := range request.Input {
				shape = append(shape, fmt.Sprintf("type=%s role=%s id_present=%t", item.Type, item.Role, len(item.ID) > 0))
			}
			shapeMu.Lock()
			lastShape = shape
			shapeMu.Unlock()
		}
		s.ServeHTTP(w, r)
	})
	config := s.state.config()
	config.ConnectTimeoutMS, config.HeaderTimeoutMS = 5000, 5000
	config.IdleTimeoutMS, config.WriteTimeoutMS = 5000, 5000
	if err := s.state.saveConfig(config); err != nil {
		t.Fatal(err)
	}
	if err := s.creds.Set("company", "synthetic-company"); err != nil {
		t.Fatal(err)
	}
	env := make([]string, 0, len(isolatedEnv)+3)
	for _, entry := range isolatedEnv {
		key, _, _ := strings.Cut(entry, "=")
		upper := strings.ToUpper(key)
		if strings.HasPrefix(upper, "OPENAI_") || strings.HasPrefix(upper, "CODEX_") || strings.HasPrefix(upper, "CHATGPT_") || upper == "HTTP_PROXY" || upper == "HTTPS_PROXY" || upper == "ALL_PROXY" || upper == "NO_PROXY" {
			continue
		}
		env = append(env, entry)
	}
	env = append(env, "CODEX_HOME="+home, "OPENAI_API_KEY="+officialKey, "OTEL_SDK_DISABLED=true")
	run := func(label string, action []string, want string) string {
		t.Helper()
		beforeOfficial, beforeCompany := officialCalls.Load(), companyCalls.Load()
		args := append([]string{"--no-daemon", "--sandbox", "read-only", "exec"}, action...)
		args = append(args, "--skip-git-repo-check", "--ignore-user-config", "--ignore-rules", "--json", "-c", "features.shell_tool=false", "-c", "features.unified_exec=false", "-c", "features.code_mode=false", "-c", "features.apply_patch_freeform=false", "-c", `model_provider="openai"`, "-c", `openai_base_url="`+host.URL+`"`, "-c", `chatgpt_base_url="`+host.URL+`"`, "-c", `web_search="disabled"`, "-c", "analytics.enabled=false", "-c", "feedback.enabled=false", "Return only the synthetic probe result. Do not use tools.")
		ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, exe, args...)
		cmd.Dir, cmd.Env, cmd.Stderr = work, env, io.Discard
		output, err := cmd.Output()
		thread, completed := "", false
		scanner := bufio.NewScanner(bytes.NewReader(output))
		scanner.Buffer(make([]byte, 4096), 1<<20)
		for scanner.Scan() {
			var event struct {
				Type     string `json:"type"`
				ThreadID string `json:"thread_id"`
			}
			if json.Unmarshal(scanner.Bytes(), &event) != nil {
				continue
			}
			if event.Type == "thread.started" {
				thread = event.ThreadID
			}
			completed = completed || event.Type == "turn.completed"
		}
		official, company := officialCalls.Load()-beforeOfficial, companyCalls.Load()-beforeCompany
		if err != nil || ctx.Err() != nil || scanner.Err() != nil || thread == "" || !completed {
			codes := make([]string, 0)
			s.mu.Lock()
			for _, diagnostic := range s.diagnostics {
				codes = append(codes, diagnostic.Code)
			}
			s.mu.Unlock()
			shapeMu.Lock()
			shape := append([]string{}, lastShape...)
			shapeMu.Unlock()
			t.Fatalf("隔离 CLI 阶段 %s 失败：exit_success=%t timeout=%t completed=%t official_calls=%d company_calls=%d diagnostic_codes=%v pending=%d input_shape=%v（不输出原始日志）", label, err == nil, ctx.Err() != nil, completed, official, company, codes, len(s.state.sessions().Pending), shape)
		}
		if (want == BuiltinOfficialID && (official < 1 || company != 0)) || (want == BuiltinCompanyID && (company < 1 || official != 0)) {
			t.Fatalf("隔离 CLI 阶段 %s 路由错误：official_calls=%d company_calls=%d", label, official, company)
		}
		if leakedIdentity.Load() {
			t.Fatal("隔离 CLI 上游出现凭据或身份头隔离失败")
		}
		// 每阶段计数是客户端实际请求数量，包含客户端预热或重试，不据此归因路由器重放。
		t.Logf("阶段=%s completed=true official_physical_calls=%d company_physical_calls=%d", label, official, company)
		return thread
	}
	original := run("official-new", nil, BuiltinOfficialID)
	config = s.state.config()
	config.DefaultProviderID = BuiltinCompanyID
	if err := s.state.saveConfig(config); err != nil {
		t.Fatal(err)
	}
	if resumed := run("official-resume-after-default-switch", []string{"resume", original}, BuiltinOfficialID); resumed != original {
		t.Fatal("CLI resume 改变原线程标识")
	}
	company := run("company-new", nil, BuiltinCompanyID)
	if company == original {
		t.Fatal("CLI 独立新线程复用了原线程标识")
	}
	child := run("official-fork-known-parent", []string{"fork", original}, BuiltinOfficialID)
	if child == original || child == company {
		t.Fatal("CLI fork 未创建独立线程标识")
	}
	reloaded, err := loadState(s.state.store)
	if err != nil {
		t.Fatal(err)
	}
	// 前一 CLI 已退出且所有请求完成，在同地址恢复持久状态以独立验证重启识别。
	s.state = reloaded
	if resumed := run("official-resume-after-state-reload", []string{"resume", original}, BuiltinOfficialID); resumed != original {
		t.Fatal("状态重载后 CLI resume 改变原线程标识")
	}
	bindings := s.state.sessions().Bindings
	if len(bindings) != 3 {
		t.Fatalf("隔离 CLI 应形成三个固定绑定，实际 %d", len(bindings))
	}
	for _, binding := range bindings {
		want := BuiltinOfficialID
		if binding.ID == company {
			want = BuiltinCompanyID
		}
		if binding.ProviderID != want {
			t.Fatal("CLI 集成的持久绑定线路错误")
		}
	}
	if officialCalls.Load() < 4 || companyCalls.Load() < 1 {
		t.Fatal("隔离 CLI 实际上游调用不足，未覆盖要求的生命周期")
	}
}

func writeSyntheticCodexSSE(w http.ResponseWriter, sequence int64) {
	id := fmt.Sprintf("resp_synthetic_integration_%d", sequence)
	item := map[string]any{"id": fmt.Sprintf("msg_%d", sequence), "type": "message", "role": "assistant", "status": "completed", "content": []any{map[string]any{"type": "output_text", "text": "synthetic probe complete", "annotations": []any{}}}}
	response := map[string]any{"id": id, "object": "response", "created_at": time.Now().Unix(), "status": "completed", "model": "gpt-5.4", "output": []any{item}, "usage": map[string]any{"input_tokens": 1, "output_tokens": 1, "total_tokens": 2, "input_tokens_details": map[string]any{"cached_tokens": 0}, "output_tokens_details": map[string]any{"reasoning_tokens": 0}}}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	eventSequence := 0
	event := func(kind string, fields map[string]any) {
		fields["type"], fields["sequence_number"] = kind, eventSequence
		eventSequence++
		data, _ := json.Marshal(fields)
		_, _ = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", kind, data)
		w.(http.Flusher).Flush()
	}
	created := make(map[string]any, len(response))
	for key, value := range response {
		created[key] = value
	}
	created["status"], created["output"] = "in_progress", []any{}
	event("response.created", map[string]any{"response": created})
	event("response.output_item.added", map[string]any{"output_index": 0, "item": map[string]any{"id": item["id"], "type": "message", "role": "assistant", "status": "in_progress", "content": []any{}}})
	part := map[string]any{"type": "output_text", "text": "", "annotations": []any{}}
	event("response.content_part.added", map[string]any{"item_id": item["id"], "output_index": 0, "content_index": 0, "part": part})
	event("response.output_text.delta", map[string]any{"item_id": item["id"], "output_index": 0, "content_index": 0, "delta": "synthetic probe complete"})
	event("response.output_text.done", map[string]any{"item_id": item["id"], "output_index": 0, "content_index": 0, "text": "synthetic probe complete"})
	event("response.content_part.done", map[string]any{"item_id": item["id"], "output_index": 0, "content_index": 0, "part": item["content"].([]any)[0]})
	event("response.output_item.done", map[string]any{"output_index": 0, "item": item})
	event("response.completed", map[string]any{"response": response})
}
