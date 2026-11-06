package router

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"codexrouter/internal/api"
	"codexrouter/internal/platform"
)

func relayInput() api.ProviderSaveInput {
	return api.ProviderSaveInput{Name: "测试中转", BaseURL: "https://relay.example.test/v1", Key: "synthetic-relay-key"}
}

func TestRelaySaveAtomicFailuresAndRestart(t *testing.T) {
	server, p, c := newSecurityServer(t, nil)
	s := server.state
	before, _ := p.snapshot()
	p.setWriteError(errors.New("synthetic write failure"))
	if _, err := s.saveRelayProvider(relayInput()); err == nil {
		t.Fatal("未报告写盘失败")
	}
	after, _ := p.snapshot()
	if !bytes.Equal(before, after) || len(s.disk.Providers) != 2 || len(c.values) != 1 {
		t.Fatal("新建失败留下供应商或凭据")
	}
	p.setWriteError(nil)
	created, err := s.saveRelayProvider(relayInput())
	if err != nil || !created.KeyConfigured {
		t.Fatal("统一保存失败")
	}
	savedKey, _ := c.Get(created.ID)
	if savedKey != relayInput().Key {
		t.Fatal("凭据未保存")
	}
	in := relayInput()
	in.ProviderID = created.ID
	in.Name = "编辑名称"
	in.Key = ""
	if _, err = s.saveRelayProvider(in); err != nil {
		t.Fatal("留空保持密钥失败", err)
	}
	in.Key = "synthetic-replacement"
	in.Model = "new-model"
	p.setWriteError(errors.New("synthetic write failure"))
	if _, err = s.saveRelayProvider(in); err == nil {
		t.Fatal("编辑写盘失败未报告")
	}
	if key, _ := c.Get(created.ID); key != savedKey {
		t.Fatal("原密钥未恢复")
	}
	old, _ := s.providerRecord(created.ID)
	if old.Model != "" {
		t.Fatal("失败发布了新模型")
	}
	p.setWriteError(nil)
	reloaded, err := loadState(p)
	if err != nil {
		t.Fatal(err)
	}
	reloaded.creds = c
	if rec, ok := reloaded.providerRecord(created.ID); !ok || rec.Name != "编辑名称" {
		t.Fatal("重启没有保持保存结果")
	}
	if _, err = s.updateProvider(created.ID, api.ProviderUpdateInput{Name: created.Name, BaseURL: "https://another.example.test/v1"}); err == nil || err.Error() != "MODEL_QUERY_KEY_REQUIRED" {
		t.Fatal("旧更新接口允许把已存密钥带到新地址")
	}
	in.Key = ""
	in.BaseURL = "https://another.example.test/v1"
	if _, err = s.saveRelayProvider(in); err == nil || err.Error() != "MODEL_QUERY_KEY_REQUIRED" {
		t.Fatal("改变地址时复用了原密钥")
	}
	in.Key = "synthetic-different-key"
	s.disk.Bindings["bound"] = api.Binding{ID: "bound", ProviderID: created.ID}
	if _, err = s.saveRelayProvider(in); err == nil || err.Error() != "PROVIDER_LOCKED" {
		t.Fatal("已有会话基址可变")
	}
}

func TestRelaySaveCredentialFailures(t *testing.T) {
	s, p, c := newSecurityServer(t, nil)
	c.setErr = errors.New("synthetic secret in backend error")
	if _, err := s.state.saveRelayProvider(relayInput()); err == nil || err.Error() != "CREDENTIAL_UNAVAILABLE" {
		t.Fatal("凭据失败不正确")
	}
	if len(s.state.disk.Providers) != 2 {
		t.Fatal("凭据失败写入了状态")
	}
	c.setErr = nil
	c.deleteErr = errors.New("synthetic delete failure")
	p.setWriteError(errors.New("synthetic write failure"))
	if _, err := s.state.saveRelayProvider(relayInput()); err == nil || err.Error() != "PROVIDER_SAVE_ROLLBACK_FAILED" {
		t.Fatal("补偿失败未显式报告")
	}
}

func TestModelQueryStoredKeyNeverMovesToDraftAddress(t *testing.T) {
	server, _, c := newSecurityServer(t, nil)
	s := server.state
	created, err := s.saveRelayProvider(relayInput())
	if err != nil {
		t.Fatal(err)
	}
	for _, in := range []api.ModelQueryInput{
		{ProviderID: created.ID, BaseURL: "https://another.example.test/v1"},
		{ProviderID: "official", BaseURL: BuiltinOfficialURL},
		{BaseURL: "https://relay.example.test/v1"},
		{ProviderID: created.ID, BaseURL: "https://user:pass@relay.example.test/v1"},
		{ProviderID: created.ID, BaseURL: "http://public.example.test/v1"},
	} {
		if _, err = s.modelQueryKey(in); err == nil {
			t.Fatal("无效或不同地址获取了密钥")
		}
	}
	key, err := s.modelQueryKey(api.ModelQueryInput{ProviderID: created.ID, BaseURL: created.BaseURL + "/"})
	if err != nil || key != relayInput().Key {
		t.Fatal("原地址无法读取密钥")
	}
	if _, err := c.Get("official"); !errors.Is(err, platform.ErrNotFound) {
		t.Fatal("官方身份进入了凭据存储")
	}
}

func TestRelayModelsWhitelistLimitsAndNoRedirect(t *testing.T) {
	var requests atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.Header.Get("Authorization") != "Bearer synthetic-model-key" || r.Header.Get("Cookie") != "" {
			t.Error("请求认证不正确")
		}
		if r.URL.Path != "/root/models" {
			t.Error("基址路径被改写")
		}
		_, _ = w.Write([]byte(`{"data":[{"id":"z-model","secret":"never-expose"},{"id":"a-model"},{"id":"z-model"},{"id":"bad\nmodel"}],"secret":"never-expose"}`))
	}))
	defer upstream.Close()
	result, err := fetchRelayModels(context.Background(), upstream.URL+"/root/", "synthetic-model-key")
	if err != nil || len(result.Items) != 2 || result.Items[0].ID != "a-model" {
		t.Fatal("模型列表未正确去重排序", err)
	}
	serialized, _ := json.Marshal(result)
	if strings.Contains(string(serialized), "never-expose") {
		t.Fatal("泄漏上游其他字段")
	}
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, upstream.URL+"/root/models", 302) }))
	defer redirect.Close()
	if _, err = fetchRelayModels(context.Background(), redirect.URL, "synthetic-model-key"); err == nil || err.Error() != "MODEL_QUERY_REDIRECT" {
		t.Fatal("重定向未拒绝")
	}
	if requests.Load() != 1 {
		t.Fatal("重定向发送了第二次请求")
	}
	for _, tt := range []struct {
		status     int
		body, code string
	}{
		{401, `secret-body`, "MODEL_QUERY_UNAUTHORIZED"}, {404, `secret-body`, "MODEL_QUERY_UNSUPPORTED"}, {500, `secret-body`, "MODEL_QUERY_UPSTREAM_FAILED"},
		{200, `<html>secret-body</html>`, "MODEL_QUERY_INVALID_RESPONSE"}, {200, `{}`, "MODEL_QUERY_INVALID_RESPONSE"}, {200, strings.Repeat("x", (1<<20)+1), "MODEL_QUERY_TOO_LARGE"},
	} {
		t.Run(tt.code, func(t *testing.T) {
			u := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tt.status)
				_, _ = w.Write([]byte(tt.body))
			}))
			defer u.Close()
			if _, e := fetchRelayModels(context.Background(), u.URL, "synthetic-model-key"); e == nil || e.Error() != tt.code {
				t.Fatal("错误未安全分类", e)
			}
		})
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err = fetchRelayModels(ctx, upstream.URL, "synthetic-model-key"); err == nil {
		t.Fatal("取消未生效")
	}
}

func TestRelayManagementAuthContractAndQuery(t *testing.T) {
	s, _, _ := newSecurityServer(t, nil)
	cookie, csrf := securityLogin(t, s)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"data":[{"id":"synthetic-model"}]}`))
	}))
	defer upstream.Close()
	for _, path := range []string{"/api/v1/providers/save", "/api/v1/providers/models"} {
		w := securityRequest(s, securityRequestOptions{method: "POST", path: path, body: `{}`})
		if w.Code != 401 {
			t.Fatal("匿名请求未拒绝")
		}
		w = securityRequest(s, securityRequestOptions{method: "POST", path: path, body: `{}`, cookie: cookie, origin: "http://" + s.authority})
		if w.Code != 403 {
			t.Fatal("CSRF未拒绝")
		}
	}
	in := relayInput()
	in.BaseURL = upstream.URL
	raw, _ := json.Marshal(in)
	request := securityRequestOptions{method: "POST", path: "/api/v1/providers/save", body: string(raw), cookie: cookie, csrf: csrf, origin: "http://" + s.authority}
	w := securityRequest(s, request)
	if w.Code != 200 || strings.Contains(w.Body.String(), in.Key) {
		t.Fatal("统一保存响应失败或含凭据")
	}
	var created api.Provider
	_ = json.Unmarshal(w.Body.Bytes(), &created)
	query, _ := json.Marshal(api.ModelQueryInput{ProviderID: created.ID, BaseURL: upstream.URL})
	request.path = "/api/v1/providers/models"
	request.body = string(query)
	w = securityRequest(s, request)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "synthetic-model") {
		t.Fatal("真实模型接口请求失败", w.Body.String())
	}
	s.modelQueries.Store(2)
	w = securityRequest(s, request)
	if w.Code != 503 {
		t.Fatal("模型查询并发未限制")
	}
	s.modelQueries.Store(0)
}
