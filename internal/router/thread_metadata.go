package router

import (
	"encoding/json"
	"errors"
	"io"
	"strings"
)

// parseThreadMetadata 拒绝重复语义字段与非字符串值，未知字段仍兼容。
func parseThreadMetadata(raw string) (threadEvidence, error) {
	var e threadEvidence
	invalid := errors.New("SESSION_METADATA_INVALID")
	if len(raw) > 8192 {
		return e, invalid
	}
	d := json.NewDecoder(strings.NewReader(raw))
	if token, err := d.Token(); err != nil || token != json.Delim('{') {
		return e, invalid
	}
	fields := map[string]*string{"thread_id": &e.ThreadID, "session_id": &e.SessionID, "history_initialization": &e.History, "forked_from_thread_id": &e.Parent}
	seen := make(map[string]bool)
	for d.More() {
		token, err := d.Token()
		if err != nil {
			return e, invalid
		}
		name, ok := token.(string)
		if !ok {
			return e, invalid
		}
		var value json.RawMessage
		if err = d.Decode(&value); err != nil {
			return e, invalid
		}
		if field, known := fields[name]; known {
			if seen[name] || len(value) == 0 || value[0] != '"' || json.Unmarshal(value, field) != nil || *field == "" {
				return e, invalid
			}
			seen[name] = true
		} else {
			// Go 的结构体 JSON 解码接受大小写别名；拒绝此类身份歧义。
			if _, alias := fields[strings.ToLower(name)]; alias {
				return e, invalid
			}
		}
	}
	if token, err := d.Token(); err != nil || token != json.Delim('}') {
		return e, invalid
	}
	if d.Decode(new(any)) != io.EOF {
		return e, invalid
	}
	return e, nil
}
