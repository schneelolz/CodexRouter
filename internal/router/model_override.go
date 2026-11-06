package router

import (
	"bytes"
	"encoding/json"
	"errors"
)

// 模型覆盖的稳定失败原因，供转发层映射为明确错误码。
var (
	errModelBodyTooLarge        = errors.New("模型覆盖后的正文超过上限")
	errModelEncodingUnsupported = errors.New("正文编码不受支持")
	errModelInvalidBody         = errors.New("正文不是 JSON 对象")
)

// applyModelOverride 只在保留未知字段的前提下替换顶层 model。
// 需要完整解码正文（支持 identity 与单层 zstd）；重写后按未压缩 JSON 发送，
// 因此调用方必须移除 Content-Encoding。键顺序与重复键会被 JSON 规范化，值保持原样。
func applyModelOverride(raw []byte, encodings []string, model string) ([]byte, error) {
	if len(raw) > maxEvidenceBody {
		return nil, errModelBodyTooLarge
	}
	decoded, ok := decodeRequestEvidence(raw, encodings)
	if !ok {
		return nil, errModelEncodingUnsupported
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(decoded, &fields); err != nil || fields == nil {
		return nil, errModelInvalidBody
	}
	encoded, err := json.Marshal(model)
	if err != nil {
		return nil, errModelInvalidBody
	}
	fields["model"] = encoded
	var buf bytes.Buffer
	encoder := json.NewEncoder(&buf)
	// 不做 HTML 转义，避免改写字符串值中的 <、>、&。
	encoder.SetEscapeHTML(false)
	if err = encoder.Encode(fields); err != nil {
		return nil, errModelInvalidBody
	}
	out := bytes.TrimSuffix(buf.Bytes(), []byte("\n"))
	if len(out) > maxEvidenceBody {
		return nil, errModelBodyTooLarge
	}
	return out, nil
}
