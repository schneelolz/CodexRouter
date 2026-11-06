package router

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"codexrouter/internal/api"
)

func TestApplyModelOverridePreservesUnknownFields(t *testing.T) {
	raw := []byte("{\n  \"model\": \"client-model\",\n  \"stream\": true,\n  \"input\": [{\"role\":\"user\",\"content\":\"合成<内容>&\u2028\"}],\n  \"future_field\": {\"nested\": [1, 2.5, null]},\n  \"metadata\": {\"keep\": \"值\"}\n}")
	out, err := applyModelOverride(raw, nil, "gpt-override")
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]json.RawMessage
	if err = json.Unmarshal(out, &decoded); err != nil {
		t.Fatal(err)
	}
	if string(decoded["model"]) != `"gpt-override"` {
		t.Fatalf("模型字段未替换: %s", decoded["model"])
	}
	for name, want := range map[string]string{
		"stream":       `true`,
		"input":        `[{"role":"user","content":"合成<内容>&\u2028"}]`,
		"future_field": `{"nested":[1,2.5,null]}`,
		"metadata":     `{"keep":"值"}`,
	} {
		if string(decoded[name]) != want {
			t.Fatalf("未知字段或值被改写: %s=%s", name, decoded[name])
		}
	}
	// 不做 HTML 转义，避免改写字符串内容。
	if !bytes.Contains(out, []byte("<内容>&")) {
		t.Fatal("重写后字符串被 HTML 转义")
	}
	if !bytes.Contains(out, []byte(`"type"`)) && false {
		t.Fatal("unreachable")
	}
}

func TestApplyModelOverrideHandlesCompressionAndFailures(t *testing.T) {
	fresh := []byte(`{"model":"client-model","stream":true,"input":[{"role":"user","content":"合成"}]}`)
	compressed := compressedFixture(t, fresh)
	out, err := applyModelOverride(compressed, []string{" zstd "}, "gpt-override")
	if err != nil || !bytes.Contains(out, []byte(`"model":"gpt-override"`)) || !bytes.Contains(out, []byte(`"stream":true`)) {
		t.Fatalf("压缩正文未正确重写: %v %s", err, out)
	}
	for _, test := range []struct {
		name string
		raw  []byte
		enc  []string
		want error
	}{
		{"非对象", []byte(`[{"model":"client"}]`), nil, errModelInvalidBody},
		{"非 JSON", []byte(`model=client`), nil, errModelInvalidBody},
		{"空正文", nil, nil, errModelInvalidBody},
		{"不支持编码", fresh, []string{"gzip"}, errModelEncodingUnsupported},
		{"损坏压缩", compressed[:len(compressed)-3], []string{"zstd"}, errModelEncodingUnsupported},
		{"叠加编码", compressed, []string{"zstd", "identity"}, errModelEncodingUnsupported},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := applyModelOverride(test.raw, test.enc, "gpt-override"); !errors.Is(err, test.want) {
				t.Fatalf("错误分类不符: %v", err)
			}
		})
	}
	if _, err := applyModelOverride(make([]byte, maxEvidenceBody+1), nil, "gpt-override"); !errors.Is(err, errModelBodyTooLarge) {
		t.Fatal("超限正文未拒绝")
	}
}

// 供应商模型覆盖必须实际进入上游正文，而不是只保存配置。
func TestProviderModelOverrideRewritesForwardedModel(t *testing.T) {
	var calls atomic.Int64
	type captured struct {
		model   string
		unknown string
		enc     string
		length  int64
	}
	requests := make(chan captured, 4)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		body, _ := io.ReadAll(r.Body)
		var fields map[string]json.RawMessage
		_ = json.Unmarshal(body, &fields)
		requests <- captured{model: string(fields["model"]), unknown: string(fields["unknown"]), enc: r.Header.Get("Content-Encoding"), length: r.ContentLength}
		w.WriteHeader(http.StatusOK)
	}))
	defer up.Close()
	s, host := liveRouter(t, up.URL)
	if _, err := s.state.updateProvider(BuiltinOfficialID, api.ProviderUpdateInput{Name: "官方线路", BaseURL: up.URL, Model: "gpt-server-side"}); err != nil {
		t.Fatal(err)
	}
	body := `{"model":"client-model","stream":true,"input":[{"role":"user","content":"合成"}],"unknown":{"preserve":true}}`
	r := inferenceRequest(t, host.URL, "override-session", "new")
	r.Body = io.NopCloser(strings.NewReader(body))
	r.ContentLength = int64(len(body))
	r.Header.Set("Content-Encoding", "identity")
	r.Header.Set("Authorization", "Bearer synthetic-official")
	resp, err := host.Client().Do(r)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("模型覆盖请求失败: %d", resp.StatusCode)
	}
	got := <-requests
	if got.model != `"gpt-server-side"` || got.unknown != `{"preserve":true}` || got.enc != "" || got.length <= 0 {
		t.Fatalf("上游未收到重写后的正文: %+v", got)
	}
	// 已绑定会话保持原供应商；切换默认值不改变原模型覆盖。
	if _, err := s.state.updateProvider(BuiltinCompanyID, api.ProviderUpdateInput{Name: "公司线路", BaseURL: up.URL, Model: "gpt-company-side"}); err != nil {
		t.Fatal(err)
	}
	cfg := s.state.config()
	cfg.DefaultProviderID = BuiltinCompanyID
	if err := s.state.saveConfig(cfg); err != nil {
		t.Fatal(err)
	}
	r = inferenceRequest(t, host.URL, "override-session", "new")
	r.Body = io.NopCloser(strings.NewReader(body))
	r.ContentLength = int64(len(body))
	resp, err = host.Client().Do(r)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	got = <-requests
	if got.model != `"gpt-server-side"` || calls.Load() != 2 {
		t.Fatalf("已绑定会话被切换供应商或模型: %+v calls=%d", got, calls.Load())
	}
}

func TestProviderModelOverrideFailuresDoNotReachUpstream(t *testing.T) {
	var calls atomic.Int64
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1) }))
	defer up.Close()
	s, host := liveRouter(t, up.URL)
	if _, err := s.state.updateProvider(BuiltinOfficialID, api.ProviderUpdateInput{Name: "官方线路", BaseURL: up.URL, Model: "gpt-server-side"}); err != nil {
		t.Fatal(err)
	}
	// 先建立绑定，确保后续失败发生在模型覆盖阶段而不是首次证据判定。
	bind := inferenceRequest(t, host.URL, "override-failure", "new")
	bindBody := `{"model":"client-model","input":[{"role":"user","content":"合成"}]}`
	bind.Body = io.NopCloser(strings.NewReader(bindBody))
	bind.ContentLength = int64(len(bindBody))
	bindResp, err := host.Client().Do(bind)
	if err != nil {
		t.Fatal(err)
	}
	bindResp.Body.Close()
	if bindResp.StatusCode != http.StatusOK || calls.Load() != 1 {
		t.Fatalf("绑定前置请求失败: %d calls=%d", bindResp.StatusCode, calls.Load())
	}
	boundCalls := calls.Load()
	for _, test := range []struct {
		name   string
		body   string
		enc    []string
		status int
		code   string
	}{
		{"非 JSON 正文", `not-json`, nil, http.StatusBadRequest, "MODEL_OVERRIDE_INVALID_BODY"},
		{"未知编码", `{"model":"client"}`, []string{"br"}, http.StatusBadRequest, "MODEL_OVERRIDE_ENCODING_UNSUPPORTED"},
		{"超过上限", strings.Repeat("a", maxEvidenceBody+1), nil, http.StatusRequestEntityTooLarge, "REQUEST_BODY_INVALID"},
	} {
		t.Run(test.name, func(t *testing.T) {
			r := inferenceRequest(t, host.URL, "override-failure", "")
			r.Body = io.NopCloser(strings.NewReader(test.body))
			r.ContentLength = int64(len(test.body))
			r.Header["Content-Encoding"] = test.enc
			resp, err := host.Client().Do(r)
			if err != nil {
				t.Fatal(err)
			}
			data, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode != test.status || !bytes.Contains(data, []byte(test.code)) {
				t.Fatalf("模型覆盖失败处理不符: %d %s", resp.StatusCode, data)
			}
		})
	}
	if calls.Load() != boundCalls {
		t.Fatal("模型覆盖失败仍向上游转发")
	}
}

func TestModelOverrideUsesProviderCredentialAndIsolatesIdentity(t *testing.T) {
	var calls atomic.Int64
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Header.Get("Authorization") != "Bearer synthetic-company" || r.Header.Get("Chatgpt-Account-Id") != "" || r.Header.Get("Thread-Id") != "" {
			t.Error("模型覆盖请求泄露官方身份或未用供应商凭据")
		}
		body, _ := io.ReadAll(r.Body)
		if !bytes.Contains(body, []byte(`"model":"gpt-company"`)) {
			t.Errorf("公司供应商模型覆盖未生效: %s", body)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer up.Close()
	s, host := liveRouter(t, up.URL)
	if err := s.creds.Set(BuiltinCompanyID, "synthetic-company"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.state.updateProvider(BuiltinCompanyID, api.ProviderUpdateInput{Name: "公司线路", BaseURL: up.URL, Model: "gpt-company"}); err != nil {
		t.Fatal(err)
	}
	cfg := s.state.config()
	cfg.DefaultProviderID = BuiltinCompanyID
	if err := s.state.saveConfig(cfg); err != nil {
		t.Fatal(err)
	}
	r := inferenceRequest(t, host.URL, "company-override", "new")
	r.Header.Set("Authorization", "Bearer synthetic-official")
	r.Header.Set("Chatgpt-Account-Id", "synthetic-account")
	resp, err := host.Client().Do(r)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || calls.Load() != 1 {
		t.Fatalf("公司模型覆盖请求失败: %d calls=%d", resp.StatusCode, calls.Load())
	}
}
