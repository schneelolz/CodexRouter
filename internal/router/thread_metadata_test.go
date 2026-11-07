package router

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"syscall"
	"testing"
)

func TestThreadMetadataRejectsAmbiguousSemanticSources(t *testing.T) {
	for _, raw := range []string{
		`null`, `[]`, `"metadata"`, `{} {}`,
		`{"thread_id":null}`, `{"session_id":false}`, `{"thread_id":""}`,
		`{"history_initialization":null}`, `{"forked_from_thread_id":42}`,
		`{"thread_id":"other","thread_id":"child"}`,
		`{"session_id":"child","session_id":"child"}`,
		`{"history_initialization":"cold_fork","history_initialization":"new"}`,
		`{"forked_from_thread_id":"parent","forked_from_thread_id":"other"}`,
		`{"Thread_ID":"child"}`,
	} {
		r := httptest.NewRequest(http.MethodPost, "/responses", nil)
		r.Header.Set("Thread-Id", "child")
		r.Header.Set("X-Codex-Turn-Metadata", raw)
		if _, err := identity(r); err == nil || err.Error() != "SESSION_METADATA_INVALID" {
			t.Fatalf("歧义元数据未拒绝: %s error=%v", raw, err)
		}
	}
	for _, raw := range []string{
		`{"thread_id":"child","session_id":"other"}`,
		`{"thread_id":"child","forked_from_thread_id":"child"}`,
		`{"thread_id":"child","forked_from_thread_id":"bad,parent"}`,
	} {
		r := httptest.NewRequest(http.MethodPost, "/responses", nil)
		r.Header.Set("Thread-Id", "child")
		r.Header.Set("Session-Id", "child")
		r.Header.Set("X-Codex-Turn-Metadata", raw)
		if _, err := identity(r); err == nil || err.Error() != "SESSION_ID_CONFLICT" {
			t.Fatal("父子或同语义身份冲突未拒绝")
		}
	}
	r := httptest.NewRequest(http.MethodPost, "/responses", nil)
	r.Header.Set("Thread-Id", "child")
	r.Header.Set("Session-Id", "child")
	r.Header.Set("X-Codex-Turn-Metadata", `{"history_initialization":"cold_fork","forked_from_thread_id":"parent","future":{"thread_id":null}}`)
	e, err := identity(r)
	if err != nil || e.ThreadID != "child" || e.SessionID != "child" || e.Parent != "parent" || e.History != "cold_fork" {
		t.Fatalf("头部或可选元数据未保留: %+v %v", e, err)
	}
}

func TestDesktopWarmForkParentSessionHeader(t *testing.T) {
	request := func(thread, session, metadata string) *http.Request {
		r := httptest.NewRequest(http.MethodPost, "/responses", nil)
		r.Header.Set("Thread-Id", thread)
		r.Header.Set("Session-Id", session)
		r.Header.Set("X-Codex-Turn-Metadata", metadata)
		return r
	}
	raw := `{"thread_id":"child","session_id":"child","history_initialization":"warm_fork","forked_from_thread_id":"parent"}`
	evidence, err := identity(request("child", "parent", raw))
	if err != nil || evidence.ThreadID != "child" || evidence.SessionID != "child" || evidence.Parent != "parent" {
		t.Fatal("observed desktop warm fork rejected", err)
	}
	s, p := testState(t)
	if _, err := s.resolveEvidence(threadEvidence{ThreadID: "parent", History: "new"}); err != nil {
		t.Fatal(err)
	}
	setProviderURL(t, s, BuiltinCompanyID, "http://127.0.0.1:19999")
	cfg := s.config()
	cfg.DefaultProviderID = BuiltinCompanyID
	if err := s.saveConfig(cfg); err != nil {
		t.Fatal(err)
	}
	p.setWriteError(errors.New("synthetic write failure"))
	if provider, err := s.resolveEvidence(evidence); provider != "" || err == nil || err.Error() != "STORAGE_WRITE_FAILED" {
		t.Fatal("child inherited before persistence")
	}
	p.setWriteError(nil)
	if provider, err := s.resolveEvidence(evidence); err != nil || provider != BuiltinOfficialID {
		t.Fatal("child failed to inherit explicit parent", err)
	}
	reloaded, err := loadState(p)
	if err != nil {
		t.Fatal(err)
	}
	if provider, err := reloaded.resolveEvidence(evidence); err != nil || provider != BuiltinOfficialID {
		t.Fatal("reloaded child changed route", err)
	}
	for _, binding := range reloaded.sessions().Bindings {
		if binding.ProviderID != BuiltinOfficialID {
			t.Fatal("session root or default selected")
		}
	}
	unknown, _ := testState(t)
	if provider, err := unknown.resolveEvidence(evidence); provider != "" || err == nil || err.Error() != "SESSION_REGISTRATION_REQUIRED" {
		t.Fatal("unknown parent used default")
	}
	for _, bad := range []struct{ session, metadata string }{
		{"other", raw},
		{"parent", strings.Replace(raw, `"warm_fork"`, `"new"`, 1)},
		{"parent", strings.Replace(raw, `"warm_fork"`, `"cold_fork"`, 1)},
		{"parent", strings.Replace(raw, `"warm_fork"`, `"ephemeral_fork"`, 1)},
		{"parent", strings.Replace(raw, `"session_id":"child"`, `"session_id":"other"`, 1)},
		{"parent", strings.Replace(raw, `"thread_id":"child"`, `"thread_id":"parent"`, 1)},
		{"parent", `{"thread_id":"child","session_id":"child","history_initialization":"warm_fork"}`},
		{"parent", `{"session_id":"child","history_initialization":"warm_fork","forked_from_thread_id":"parent"}`},
		{"parent", strings.Replace(raw, `"forked_from_thread_id":"parent"`, `"forked_from_thread_id":"child"`, 1)},
	} {
		if _, err := identity(request("child", bad.session, bad.metadata)); err == nil {
			t.Fatal("unproven identity combination accepted")
		}
	}
	duplicate := request("child", "parent", raw)
	duplicate.Header.Add("Session-Id", "parent")
	if _, err := identity(duplicate); err == nil {
		t.Fatal("duplicate session header accepted")
	}
}

func TestUnobservedForkKindsCannotUseKnownParentOrSessionRoot(t *testing.T) {
	s, _ := testState(t)
	if _, err := s.resolveEvidence(threadEvidence{ThreadID: "parent", History: "new"}); err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"ephemeral_fork", "future_fork", "cold_resume"} {
		provider, err := s.resolveEvidence(threadEvidence{ThreadID: kind, SessionID: "parent", History: kind, Parent: "parent"})
		if provider != "" || err == nil || err.Error() != "SESSION_REGISTRATION_REQUIRED" {
			t.Fatalf("未实测类型通过父线程或会话树根自动绑定: %s %v", kind, err)
		}
	}
	if _, err := s.resolveEvidence(threadEvidence{ThreadID: "self", Parent: "self", History: "cold_fork"}); err == nil {
		t.Fatal("父线程与子线程相同未拒绝")
	}
}

func TestKnownForkPersistsChildOnlyOnceAndRetainsProviderAfterRestart(t *testing.T) {
	for _, kind := range []string{"cold_fork", "warm_fork"} {
		t.Run(kind, func(t *testing.T) {
			s, p := testState(t)
			if _, err := s.resolveEvidence(threadEvidence{ThreadID: "parent", History: "new"}); err != nil {
				t.Fatal(err)
			}
			setProviderURL(t, s, BuiltinCompanyID, "http://127.0.0.1:19999")
			cfg := s.config()
			cfg.DefaultProviderID = BuiltinCompanyID
			if err := s.saveConfig(cfg); err != nil {
				t.Fatal(err)
			}
			e := threadEvidence{ThreadID: "child", SessionID: "unbound-tree-root", Parent: "parent", History: kind}
			p.setWriteError(errors.New("synthetic fork write failure"))
			if provider, err := s.resolveEvidence(e); provider != "" || err == nil || err.Error() != "STORAGE_WRITE_FAILED" {
				t.Fatal("fork 未持久化仍获准转发")
			}
			if len(s.sessions().Bindings) != 1 {
				t.Fatal("fork 落盘失败发布了内存绑定")
			}
			p.setWriteError(nil)
			_, before := p.snapshot()
			var wg sync.WaitGroup
			for range 32 {
				wg.Add(1)
				go func() {
					defer wg.Done()
					if provider, err := s.resolveEvidence(e); err != nil || provider != BuiltinOfficialID {
						t.Error("fork 未保持父供应商")
					}
				}()
			}
			wg.Wait()
			_, after := p.snapshot()
			if after-before != 1 || len(s.sessions().Bindings) != 2 {
				t.Fatal("子线程重复持久化或会话树根被绑定")
			}
			restarted, err := loadState(p)
			if err != nil {
				t.Fatal(err)
			}
			for _, evidence := range []threadEvidence{
				{ThreadID: "child", History: "new"},
				{ThreadID: "child", History: "cold_fork", Parent: "unknown-parent"},
				{ThreadID: "child", History: "cold_resume"},
			} {
				if provider, err := restarted.resolveEvidence(evidence); err != nil || provider != BuiltinOfficialID {
					t.Fatal("重启或后续标记改变子线程供应商")
				}
			}
		})
	}
}

func TestWarmForkStillRequiresExplicitBoundParent(t *testing.T) {
	s, _ := testState(t)
	if _, err := s.resolveEvidence(threadEvidence{ThreadID: "tree-root", History: "new"}); err != nil {
		t.Fatal(err)
	}
	for _, e := range []threadEvidence{
		{ThreadID: "warm-no-parent", SessionID: "tree-root", History: "warm_fork"},
		{ThreadID: "warm-unknown-parent", SessionID: "tree-root", Parent: "unknown", History: "warm_fork"},
	} {
		provider, err := s.resolveEvidence(e)
		if provider != "" || err == nil || err.Error() != "SESSION_REGISTRATION_REQUIRED" {
			t.Fatal("warm fork 未知父来源误用了会话树根或默认供应商")
		}
	}
}

func TestNetworkFailureHasSafeActionableClassification(t *testing.T) {
	for _, tc := range []struct {
		err  error
		code string
	}{
		{context.Canceled, "REQUEST_CANCELED"},
		{&net.DNSError{IsNotFound: true}, "UPSTREAM_DNS_FAILED"},
		{&tls.CertificateVerificationError{Err: x509.UnknownAuthorityError{}}, "UPSTREAM_TLS_FAILED"},
		{context.DeadlineExceeded, "UPSTREAM_TIMEOUT"},
		{io.EOF, "UPSTREAM_CONNECTION_CLOSED"},
		{io.ErrUnexpectedEOF, "UPSTREAM_CONNECTION_CLOSED"},
		{syscall.ECONNRESET, "UPSTREAM_CONNECTION_CLOSED"},
		{errors.New("synthetic secret"), "UPSTREAM_UNREACHABLE"},
	} {
		wrapped := &url.Error{Op: "Post", URL: "https://synthetic.invalid/private", Err: tc.err}
		code := networkCode(wrapped)
		message := networkMessage(code)
		if code != tc.code || !strings.Contains(message, code) || strings.Contains(message, "synthetic") || strings.Contains(message, "https://") {
			t.Fatalf("网络错误分类或脱敏失败: %s", code)
		}
	}
}

func TestStreamDiagnosticsSeparateCancellationFromUpstreamFailure(t *testing.T) {
	request, cancelRequest := context.WithCancel(context.Background())
	server, cancelServer := context.WithCancel(context.Background())
	defer cancelRequest()
	defer cancelServer()
	if got := streamFailureCode(request, server, "STREAM_INTERRUPTED"); got != "STREAM_INTERRUPTED" {
		t.Fatal("上游断流误归类为取消")
	}
	if got := streamFailureCode(request, server, "STREAM_IDLE_TIMEOUT"); got != "STREAM_IDLE_TIMEOUT" {
		t.Fatal("上游空闲超时未保留")
	}
	cancelServer()
	if got := streamFailureCode(request, server, "STREAM_INTERRUPTED"); got != "ROUTER_SHUTDOWN" {
		t.Fatal("路由器关闭误归类为上游断流")
	}
	cancelRequest()
	if got := streamFailureCode(request, server, "CLIENT_WRITE_FAILED"); got != "REQUEST_CANCELED" {
		t.Fatal("客户端取消误归类为写入故障")
	}
}

func TestProtocolHeaderCopyPreservesValuesWithoutHopOrUnknownHeaders(t *testing.T) {
	src := http.Header{}
	src.Add("X-Codex-Turn-State", "state-one")
	src.Add("X-Codex-Turn-State", "state-two")
	src.Set("Content-Type", "application/json")
	src.Set("Connection", " keep-alive , CONTENT-TYPE")
	src.Set("Cookie", "synthetic")
	dst := http.Header{}
	copyProtocolHeaders(dst, src, "Content-Type", "X-Codex-Turn-State")
	if dst.Get("Content-Type") != "" || dst.Get("Cookie") != "" || len(dst.Values("X-Codex-Turn-State")) != 2 {
		t.Fatal("逐跳头或未知头泄露，或多值字段丢失")
	}
}
