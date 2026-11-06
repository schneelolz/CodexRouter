package router

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"sort"
	"strings"
	"time"

	"codexrouter/internal/api"
	"codexrouter/internal/platform"
)

func validRelayKey(key string) bool {
	if len(key) == 0 || len(key) > 2560 {
		return false
	}
	for _, r := range key {
		if r <= 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}
func sameBase(a, b string) bool { return strings.TrimRight(a, "/") == strings.TrimRight(b, "/") }

// saveRelayProvider 在同一状态锁中保存信息和凭据；状态写失败时补偿恢复凭据。
// 不调用已有分步API，避免新建产生半条供应商或编辑后信息与密钥不一致。
func (s *state) saveRelayProvider(in api.ProviderSaveInput) (api.Provider, error) {
	in.Name = strings.TrimSpace(in.Name)
	in.BaseURL = strings.TrimSpace(in.BaseURL)
	in.Model = strings.TrimSpace(in.Model)
	in.Key = strings.TrimSpace(in.Key)
	if validateBaseURL(in.BaseURL, false) != nil || validateProviderText(in.Name, 64) != nil || !modelPattern.MatchString(in.Model) || (in.Key != "" && !validRelayKey(in.Key)) {
		return api.Provider{}, errors.New("INVALID_INPUT")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.creds == nil {
		return api.Provider{}, errors.New("CREDENTIAL_UNAVAILABLE")
	}
	index := -1
	rec := providerRecord{Kind: api.ProviderKindAPIKey, Name: in.Name, BaseURL: in.BaseURL, Model: in.Model, CreatedAt: time.Now().UTC().Format(time.RFC3339)}
	if in.ProviderID == "" {
		if len(s.disk.Providers) >= maxProviders {
			return api.Provider{}, errors.New("PROVIDER_LIMIT")
		}
		if in.Key == "" {
			return api.Provider{}, errors.New("API_KEY_REQUIRED")
		}
		id, err := newProviderID(s.disk.Providers)
		if err != nil {
			return api.Provider{}, err
		}
		rec.ID = id
	} else {
		for i, p := range s.disk.Providers {
			if p.ID == in.ProviderID {
				index = i
				rec.ID = p.ID
				rec.CreatedAt = p.CreatedAt
				if p.Kind != api.ProviderKindAPIKey {
					return api.Provider{}, errors.New("INVALID_INPUT")
				}
				break
			}
		}
		if index < 0 {
			return api.Provider{}, errors.New("PROVIDER_NOT_FOUND")
		}
		old := s.disk.Providers[index]
		if bindingCounts(s.disk.Bindings)[rec.ID] > 0 && old.BaseURL != rec.BaseURL {
			return api.Provider{}, errors.New("PROVIDER_LOCKED")
		}
		if !sameBase(old.BaseURL, rec.BaseURL) && in.Key == "" {
			return api.Provider{}, errors.New("MODEL_QUERY_KEY_REQUIRED")
		}
	}
	previous, err := s.creds.Get(rec.ID)
	hadKey := err == nil && previous != ""
	if err != nil && !errors.Is(err, platform.ErrNotFound) {
		return api.Provider{}, errors.New("CREDENTIAL_UNAVAILABLE")
	}
	if !hadKey && in.Key == "" {
		return api.Provider{}, errors.New("API_KEY_REQUIRED")
	}
	next := s.disk
	next.Providers = append([]providerRecord{}, s.disk.Providers...)
	if index < 0 {
		next.Providers = append(next.Providers, rec)
	} else {
		next.Providers[index] = rec
	}
	if in.Key != "" {
		if err = s.creds.Set(rec.ID, in.Key); err != nil {
			return api.Provider{}, errors.New("CREDENTIAL_UNAVAILABLE")
		}
	}
	if err = s.store.Write("state.json", next); err != nil {
		if in.Key != "" {
			if hadKey {
				err = s.creds.Set(rec.ID, previous)
			} else {
				err = s.creds.Delete(rec.ID)
				if errors.Is(err, platform.ErrNotFound) {
					err = nil
				}
			}
			if err != nil {
				return api.Provider{}, errors.New("PROVIDER_SAVE_ROLLBACK_FAILED")
			}
		}
		return api.Provider{}, errors.New("STORAGE_WRITE_FAILED")
	}
	s.disk = next
	return api.Provider{ID: rec.ID, Name: rec.Name, Kind: rec.Kind, BaseURL: rec.BaseURL, Model: rec.Model, KeyConfigured: true, UpstreamLocked: bindingCounts(next.Bindings)[rec.ID] > 0}, nil
}

// modelQueryKey 只有基址仍与已保存供应商完全相同时，才允许复用已保存密钥。
func (s *state) modelQueryKey(in api.ModelQueryInput) (string, error) {
	in.Key = strings.TrimSpace(in.Key)
	if validateBaseURL(in.BaseURL, false) != nil || (in.Key != "" && !validRelayKey(in.Key)) {
		return "", errors.New("INVALID_INPUT")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if in.ProviderID != "" {
		rec, ok := findProvider(s.disk.Providers, in.ProviderID)
		if !ok {
			return "", errors.New("PROVIDER_NOT_FOUND")
		}
		if rec.Kind != api.ProviderKindAPIKey {
			return "", errors.New("INVALID_INPUT")
		}
		if in.Key == "" {
			if !sameBase(rec.BaseURL, in.BaseURL) {
				return "", errors.New("MODEL_QUERY_KEY_REQUIRED")
			}
			if s.creds == nil {
				return "", errors.New("CREDENTIAL_UNAVAILABLE")
			}
			value, err := s.creds.Get(rec.ID)
			if err != nil || value == "" {
				if errors.Is(err, platform.ErrNotFound) || err == nil {
					return "", errors.New("API_KEY_REQUIRED")
				}
				return "", errors.New("CREDENTIAL_UNAVAILABLE")
			}
			return value, nil
		}
	}
	if in.Key == "" {
		return "", errors.New("API_KEY_REQUIRED")
	}
	return in.Key, nil
}

// fetchRelayModels 不转发客户端身份，不跟随重定向；只返回有界、去重后的模型ID。
func fetchRelayModels(ctx context.Context, base, key string) (api.Models, error) {
	ctx, cancel := context.WithTimeout(ctx, 6*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(base, "/")+"/models", nil)
	if err != nil {
		return api.Models{}, errors.New("INVALID_INPUT")
	}
	request.Header.Set("Authorization", "Bearer "+key)
	request.Header.Set("Accept", "application/json")
	request.Header.Set("User-Agent", "CodexRouter/ModelList")
	transport := &http.Transport{Proxy: nil, DialContext: (&net.Dialer{Timeout: 3 * time.Second}).DialContext, TLSHandshakeTimeout: 3 * time.Second, ResponseHeaderTimeout: 4 * time.Second, MaxResponseHeaderBytes: 32 << 10, DisableCompression: true}
	defer transport.CloseIdleConnections()
	client := http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	response, err := client.Do(request)
	if err != nil {
		if ctx.Err() != nil {
			return api.Models{}, errors.New("MODEL_QUERY_TIMEOUT")
		}
		return api.Models{}, errors.New("MODEL_QUERY_UNREACHABLE")
	}
	defer response.Body.Close()
	switch response.StatusCode {
	case 401, 403:
		return api.Models{}, errors.New("MODEL_QUERY_UNAUTHORIZED")
	case 404, 405, 501:
		return api.Models{}, errors.New("MODEL_QUERY_UNSUPPORTED")
	}
	if response.StatusCode >= 300 && response.StatusCode < 400 {
		return api.Models{}, errors.New("MODEL_QUERY_REDIRECT")
	}
	if response.StatusCode != 200 {
		return api.Models{}, errors.New("MODEL_QUERY_UPSTREAM_FAILED")
	}
	raw, err := io.ReadAll(io.LimitReader(response.Body, (1<<20)+1))
	if err != nil {
		return api.Models{}, errors.New("MODEL_QUERY_INVALID_RESPONSE")
	}
	if len(raw) > 1<<20 {
		return api.Models{}, errors.New("MODEL_QUERY_TOO_LARGE")
	}
	var payload struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
		Models []struct {
			ID string `json:"id"`
		} `json:"models"`
	}
	if json.Unmarshal(raw, &payload) != nil {
		return api.Models{}, errors.New("MODEL_QUERY_INVALID_RESPONSE")
	}
	if payload.Data == nil && payload.Models == nil {
		return api.Models{}, errors.New("MODEL_QUERY_INVALID_RESPONSE")
	}
	if payload.Data == nil {
		payload.Data = payload.Models
	}
	if len(payload.Data) > 1000 {
		return api.Models{}, errors.New("MODEL_QUERY_TOO_LARGE")
	}
	result := api.Models{Items: []api.ModelItem{}}
	seen := map[string]bool{}
	for _, item := range payload.Data {
		if item.ID == "" || !modelPattern.MatchString(item.ID) {
			continue
		}
		if !seen[item.ID] {
			seen[item.ID] = true
			result.Items = append(result.Items, api.ModelItem{ID: item.ID})
		}
	}
	sort.Slice(result.Items, func(i, j int) bool { return result.Items[i].ID < result.Items[j].ID })
	return result, nil
}

func (s *Server) manageRelaySave(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		fail(w, 405, "METHOD_NOT_ALLOWED", "供应商保存仅支持POST")
		return
	}
	var in api.ProviderSaveInput
	if !decode(w, r, &in) {
		return
	}
	result, err := s.state.saveRelayProvider(in)
	if err != nil {
		code := err.Error()
		switch code {
		case "API_KEY_REQUIRED":
			fail(w, 409, code, "请输入该中转供应商的API Key")
		case "MODEL_QUERY_KEY_REQUIRED":
			fail(w, 409, code, "地址已改变，请重新输入该地址的API Key")
		case "PROVIDER_SAVE_ROLLBACK_FAILED":
			fail(w, 500, code, "信息未保存，系统凭据未能恢复；请重新录入该供应商密钥，勿直接重试新建")
		default:
			status, code, message := providerError(err)
			fail(w, status, code, message)
		}
		return
	}
	respond(w, 200, result)
}
func (s *Server) manageRelayModels(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		fail(w, 405, "METHOD_NOT_ALLOWED", "模型查询仅支持POST")
		return
	}
	var in api.ModelQueryInput
	if !decode(w, r, &in) {
		return
	}
	in.BaseURL = strings.TrimSpace(in.BaseURL)
	// 使用独立并发计数，查询不会挤占生成请求，且最多同时两条。
	if s.modelQueries.Add(1) > 2 {
		s.modelQueries.Add(-1)
		fail(w, 503, "MODEL_QUERY_BUSY", "模型查询较忙，请稍后再试")
		return
	}
	defer s.modelQueries.Add(-1)
	key, err := s.state.modelQueryKey(in)
	if err == nil {
		var result api.Models
		queryContext, cancel := context.WithCancel(r.Context())
		stop := context.AfterFunc(s.ctx, cancel)
		defer stop()
		defer cancel()
		result, err = fetchRelayModels(queryContext, in.BaseURL, key)
		if err == nil {
			respond(w, 200, result)
			return
		}
	}
	code := err.Error()
	status := 502
	message := "拉取模型失败，可手动填写模型后保存"
	switch code {
	case "INVALID_INPUT":
		status = 400
		message = "请检查BaseUrl和API Key，地址不能包含凭据、查询参数或片段"
	case "PROVIDER_NOT_FOUND":
		status = 404
		message = "供应商不存在，请刷新后重试"
	case "API_KEY_REQUIRED", "MODEL_QUERY_KEY_REQUIRED":
		status = 409
		message = "请输入API Key；地址改变后不能使用原地址的已存密钥"
	case "CREDENTIAL_UNAVAILABLE":
		status = 503
		message = "系统凭据存储不可用"
	case "MODEL_QUERY_UNAUTHORIZED":
		message = "中转服务拒绝认证，请检查API Key或访问权限"
	case "MODEL_QUERY_UNSUPPORTED":
		message = "该地址不提供模型列表，可手动填写模型后保存"
	case "MODEL_QUERY_REDIRECT":
		message = "模型接口返回重定向；为保护密钥没有跳转，请填写最终API基址"
	case "MODEL_QUERY_TIMEOUT":
		message = "拉取模型超时，请稍后重试或手动填写"
	case "MODEL_QUERY_UNREACHABLE":
		message = "无法连接该中转地址，请检查地址和服务状态"
	case "MODEL_QUERY_TOO_LARGE":
		message = "模型列表超过显示上限，请手动填写模型"
	case "MODEL_QUERY_INVALID_RESPONSE":
		message = "接口返回的内容不是支持的模型列表，可手动填写模型"
	}
	fail(w, status, code, message)
}
