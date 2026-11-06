package router

import (
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"testing"

	"codexrouter/internal/api"
)

type contractDocument struct {
	Paths map[string]map[string]struct {
		Responses map[string]struct {
			Content map[string]struct {
				Schema map[string]any `json:"schema"`
			} `json:"content"`
		} `json:"responses"`
	} `json:"paths"`
	Components struct {
		Schemas map[string]map[string]any `json:"schemas"`
	} `json:"components"`
}

func validateSchema(doc contractDocument, schema map[string]any, value any, path string) error {
	if ref, ok := schema["$ref"].(string); ok {
		name := strings.TrimPrefix(ref, "#/components/schemas/")
		s, ok := doc.Components.Schemas[name]
		if !ok {
			return fmt.Errorf("未知 ref %s", ref)
		}
		return validateSchema(doc, s, value, path)
	}
	if enums, ok := schema["enum"].([]any); ok {
		found := false
		for _, v := range enums {
			if fmt.Sprint(v) == fmt.Sprint(value) {
				found = true
			}
		}
		if !found {
			return fmt.Errorf("%s 不满足枚举", path)
		}
	}
	switch schema["type"] {
	case "object":
		obj, ok := value.(map[string]any)
		if !ok {
			return fmt.Errorf("%s 应为object", path)
		}
		if required, ok := schema["required"].([]any); ok {
			for _, key := range required {
				if _, exists := obj[key.(string)]; !exists {
					return fmt.Errorf("%s 缺少 %s", path, key)
				}
			}
		}
		props, _ := schema["properties"].(map[string]any)
		for k, v := range obj {
			child, exists := props[k]
			if !exists {
				if schema["additionalProperties"] == false {
					return fmt.Errorf("%s 出现未定义字段 %s", path, k)
				}
				continue
			}
			if err := validateSchema(doc, child.(map[string]any), v, path+"."+k); err != nil {
				return err
			}
		}
	case "array":
		arr, ok := value.([]any)
		if !ok {
			return fmt.Errorf("%s 应为array", path)
		}
		for i, v := range arr {
			if err := validateSchema(doc, schema["items"].(map[string]any), v, fmt.Sprintf("%s[%d]", path, i)); err != nil {
				return err
			}
		}
	case "string":
		v, ok := value.(string)
		if !ok {
			return fmt.Errorf("%s 应为string", path)
		}
		if n, ok := schema["minLength"].(float64); ok && float64(len([]rune(v))) < n {
			return fmt.Errorf("%s 过短", path)
		}
		if n, ok := schema["maxLength"].(float64); ok && float64(len([]rune(v))) > n {
			return fmt.Errorf("%s 过长", path)
		}
		if pattern, ok := schema["pattern"].(string); ok && !regexp.MustCompile(pattern).MatchString(v) {
			return fmt.Errorf("%s pattern失败", path)
		}
	case "integer", "number":
		v, ok := value.(float64)
		if !ok || (schema["type"] == "integer" && v != math.Trunc(v)) {
			return fmt.Errorf("%s 应为数字", path)
		}
		if n, ok := schema["minimum"].(float64); ok && v < n {
			return fmt.Errorf("%s 小于minimum", path)
		}
		if n, ok := schema["maximum"].(float64); ok && v > n {
			return fmt.Errorf("%s 大于maximum", path)
		}
	case "boolean":
		if _, ok := value.(bool); !ok {
			return fmt.Errorf("%s 应为bool", path)
		}
	default:
		return fmt.Errorf("schema类型未支持: %v", schema["type"])
	}
	return nil
}

func TestActualManagementResponsesMatchOpenAPI(t *testing.T) {
	data, err := os.ReadFile("../../contracts/openapi.json")
	if err != nil {
		t.Fatal(err)
	}
	var doc contractDocument
	if err = json.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	s, _, _ := newSecurityServer(t, nil)
	cookie, csrf := securityLogin(t, s)
	origin := "http://" + s.authority
	// 供应商标识由后端生成，先创建再用于路径模板用例。
	w := securityRequest(s, securityRequestOptions{method: http.MethodPost, path: "/api/v1/providers", body: `{"name":"合成供应商","kind":"api_key","base_url":"https://api.example.test/v1","model":"gpt-test"}`, origin: origin, csrf: csrf, cookie: cookie})
	if w.Code != http.StatusOK {
		t.Fatalf("创建供应商失败: %d %s", w.Code, w.Body.String())
	}
	var created api.Provider
	if err = json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	checkResponse := func(t *testing.T, schema map[string]any, body []byte, label string) {
		t.Helper()
		var value any
		if err := json.Unmarshal(body, &value); err != nil {
			t.Fatal(err)
		}
		if err := validateSchema(doc, schema, value, label); err != nil {
			t.Fatal(err)
		}
	}
	checkResponse(t, doc.Components.Schemas["Provider"], w.Body.Bytes(), "provider")
	_, _ = s.state.resolve("contract-pending")
	cfgInput, _ := json.Marshal(api.ConfigInput{DefaultProviderID: BuiltinOfficialID, ConnectTimeoutMS: 10000, HeaderTimeoutMS: 30000, IdleTimeoutMS: 120000, WriteTimeoutMS: 30000, MaxConcurrent: 16})
	models := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"data":[{"id":"synthetic-contract-model"}]}`))
	}))
	defer models.Close()
	relayBody, _ := json.Marshal(api.ProviderSaveInput{Name: "统一保存测试", BaseURL: models.URL, Key: "synthetic-contract-model-key"})
	modelBody, _ := json.Marshal(api.ModelQueryInput{BaseURL: models.URL, Key: "synthetic-contract-model-key"})
	cases := []struct{ method, path, contract, body string }{
		{"POST", "/api/v1/auth/renew", "/api/v1/auth/renew", ""},
		{"GET", "/api/v1/requests?page=1&filter=success", "/api/v1/requests", ""},
		{"GET", "/api/v1/activity", "/api/v1/activity", ""},
		{"GET", "/api/v1/requests", "/api/v1/requests", ""},
		{"GET", "/api/v1/sessions/page", "/api/v1/sessions/page", ""},
		{"GET", "/api/v1/history/policy", "/api/v1/history/policy", ""},
		{"PUT", "/api/v1/history/policy", "/api/v1/history/policy", `{"days":0}`},
		{"POST", "/api/v1/history/clear", "/api/v1/history/clear", `{"confirm":true}`},
		{"POST", "/api/v1/providers/save", "/api/v1/providers/save", string(relayBody)},
		{"POST", "/api/v1/providers/models", "/api/v1/providers/models", string(modelBody)},
		{"GET", "/api/v1/clients", "/api/v1/clients", ""},
		{"GET", "/api/v1/capabilities", "/api/v1/capabilities", ""},
		{"GET", "/api/v1/status", "/api/v1/status", ""},
		{"GET", "/api/v1/config", "/api/v1/config", ""},
		{"PUT", "/api/v1/config", "/api/v1/config", string(cfgInput)},
		{"GET", "/api/v1/providers", "/api/v1/providers", ""},
		{"PUT", "/api/v1/providers/" + created.ID, "/api/v1/providers/{providerId}", `{"name":"合成供应商","base_url":"https://api.example.test/v1","model":"gpt-test"}`},
		{"GET", "/api/v1/credentials", "/api/v1/credentials", ""},
		{"PUT", "/api/v1/credentials/" + created.ID, "/api/v1/credentials/{providerId}", `{"key":"synthetic-contract-key"}`},
		{"DELETE", "/api/v1/credentials/" + created.ID, "/api/v1/credentials/{providerId}", ""},
		{"GET", "/api/v1/sessions", "/api/v1/sessions", ""},
		{"POST", "/api/v1/sessions/register", "/api/v1/sessions/register", `{"id":"contract-pending","provider_id":"official"}`},
		{"GET", "/api/v1/diagnostics", "/api/v1/diagnostics", ""},
		{"GET", "/api/v1/auth/session", "/api/v1/auth/session", ""},
		{"DELETE", "/api/v1/providers/" + created.ID, "/api/v1/providers/{providerId}", ""},
	}
	for _, test := range cases {
		t.Run(test.method+" "+test.method+test.contract, func(t *testing.T) {
			w := securityRequest(s, securityRequestOptions{method: test.method, path: test.path, body: test.body, cookie: cookie, csrf: csrf, origin: origin})
			if w.Code != http.StatusOK {
				t.Fatalf("状态%d %s", w.Code, w.Body.String())
			}
			schema := doc.Paths[test.contract][strings.ToLower(test.method)].Responses["200"].Content["application/json"].Schema
			checkResponse(t, schema, w.Body.Bytes(), "response")
		})
	}
	w = securityRequest(s, securityRequestOptions{path: "/api/v1/config"})
	if w.Code != http.StatusUnauthorized {
		t.Fatal("无认证读取未返回401")
	}
	checkResponse(t, doc.Components.Schemas["ErrorResponse"], w.Body.Bytes(), "error")
}

func TestFreshInputCannotMistakeGeneratedHistoryForNew(t *testing.T) {
	for _, body := range []string{
		`{"input":[{"role":"user"},{"role":"assistant"},{"role":"user"}]}`,
		`{"previous_response_id":"resp_synthetic","input":[{"role":"user"}]}`,
		`{"input":[{"type":"function_call_output","call_id":"synthetic"}]}`,
		`{"input":[{"type":"compaction"},{"role":"user"}]}`,
		`{"input":[]}`, `{`,
	} {
		if hasFreshInput([]byte(body)) {
			t.Fatal("含生成历史或未知结构被识别为新建")
		}
	}
	if !hasFreshInput([]byte(`{"input":[{"type":"additional_tools","role":"developer","id":"synthetic-tools"},{"type":"message","role":"developer","id":"synthetic-context"},{"role":"user","id":"synthetic-environment"},{"role":"user","id":"synthetic-user"}]}`)) {
		t.Fatal("已观察的新建输入结构未识别")
	}
	// 即使元数据仍为 new，带历史的未知线程也必须登记。
	s, _, _ := newSecurityServer(t, nil)
	r := inferenceRequest(t, "http://"+s.authority, "history-marked-new", "new")
	r.URL.Scheme = ""
	r.URL.Host = ""
	r.Body = http.NoBody
	r.Body = io.NopCloser(strings.NewReader(`{"input":[{"role":"assistant"},{"role":"user"}]}`))
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	if w.Code != 409 || !strings.Contains(w.Body.String(), "SESSION_REGISTRATION_REQUIRED") {
		t.Fatal("带历史new线程未暂停登记")
	}
}
