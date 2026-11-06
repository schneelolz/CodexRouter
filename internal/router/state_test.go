package router

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"codexrouter/internal/api"
	"codexrouter/internal/platform"
)

func testState(t *testing.T) (*state, *testPersistence) {
	t.Helper()
	p := &testPersistence{}
	s, err := loadState(p)
	if err != nil {
		t.Fatal(err)
	}
	return s, p
}

// setProviderURL 在测试中为供应商写入合成基址。
func setProviderURL(t *testing.T, s *state, id, base string) {
	t.Helper()
	if _, err := s.updateProvider(id, api.ProviderUpdateInput{Name: "合成线路", BaseURL: base, Model: ""}); err != nil {
		t.Fatalf("设置供应商基址失败: %v", err)
	}
}

func validDisk(t *testing.T) diskState {
	t.Helper()
	return diskState{Version: stateVersion, Config: DefaultConfig(), Providers: defaultProviders(time.Now().UTC().Format(time.RFC3339)), Bindings: map[string]api.Binding{}}
}

func TestStateInitialAndCorruptStorageNeverSilentlyRebuilt(t *testing.T) {
	for _, test := range []struct {
		name string
		p    *testPersistence
	}{
		{"读取失败", &testPersistence{readErr: errors.New("synthetic read failure")}},
		{"JSON 损坏", &testPersistence{data: []byte(`{"version":`)}},
		{"版本不兼容", &testPersistence{data: []byte(`{"version":3,"config":{},"providers":[],"bindings":{}}`)}},
		{"绑定结构缺失", &testPersistence{data: []byte(`{"version":2,"config":{},"providers":[],"bindings":null}`)}},
		{"供应商未知字段", &testPersistence{data: []byte(`{"version":2,"config":{},"providers":[{"id":"official","unknown":true}],"bindings":{}}`)}},
	} {
		t.Run(test.name, func(t *testing.T) {
			before, _ := test.p.snapshot()
			if _, err := loadState(test.p); err == nil {
				t.Fatal("损坏或不可读状态未阻止启动")
			}
			after, writes := test.p.snapshot()
			if !bytes.Equal(before, after) || writes != 0 {
				t.Fatal("失败启动重建或修改了原状态")
			}
		})
	}
	p := &testPersistence{writeErr: errors.New("synthetic init write failure")}
	if _, err := loadState(p); err == nil {
		t.Fatal("首次状态不能持久化时仍启动")
	}
	s, clean := testState(t)
	c := s.config()
	views, err := s.providers()
	if err != nil {
		t.Fatal(err)
	}
	providers := views.Items
	if c.DefaultProviderID != BuiltinOfficialID || c.Version != stateVersion || len(providers) != 2 || providers[0].ID != BuiltinOfficialID || providers[0].BaseURL != BuiltinOfficialURL || providers[1].BaseURL != "" {
		t.Fatal("初始配置没有保持保守默认和旧线路兼容")
	}
	data, writes := clean.snapshot()
	if len(data) == 0 || writes != 1 {
		t.Fatal("首次配置未成功持久化")
	}
}

func TestInvalidPersistedBindingPreventsStartup(t *testing.T) {
	for _, binding := range []api.Binding{
		{ID: "different", ProviderID: BuiltinOfficialID, CreatedAt: "2026-11-03T00:00:00Z"},
		{ID: "session", ProviderID: "missing-provider", CreatedAt: "2026-11-03T00:00:00Z"},
		{ID: "session", ProviderID: BuiltinOfficialID, CreatedAt: "not-a-time"},
	} {
		disk := validDisk(t)
		disk.Bindings["session"] = binding
		data, err := json.Marshal(disk)
		if err != nil {
			t.Fatal(err)
		}
		p := &testPersistence{data: data}
		if _, err := loadState(p); err == nil {
			t.Fatal("损坏绑定未阻止启动")
		}
		_, writes := p.snapshot()
		if writes != 0 {
			t.Fatal("损坏绑定被静默重写")
		}
	}
	disk := validDisk(t)
	disk.Bindings = make(map[string]api.Binding, 10001)
	for i := range 10001 {
		id := fmt.Sprintf("thread-%d", i)
		disk.Bindings[id] = api.Binding{ID: id, ProviderID: BuiltinOfficialID, CreatedAt: time.Now().UTC().Format(time.RFC3339)}
	}
	data, err := json.Marshal(disk)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := loadState(&testPersistence{data: data}); err == nil {
		t.Fatal("超限持久化绑定未阻止启动")
	}
}

func TestLegacyStateMigrationPreservesBindingsAndCompanyCredential(t *testing.T) {
	legacy := `{"version":1,"config":{"version":1,"default_route":"company","official_url":"https://chatgpt.com/backend-api/codex","company_url":"https://company.example.test/v1","connect_timeout_ms":11000,"header_timeout_ms":30000,"idle_timeout_ms":120000,"write_timeout_ms":30000,"max_concurrent":16,"upstreams_locked":true},"bindings":{"legacy-official":{"id":"legacy-official","route":"official","created_at":"2026-11-01T00:00:00Z"},"legacy-company":{"id":"legacy-company","route":"company","created_at":"2026-11-01T00:00:01Z"}}}`
	p := &testPersistence{data: []byte(legacy)}
	s, err := loadState(p)
	if err != nil {
		t.Fatal(err)
	}
	_, writes := p.snapshot()
	if writes != 1 {
		t.Fatal("版本 1 状态没有立即迁移落盘")
	}
	c := s.config()
	if c.Version != stateVersion || c.DefaultProviderID != BuiltinCompanyID || c.ConnectTimeoutMS != 11000 {
		t.Fatal("旧配置未迁移为供应商默认值与超时")
	}
	views, err := s.providers()
	if err != nil {
		t.Fatal(err)
	}
	providers := map[string]api.Provider{}
	for _, item := range views.Items {
		providers[item.ID] = item
	}
	if p0 := providers[BuiltinOfficialID]; p0.Name != "官方线路" || p0.Kind != api.ProviderKindOfficial || p0.BaseURL != "https://chatgpt.com/backend-api/codex" {
		t.Fatal("旧官方线路未迁移")
	}
	if p1 := providers[BuiltinCompanyID]; p1.Kind != api.ProviderKindAPIKey || p1.BaseURL != "https://company.example.test/v1" || !p1.UpstreamLocked {
		t.Fatal("旧公司线路未迁移或未锁定")
	}
	for id, want := range map[string]string{"legacy-official": BuiltinOfficialID, "legacy-company": BuiltinCompanyID} {
		if got, err := s.resolve(id); err != nil || got != want {
			t.Fatalf("旧绑定未保留 %s: %v %v", id, got, err)
		}
	}
	// 旧公司凭据名称必须继续可用，不允许换成新命名空间。
	creds := &testCredentials{}
	if err := creds.Set("company", "synthetic-legacy-company"); err != nil {
		t.Fatal(err)
	}
	s.creds = creds
	status, err := s.credentialStatus(BuiltinCompanyID)
	if err != nil || !status.Configured || status.ProviderID != BuiltinCompanyID {
		t.Fatal("旧公司凭据命名空间未被沿用")
	}
	restarted, err := loadState(p)
	if err != nil || restarted.config().DefaultProviderID != BuiltinCompanyID {
		t.Fatal("迁移后的状态无法重新加载")
	}
}

func TestConfigWriteFailurePreservesPublishedAndStoredConfig(t *testing.T) {
	s, p := testState(t)
	before := s.config()
	storedBefore, _ := p.snapshot()
	next := before
	next.HeaderTimeoutMS += 1000
	p.setWriteError(errors.New("synthetic write failure"))
	if err := s.saveConfig(next); err == nil {
		t.Fatal("持久化失败未返回错误")
	}
	storedAfter, _ := p.snapshot()
	if got := s.config(); got != before || !bytes.Equal(storedBefore, storedAfter) {
		t.Fatal("配置写入失败后发布了未持久化配置")
	}
	p.setWriteError(nil)
	if err := s.saveConfig(next); err != nil {
		t.Fatal(err)
	}
	restarted, err := loadState(p)
	if err != nil || restarted.config().HeaderTimeoutMS != next.HeaderTimeoutMS {
		t.Fatal("恢复后的配置写入或重启读取失败")
	}
}

func TestConfigAPIFailureKeepsLastConfigAndRedactsBackendError(t *testing.T) {
	s, p, _ := newSecurityServer(t, nil)
	cookie, csrf := securityLogin(t, s)
	before := s.state.config()
	next := before
	next.IdleTimeoutMS += 1000
	in := api.ConfigInput{DefaultProviderID: next.DefaultProviderID, ConnectTimeoutMS: next.ConnectTimeoutMS, HeaderTimeoutMS: next.HeaderTimeoutMS, IdleTimeoutMS: next.IdleTimeoutMS, WriteTimeoutMS: next.WriteTimeoutMS, MaxConcurrent: next.MaxConcurrent}
	data, _ := json.Marshal(in)
	p.setWriteError(errors.New("synthetic-private-filesystem-path"))
	w := securityRequest(s, securityRequestOptions{method: http.MethodPut, path: "/api/v1/config", body: string(data), origin: "http://" + s.authority, cookie: cookie, csrf: csrf})
	if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "CONFIG_SAVE_FAILED") || strings.Contains(w.Body.String(), "synthetic-private-filesystem-path") {
		t.Fatal("配置写失败未安全报告")
	}
	if s.state.config() != before {
		t.Fatal("API 保存失败改变了当前配置")
	}
}

func TestUnknownSessionsRequireRegistrationBeforeBinding(t *testing.T) {
	s, p := testState(t)
	if _, err := s.resolve("session-new"); err == nil || err.Error() != "SESSION_REGISTRATION_REQUIRED" {
		t.Fatal("来源不明会话未要求首次登记")
	}
	if _, err := s.resolve("session-new"); err == nil {
		t.Fatal("重复未知请求被自动绑定")
	}
	if got := s.sessions(); len(got.Pending) != 1 || len(got.Bindings) != 0 {
		t.Fatal("待登记去重或未绑定规则错误")
	}
	_, writes := p.snapshot()
	if writes != 1 {
		t.Fatal("来源不明请求产生了持久绑定")
	}
	if _, err := s.register(api.RegisterInput{ID: "not-pending", ProviderID: BuiltinOfficialID}); err == nil || err.Error() != "SESSION_NOT_PENDING" {
		t.Fatal("不在待登记列表的会话可直接登记")
	}
	if _, err := s.register(api.RegisterInput{ID: "session-new", ProviderID: BuiltinCompanyID}); err == nil || err.Error() != "UPSTREAM_NOT_CONFIGURED" {
		t.Fatal("未配置公司地址时可绑定公司线路")
	}
}

func TestBindingWriteMustSucceedBeforeRouteBecomesAvailable(t *testing.T) {
	s, p := testState(t)
	_, _ = s.resolve("session-one")
	before, _ := p.snapshot()
	p.setWriteError(errors.New("synthetic disk failure"))
	if _, err := s.register(api.RegisterInput{ID: "session-one", ProviderID: BuiltinOfficialID}); err == nil || err.Error() != "STORAGE_WRITE_FAILED" {
		t.Fatal("绑定写失败未返回稳定错误码")
	}
	after, _ := p.snapshot()
	if got := s.sessions(); len(got.Bindings) != 0 || len(got.Pending) != 1 || !bytes.Equal(before, after) {
		t.Fatal("绑定写失败仍发布了绑定或丢失了待登记项")
	}
	if _, err := s.resolve("session-one"); err == nil {
		t.Fatal("未持久化的会话可以转发")
	}
	p.setWriteError(nil)
	binding, err := s.register(api.RegisterInput{ID: "session-one", ProviderID: BuiltinOfficialID})
	if err != nil || binding.ProviderID != BuiltinOfficialID {
		t.Fatal("存储恢复后无法登记")
	}
	if provider, err := s.resolve("session-one"); err != nil || provider != BuiltinOfficialID {
		t.Fatal("持久化成功后无法解析绑定")
	}
}

func TestConcurrentFirstRequestsAndRegistrationHaveOneBinding(t *testing.T) {
	s, p := testState(t)
	var wg sync.WaitGroup
	for range 32 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := s.resolve("session-concurrent"); err == nil || err.Error() != "SESSION_REGISTRATION_REQUIRED" {
				t.Error("并发未知会话未保持待登记")
			}
		}()
	}
	wg.Wait()
	if len(s.sessions().Pending) != 1 {
		t.Fatal("并发首次请求产生了重复待登记项")
	}
	for range 32 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := s.register(api.RegisterInput{ID: "session-concurrent", ProviderID: BuiltinOfficialID}); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if got := s.sessions(); len(got.Bindings) != 1 || len(got.Pending) != 0 {
		t.Fatal("并发登记未形成唯一绑定")
	}
	_, writes := p.snapshot()
	if writes != 2 {
		t.Fatalf("同一会话应只写入一次绑定，含初始化实际写入次数 %d", writes)
	}
}

func TestDefaultSwitchRestartAndUpstreamLockPreserveBindings(t *testing.T) {
	s, p := testState(t)
	setProviderURL(t, s, BuiltinCompanyID, "http://127.0.0.1:19999/v1")
	_, _ = s.resolve("official-thread")
	if _, err := s.register(api.RegisterInput{ID: "official-thread", ProviderID: BuiltinOfficialID}); err != nil {
		t.Fatal(err)
	}
	config := s.config()
	config.DefaultProviderID = BuiltinCompanyID
	config.HeaderTimeoutMS += 1000
	if err := s.saveConfig(config); err != nil {
		t.Fatal("产生绑定后应仍允许切换新会话默认值和超时")
	}
	_, _ = s.resolve("company-thread")
	if _, err := s.register(api.RegisterInput{ID: "company-thread", ProviderID: BuiltinCompanyID}); err != nil {
		t.Fatal(err)
	}
	_, _ = s.resolve("unknown-history-thread")
	for id, base := range map[string]string{BuiltinOfficialID: "https://example.test/different", BuiltinCompanyID: "http://127.0.0.1:20000/v1"} {
		if _, err := s.updateProvider(id, api.ProviderUpdateInput{Name: "被锁定的线路", BaseURL: base, Model: ""}); err == nil || err.Error() != "PROVIDER_LOCKED" {
			t.Fatalf("已有绑定后仍能修改 %s 上游基址", id)
		}
	}
	if _, err := s.register(api.RegisterInput{ID: "official-thread", ProviderID: BuiltinCompanyID}); err == nil || err.Error() != "SESSION_ALREADY_BOUND" {
		t.Fatal("允许已有会话跨供应商迁移")
	}
	restarted, err := loadState(p)
	if err != nil {
		t.Fatal(err)
	}
	for id, want := range map[string]string{"official-thread": BuiltinOfficialID, "company-thread": BuiltinCompanyID} {
		if got, err := restarted.resolve(id); err != nil || got != want {
			t.Fatalf("默认切换/重启改变了原绑定 %s", id)
		}
	}
	if restarted.config().DefaultProviderID != BuiltinCompanyID {
		t.Fatal("重启未恢复默认供应商")
	}
	locked := map[string]bool{}
	restartedViews, err := restarted.providers()
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range restartedViews.Items {
		locked[item.ID] = item.UpstreamLocked
	}
	if !locked[BuiltinOfficialID] || !locked[BuiltinCompanyID] {
		t.Fatal("重启未恢复供应商基址锁定状态")
	}
	if provider, err := restarted.resolve("unknown-history-thread"); err == nil || provider != "" {
		t.Fatal("公司默认下来源不明历史会话被自动发送")
	}
}

func TestConcurrentConflictingRegistrationCannotMigrate(t *testing.T) {
	s, _ := testState(t)
	setProviderURL(t, s, BuiltinCompanyID, "http://127.0.0.1:19999/v1")
	_, _ = s.resolve("competing-thread")
	var wg sync.WaitGroup
	for i := range 32 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			provider := BuiltinOfficialID
			if i%2 == 1 {
				provider = BuiltinCompanyID
			}
			_, err := s.register(api.RegisterInput{ID: "competing-thread", ProviderID: provider})
			if err != nil && err.Error() != "SESSION_ALREADY_BOUND" {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	got := s.sessions()
	if len(got.Bindings) != 1 || (got.Bindings[0].ProviderID != BuiltinOfficialID && got.Bindings[0].ProviderID != BuiltinCompanyID) {
		t.Fatal("冲突并发登记没有形成唯一合法绑定")
	}
	want := got.Bindings[0].ProviderID
	for range 20 {
		if provider, err := s.resolve("competing-thread"); err != nil || provider != want {
			t.Fatal("已确定的并发绑定不稳定")
		}
	}
}

func TestPendingAndBindingCountsAreBounded(t *testing.T) {
	s, _ := testState(t)
	for i := range 256 {
		_, _ = s.resolve(fmt.Sprintf("pending-%d", i))
	}
	if _, err := s.resolve("overflow-pending"); err == nil || err.Error() != "PENDING_LIMIT" {
		t.Fatal("待登记数量未遵守 256 条边界")
	}
	s.mu.Lock()
	for i := range 10000 {
		id := fmt.Sprintf("binding-%d", i)
		s.disk.Bindings[id] = api.Binding{ID: id, ProviderID: BuiltinOfficialID}
	}
	s.mu.Unlock()
	if _, err := s.register(api.RegisterInput{ID: "pending-0", ProviderID: BuiltinOfficialID}); err == nil || err.Error() != "BINDING_LIMIT" {
		t.Fatal("绑定数量未遵守 10000 条边界")
	}
}

func TestConfigAndSessionInputValidation(t *testing.T) {
	providers := defaultProviders(time.Now().UTC().Format(time.RFC3339))
	for _, edit := range []func(*api.Config){
		func(c *api.Config) { c.Version = 1 },
		func(c *api.Config) { c.DefaultProviderID = "unknown" },
		func(c *api.Config) { c.DefaultProviderID = BuiltinCompanyID },
		func(c *api.Config) { c.ConnectTimeoutMS = 99 },
		func(c *api.Config) { c.HeaderTimeoutMS = 3600001 },
		func(c *api.Config) { c.MaxConcurrent = 0 },
		func(c *api.Config) { c.MaxConcurrent = 129 },
	} {
		config := DefaultConfig()
		edit(&config)
		if err := validateConfig(config, providers); err == nil {
			t.Fatal("不合法配置通过校验")
		}
	}
	for _, id := range []string{"", "contains space", "../path", strings.Repeat("x", 129), "中文", "with\nline", "control"} {
		if validProviderID(id) {
			t.Fatalf("不安全的供应商标识通过校验: %q", id)
		}
	}
	for _, id := range []string{"official", "company", "p123", strings.Repeat("x", 64)} {
		if !validProviderID(id) {
			t.Fatal("合法供应商标识未通过校验")
		}
	}
	for _, id := range []string{"", "contains space", "../path", strings.Repeat("x", 129), "中文", "with\nline"} {
		if validID(id) {
			t.Fatalf("不安全会话标识通过校验: %q", id)
		}
	}
	for _, id := range []string{"thread-123", "thread_123", "123ABC", strings.Repeat("x", 128)} {
		if !validID(id) {
			t.Fatal("合法会话标识未通过校验")
		}
	}
}

func TestProviderFieldValidation(t *testing.T) {
	base := providerRecord{ID: "p123", Name: "线路", Kind: api.ProviderKindAPIKey, CreatedAt: "2026-11-03T00:00:00Z"}
	for _, edit := range []func(*providerRecord){
		func(p *providerRecord) { p.Kind = "unknown" },
		func(p *providerRecord) { p.ID = "control" },
		func(p *providerRecord) { p.Name = "" },
		func(p *providerRecord) { p.Name = strings.Repeat("名", 65) },
		func(p *providerRecord) { p.Name = "line\nbreak" },
		func(p *providerRecord) { p.BaseURL = "https://user:password@example.test/v1" },
		func(p *providerRecord) { p.BaseURL = "https://example.test/v1?secret=value" },
		func(p *providerRecord) { p.BaseURL = "https://example.test/v1#fragment" },
		func(p *providerRecord) { p.BaseURL = "file:///tmp/state" },
		func(p *providerRecord) { p.BaseURL = "http://8.8.8.8/v1" },
		func(p *providerRecord) { p.BaseURL = "http://example.test/v1" },
		func(p *providerRecord) { p.BaseURL = "https://example.test/%2e%2e/private" },
		func(p *providerRecord) { p.BaseURL = "https://example.test/" + strings.Repeat("a", 2048) },
		func(p *providerRecord) { p.Model = "gpt has space" },
		func(p *providerRecord) { p.Model = strings.Repeat("m", 129) },
		func(p *providerRecord) { p.CreatedAt = "not-a-time" },
	} {
		candidate := base
		edit(&candidate)
		if err := validateProvider(candidate); err == nil {
			t.Fatalf("不合法供应商通过校验: %+v", candidate)
		}
	}
	if err := validateProvider(base); err != nil {
		t.Fatal(err)
	}
	official := providerRecord{ID: BuiltinOfficialID, Name: "官方线路", Kind: api.ProviderKindOfficial, BaseURL: BuiltinOfficialURL, CreatedAt: base.CreatedAt}
	if err := validateProvider(official); err != nil {
		t.Fatal(err)
	}
	official.BaseURL = ""
	if err := validateProvider(official); err == nil {
		t.Fatal("官方转发供应商允许空基址")
	}
	if !modelPattern.MatchString("gpt-6.1-sol") || modelPattern.MatchString("模型") {
		t.Fatal("模型覆盖字符集校验不符")
	}
}

func TestProviderCRUDLimitsAndLocks(t *testing.T) {
	s, p := testState(t)
	created, err := s.addProvider(api.ProviderCreateInput{Name: "备用线路", Kind: api.ProviderCreateKindAPIKey, BaseURL: "https://api.example.test/v1", Model: "gpt-test"})
	if err != nil || created.ID == "" || created.KeyConfigured || created.UpstreamLocked || created.Model != "gpt-test" {
		t.Fatalf("新建供应商失败: %+v %v", created, err)
	}
	for _, kind := range []api.ProviderCreateKind{"oauth", "official"} {
		if _, err := s.addProvider(api.ProviderCreateInput{Name: "非法", Kind: kind, BaseURL: ""}); err == nil || err.Error() != "INVALID_INPUT" {
			t.Fatalf("新增供应商接受认证方式 %q", kind)
		}
	}
	if _, err := s.addProvider(api.ProviderCreateInput{Name: "非法基址", Kind: api.ProviderCreateKindAPIKey, BaseURL: "http://public.example.test/v1"}); err == nil {
		t.Fatal("公网 HTTP 基址被接受")
	}
	for i := 0; i < maxProviders-3; i++ {
		if _, err := s.addProvider(api.ProviderCreateInput{Name: fmt.Sprintf("线路%d", i), Kind: api.ProviderCreateKindAPIKey}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.addProvider(api.ProviderCreateInput{Name: "超限", Kind: api.ProviderCreateKindAPIKey}); err == nil || err.Error() != "PROVIDER_LIMIT" {
		t.Fatal("供应商数量未遵守上限")
	}
	if _, err := s.deleteProvider(BuiltinOfficialID); err == nil || err.Error() != "PROVIDER_BUILTIN" {
		t.Fatal("允许删除内置官方供应商")
	}
	if _, err := s.deleteProvider(created.ID); err != nil {
		t.Fatalf("未绑定供应商无法删除: %v", err)
	}
	if _, ok := s.providerRecord(created.ID); ok {
		t.Fatal("删除后仍可读取供应商")
	}
	// 删除的标识不能复用，避免继承旧凭据命名空间。
	used := map[string]bool{}
	currentViews, err := s.providers()
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range currentViews.Items {
		used[item.ID] = true
	}
	replacement, err := s.addProvider(api.ProviderCreateInput{Name: "替代线路", Kind: api.ProviderCreateKindAPIKey})
	if err != nil || used[replacement.ID] || replacement.ID == created.ID {
		t.Fatal("新建供应商标识复用了旧标识")
	}
	if _, err := s.updateProvider(BuiltinOfficialID, api.ProviderUpdateInput{Name: "官方线路", BaseURL: BuiltinOfficialURL, Model: "gpt-official"}); err != nil {
		t.Fatal("无绑定时应允许设置官方模型覆盖")
	}
	_, _ = s.resolve("provider-thread")
	if _, err := s.register(api.RegisterInput{ID: "provider-thread", ProviderID: BuiltinOfficialID}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.deleteProvider(BuiltinOfficialID); err == nil {
		t.Fatal("允许删除有绑定或内置供应商")
	}
	if _, err := s.updateProvider(BuiltinOfficialID, api.ProviderUpdateInput{Name: "官方线路", BaseURL: "https://example.test/v2", Model: "gpt-official"}); err == nil || err.Error() != "PROVIDER_LOCKED" {
		t.Fatal("有绑定后仍允许改基址")
	}
	if _, err := s.updateProvider(BuiltinOfficialID, api.ProviderUpdateInput{Name: "官方线路", BaseURL: BuiltinOfficialURL, Model: ""}); err != nil {
		t.Fatal("有绑定后应允许修改名称和模型覆盖")
	}
	if _, err := s.updateProvider("missing-provider", api.ProviderUpdateInput{Name: "缺失", BaseURL: "", Model: ""}); err == nil || err.Error() != "PROVIDER_NOT_FOUND" {
		t.Fatal("更新不存在的供应商未返回稳定错误")
	}
	// 默认供应商不能清空基址，否则重启校验会拒绝状态。
	setProviderURL(t, s, BuiltinCompanyID, "https://company.example.test/v1")
	cfg := s.config()
	cfg.DefaultProviderID = BuiltinCompanyID
	if err := s.saveConfig(cfg); err != nil {
		t.Fatal(err)
	}
	if _, err := s.updateProvider(BuiltinCompanyID, api.ProviderUpdateInput{Name: "公司线路", BaseURL: "", Model: ""}); err == nil || err.Error() != "PROVIDER_DEFAULT" {
		t.Fatal("默认供应商允许清空上游基址")
	}
	if _, err := s.updateProvider(replacement.ID, api.ProviderUpdateInput{Name: "替代线路", BaseURL: "https://api.example.test/v2", Model: ""}); err != nil {
		t.Fatal(err)
	}
	config := s.config()
	config.DefaultProviderID = replacement.ID
	if err := s.saveConfig(config); err != nil {
		t.Fatal(err)
	}
	if _, err := s.deleteProvider(replacement.ID); err == nil || err.Error() != "PROVIDER_DEFAULT" {
		t.Fatal("删除当前默认供应商未受保护")
	}
	p.setWriteError(errors.New("synthetic write failure"))
	if _, err := s.updateProvider(BuiltinCompanyID, api.ProviderUpdateInput{Name: "写入失败", BaseURL: "", Model: ""}); err == nil || err.Error() != "STORAGE_WRITE_FAILED" {
		t.Fatal("供应商写入失败未返回稳定错误")
	}
	if company, ok := s.providerRecord(BuiltinCompanyID); !ok || company.Name == "写入失败" {
		t.Fatal("供应商写入失败仍发布了修改")
	}
}

func TestThreadIdentityRequiresConsistentEvidence(t *testing.T) {
	for _, test := range []struct {
		name     string
		thread   string
		session  string
		metadata string
		code     string
	}{
		{name: "没有会话标识", code: "SESSION_ID_REQUIRED"},
		{name: "非法标识", thread: "thread,other", code: "SESSION_ID_REQUIRED"},
		{name: "握手标识冲突", thread: "thread-one", session: "thread-two", code: "SESSION_ID_CONFLICT"},
		{name: "元数据线程冲突", thread: "thread-one", metadata: `{"thread_id":"thread-two","history_initialization":"new"}`, code: "SESSION_ID_CONFLICT"},
		{name: "元数据会话冲突", thread: "thread-one", metadata: `{"thread_id":"thread-one","session_id":"thread-two"}`, code: "SESSION_ID_CONFLICT"},
		{name: "父标识无效", thread: "thread-one", metadata: `{"thread_id":"thread-one","forked_from_thread_id":"../parent"}`, code: "SESSION_ID_CONFLICT"},
		{name: "格式错误", thread: "thread-one", metadata: `{`, code: "SESSION_METADATA_INVALID"},
		{name: "元数据超限", thread: "thread-one", metadata: strings.Repeat("x", 8193), code: "SESSION_METADATA_INVALID"},
		{name: "无来源证据仍可识别", thread: "thread-one"},
		{name: "新线程元数据", thread: "thread-one", session: "thread-one", metadata: `{"thread_id":"thread-one","session_id":"thread-one","history_initialization":"new","future_field":{"retained_by_forward":true}}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:18788/responses", nil)
			if test.thread != "" {
				r.Header.Set("Thread-Id", test.thread)
			}
			if test.session != "" {
				r.Header.Set("Session-Id", test.session)
			}
			if test.metadata != "" {
				r.Header.Set("X-Codex-Turn-Metadata", test.metadata)
			}
			evidence, err := identity(r)
			if test.code != "" {
				if err == nil || err.Error() != test.code {
					t.Fatalf("不一致身份错误=%v，期望 %s", err, test.code)
				}
			} else if err != nil || evidence.ThreadID != test.thread {
				t.Fatalf("一致身份解析失败: %+v %v", evidence, err)
			}
		})
	}
	for _, header := range []string{"Thread-Id", "Session-Id", "X-Codex-Turn-Metadata"} {
		t.Run("重复头部"+header, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:18788/responses", nil)
			r.Header.Set("Thread-Id", "thread-one")
			r.Header.Set("Session-Id", "thread-one")
			r.Header.Set("X-Codex-Turn-Metadata", `{"thread_id":"thread-one","history_initialization":"new"}`)
			r.Header.Add(header, "conflicting-extra-value")
			if _, err := identity(r); err == nil {
				t.Fatalf("忽略了重复 %s 中的冲突身份", header)
			}
		})
	}
}

func TestNewEvidenceUsesCurrentDefaultAndKnownForkInheritsParent(t *testing.T) {
	s, p := testState(t)
	setProviderURL(t, s, BuiltinCompanyID, "http://127.0.0.1:19999/v1")
	if provider, err := s.resolveEvidence(threadEvidence{ThreadID: "official-parent", History: "new"}); err != nil || provider != BuiltinOfficialID {
		t.Fatal("可靠新建证据没有使用当时的官方默认线路")
	}
	config := s.config()
	config.DefaultProviderID = BuiltinCompanyID
	if err := s.saveConfig(config); err != nil {
		t.Fatal(err)
	}
	if provider, err := s.resolveEvidence(threadEvidence{ThreadID: "company-parent", History: "new"}); err != nil || provider != BuiltinCompanyID {
		t.Fatal("默认切换后的可靠新会话没有绑定公司线路")
	}
	if provider, err := s.resolveEvidence(threadEvidence{ThreadID: "official-child", History: "cold_fork", Parent: "official-parent"}); err != nil || provider != BuiltinOfficialID {
		t.Fatal("已知来源 fork 未继承父线程，或误用公司默认")
	}
	if provider, err := s.resolveEvidence(threadEvidence{ThreadID: "company-child", History: "cold_fork", Parent: "company-parent"}); err != nil || provider != BuiltinCompanyID {
		t.Fatal("公司父线程 fork 未继承线路")
	}
	if provider, err := s.resolveEvidence(threadEvidence{ThreadID: "official-parent", History: "new"}); err != nil || provider != BuiltinOfficialID {
		t.Fatal("已有绑定被重复的新建元数据改线")
	}
	restarted, err := loadState(p)
	if err != nil {
		t.Fatal(err)
	}
	for id, want := range map[string]string{"official-parent": BuiltinOfficialID, "official-child": BuiltinOfficialID, "company-parent": BuiltinCompanyID, "company-child": BuiltinCompanyID} {
		if provider, err := restarted.resolveEvidence(threadEvidence{ThreadID: id, History: "cold_resume"}); err != nil || provider != want {
			t.Fatalf("重启恢复改变已有绑定 %s", id)
		}
	}
}

func TestUnprovenHistoryEvidenceStaysPendingUnderCompanyDefault(t *testing.T) {
	s, _ := testState(t)
	setProviderURL(t, s, BuiltinCompanyID, "http://127.0.0.1:19999/v1")
	config := s.config()
	config.DefaultProviderID = BuiltinCompanyID
	if err := s.saveConfig(config); err != nil {
		t.Fatal(err)
	}
	for _, evidence := range []threadEvidence{
		{ThreadID: "no-metadata"},
		{ThreadID: "cold-resume", History: "cold_resume"},
		{ThreadID: "unknown-fork", History: "cold_fork", Parent: "missing-parent"},
		{ThreadID: "missing-fork-parent", History: "cold_fork"},
		{ThreadID: "future-history", History: "future-kind"},
		{ThreadID: "conflicting-new-parent", History: "new", Parent: "missing-parent"},
	} {
		if provider, err := s.resolveEvidence(evidence); err == nil || err.Error() != "SESSION_REGISTRATION_REQUIRED" || provider != "" {
			t.Fatalf("来源不明历史被自动分配: %+v %s %v", evidence, provider, err)
		}
	}
	if got := s.sessions(); len(got.Bindings) != 0 || len(got.Pending) != 6 {
		t.Fatal("不可靠来源证据产生持久绑定或丢失待登记项")
	}
}

func TestEvidenceBindingWriteFailureAndConcurrentFirstRequest(t *testing.T) {
	s, p := testState(t)
	p.setWriteError(errors.New("synthetic evidence write failure"))
	if provider, err := s.resolveEvidence(threadEvidence{ThreadID: "new-thread", History: "new"}); err == nil || err.Error() != "STORAGE_WRITE_FAILED" || provider != "" {
		t.Fatal("可靠新建证据在落盘失败时仍获准转发")
	}
	if len(s.sessions().Bindings) != 0 {
		t.Fatal("证据绑定写入失败改变了内存状态")
	}
	p.setWriteError(nil)
	_, writesBefore := p.snapshot()
	var wg sync.WaitGroup
	for range 32 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if provider, err := s.resolveEvidence(threadEvidence{ThreadID: "new-thread", History: "new"}); err != nil || provider != BuiltinOfficialID {
				t.Error("并发新建证据没有形成唯一稳定线路")
			}
		}()
	}
	wg.Wait()
	_, writesAfter := p.snapshot()
	if len(s.sessions().Bindings) != 1 || writesAfter-writesBefore != 1 {
		t.Fatal("并发可靠新建证据重复持久化了绑定")
	}
}

// addTestProvider 用合成基址创建一个仅 API 密钥的供应商。
func addTestProvider(t *testing.T, s *state, name, base string) string {
	t.Helper()
	created, err := s.addProvider(api.ProviderCreateInput{Name: name, Kind: api.ProviderCreateKindAPIKey, BaseURL: base})
	if err != nil {
		t.Fatalf("创建供应商失败: %v", err)
	}
	return created.ID
}

func TestPersistedBindingRequiresUsableProvider(t *testing.T) {
	legacy := func(officialURL, companyURL, route string) string {
		return fmt.Sprintf(`{"version":1,"config":{"version":1,"default_route":"official","official_url":%q,"company_url":%q,"connect_timeout_ms":10000,"header_timeout_ms":30000,"idle_timeout_ms":120000,"write_timeout_ms":30000,"max_concurrent":16,"upstreams_locked":false},"bindings":{"session":{"id":"session","route":%q,"created_at":"2026-11-01T00:00:00Z"}}}`, officialURL, companyURL, route)
	}
	for _, test := range []struct{ name, data string }{
		{"版本1公司绑定无地址", legacy("https://chatgpt.com/backend-api/codex", "", "company")},
		{"版本1官方绑定无地址", legacy("", "https://company.example.test/v1", "official")},
	} {
		t.Run(test.name, func(t *testing.T) {
			p := &testPersistence{data: []byte(test.data)}
			if _, err := loadState(p); err == nil {
				t.Fatal("已绑定但缺少上游地址的版本 1 状态未阻止启动")
			}
			if _, writes := p.snapshot(); writes != 0 {
				t.Fatal("拒绝启动时改写了版本 1 状态")
			}
		})
	}
	disk := validDisk(t)
	disk.Bindings["session"] = api.Binding{ID: "session", ProviderID: BuiltinCompanyID, CreatedAt: "2026-11-03T00:00:00Z"}
	data, err := json.Marshal(disk)
	if err != nil {
		t.Fatal(err)
	}
	p := &testPersistence{data: data}
	if _, err = loadState(p); err == nil {
		t.Fatal("已绑定但缺少上游地址的版本 2 状态未阻止启动")
	}
	if _, writes := p.snapshot(); writes != 0 {
		t.Fatal("拒绝启动时改写了版本 2 状态")
	}
	// 只有内置官方供应商可以转发客户端官方身份。
	disk = validDisk(t)
	disk.Providers = append(disk.Providers, providerRecord{ID: "p1", Name: "伪装官方", Kind: api.ProviderKindOfficial, BaseURL: "https://api.example.test/v1", CreatedAt: "2026-11-03T00:00:00Z"})
	if data, err = json.Marshal(disk); err != nil {
		t.Fatal(err)
	}
	p = &testPersistence{data: data}
	if _, err = loadState(p); err == nil {
		t.Fatal("非内置供应商使用官方身份未被拒绝")
	}
	if _, writes := p.snapshot(); writes != 0 {
		t.Fatal("拒绝非内置官方身份时改写了状态")
	}
}

func TestDeleteProviderKeepsKeyWhenStateWriteFails(t *testing.T) {
	s, p := testState(t)
	creds := &testCredentials{}
	s.creds = creds
	id := addTestProvider(t, s, "待删线路", "https://api.example.test/v1")
	if _, err := s.setProviderKey(id, "synthetic-delete-key"); err != nil {
		t.Fatal(err)
	}
	p.setWriteError(errors.New("synthetic delete write failure"))
	if _, err := s.deleteProvider(id); err == nil || err.Error() != "STORAGE_WRITE_FAILED" {
		t.Fatalf("删除写盘失败未返回稳定错误: %v", err)
	}
	if _, ok := s.providerRecord(id); !ok {
		t.Fatal("删除写盘失败仍移除了供应商")
	}
	if value, err := creds.Get(id); err != nil || value != "synthetic-delete-key" {
		t.Fatal("删除写盘失败后没有恢复原密钥")
	}
	// 补偿也失败时必须明确报告，不能谎称原配置完全有效。
	creds.setErr = errors.New("synthetic credential restore failure")
	p.setWriteError(errors.New("synthetic delete write failure"))
	if _, err := s.deleteProvider(id); err == nil || err.Error() != "CREDENTIAL_RESTORE_FAILED" {
		t.Fatalf("补偿失败未返回明确错误: %v", err)
	}
	if _, ok := s.providerRecord(id); !ok {
		t.Fatal("补偿失败时移除了供应商")
	}
	if _, err := creds.Get(id); !errors.Is(err, platform.ErrNotFound) {
		t.Fatal("补偿失败后凭据应已丢失")
	}
	creds.setErr = nil
	p.setWriteError(nil)
	if _, err := s.setProviderKey(id, "synthetic-reentered-key"); err != nil {
		t.Fatal("补偿失败后无法重新录入密钥")
	}
}

func TestCredentialReadFailurePreventsMutation(t *testing.T) {
	s, p := testState(t)
	creds := &testCredentials{}
	s.creds = creds
	id := addTestProvider(t, s, "读取失败线路", "https://api.example.test/v1")
	_, writesBefore := p.snapshot()
	creds.getErr = errors.New("synthetic credential read failure")
	if _, err := s.addProvider(api.ProviderCreateInput{Name: "新线路", Kind: api.ProviderCreateKindAPIKey, BaseURL: "https://api.example.test/v2"}); err == nil || err.Error() != "CREDENTIAL_UNAVAILABLE" {
		t.Fatalf("凭据读取失败时仍然新增供应商: %v", err)
	}
	if _, err := s.updateProvider(id, api.ProviderUpdateInput{Name: "改名线路", BaseURL: "https://api.example.test/v2", Model: ""}); err == nil || err.Error() != "CREDENTIAL_UNAVAILABLE" {
		t.Fatalf("凭据读取失败时仍然更新供应商: %v", err)
	}
	if _, err := s.deleteProvider(id); err == nil || err.Error() != "CREDENTIAL_UNAVAILABLE" {
		t.Fatalf("凭据读取失败时仍然删除供应商: %v", err)
	}
	_, writesAfter := p.snapshot()
	if writesAfter != writesBefore {
		t.Fatal("凭据读取失败时仍写入了状态")
	}
	if got, ok := s.providerRecord(id); !ok || got.Name != "读取失败线路" {
		t.Fatal("凭据读取失败仍发布了供应商修改")
	}
	if _, ok := s.providerRecord("p"); ok {
		t.Fatal("凭据读取失败仍新增了供应商")
	}
}

func TestDeleteProviderResponseUsesActualBindingLocks(t *testing.T) {
	s, _ := testState(t)
	s.creds = &testCredentials{}
	setProviderURL(t, s, BuiltinCompanyID, "http://127.0.0.1:19999/v1")
	_, _ = s.resolve("company-thread")
	if _, err := s.register(api.RegisterInput{ID: "company-thread", ProviderID: BuiltinCompanyID}); err != nil {
		t.Fatal(err)
	}
	id := addTestProvider(t, s, "待删线路", "https://api.example.test/v1")
	items, err := s.deleteProvider(id)
	if err != nil {
		t.Fatal(err)
	}
	locked := map[string]bool{}
	for _, item := range items.Items {
		locked[item.ID] = item.UpstreamLocked
	}
	if !locked[BuiltinCompanyID] || locked[BuiltinOfficialID] {
		t.Fatalf("删除返回视图没有按实际绑定数恢复锁定状态: %+v", items.Items)
	}
	if _, ok := locked[id]; ok {
		t.Fatal("删除返回视图仍包含已删除供应商")
	}
}

func TestConcurrentKeyWriteAndProviderDeleteLeaveNoOrphan(t *testing.T) {
	s, _ := testState(t)
	creds := &testCredentials{}
	s.creds = creds
	for i := range 16 {
		id := addTestProvider(t, s, fmt.Sprintf("并发线路%d", i), "https://api.example.test/v1")
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			_, _ = s.setProviderKey(id, "synthetic-concurrent-key")
		}()
		go func() {
			defer wg.Done()
			if _, err := s.deleteProvider(id); err != nil {
				t.Error(err)
			}
		}()
		wg.Wait()
		if _, ok := s.providerRecord(id); ok {
			t.Fatalf("并发删除后供应商 %s 仍然存在", id)
		}
		if _, err := creds.Get(id); !errors.Is(err, platform.ErrNotFound) {
			t.Fatalf("并发删除后供应商 %s 遗留孤立凭据", id)
		}
	}
}
