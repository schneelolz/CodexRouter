package clients

import (
	"errors"
	"reflect"

	"github.com/pelletier/go-toml/v2"
)

// mergeConfig 只检查和修改接入字段，保留桌面客户端新增的信任、模型等设置。
// TOML经结构化解析后重写会改变排版和注释，变更前始终保留专用配置备份。
func (m *Manager) mergeConfig(current []byte, target string, meta managedFile, nextMode string) ([]byte, error) {
	if len(current) > 1<<20 {
		return nil, errors.New("PROFILE_CONFLICT")
	}
	var doc, expected map[string]any
	if toml.Unmarshal(current, &doc) != nil || toml.Unmarshal(m.config(target, meta.Mode), &expected) != nil {
		return nil, errors.New("PROFILE_CONFLICT")
	}
	if meta.Mode == "routed" && meta.BaseURL != "" {
		expected["openai_base_url"] = meta.BaseURL
	}
	for _, key := range []string{"model_provider", "cli_auth_credentials_store", "web_search", "openai_base_url"} {
		if !reflect.DeepEqual(doc[key], expected[key]) {
			return nil, errors.New("PROFILE_CONFLICT")
		}
	}
	for _, key := range []string{"analytics", "feedback"} {
		actual, ok := doc[key].(map[string]any)
		want := expected[key].(map[string]any)
		if !ok || !reflect.DeepEqual(actual["enabled"], want["enabled"]) {
			return nil, errors.New("PROFILE_CONFLICT")
		}
	}
	if nextMode == meta.Mode && (nextMode == "direct" || expected["openai_base_url"] == m.base) {
		return current, nil
	}
	if nextMode == "direct" {
		delete(doc, "openai_base_url")
	} else {
		doc["openai_base_url"] = m.base
	}
	out, err := toml.Marshal(doc)
	if err != nil {
		return nil, errors.New("PROFILE_CONFLICT")
	}
	return out, nil
}
