package router

import (
	"bytes"
	"compress/gzip"
	"encoding/binary"
	"encoding/json"
	"io"
	"mime"
	"net/http"
	"strings"
	"time"

	"codexrouter/internal/api"
	"github.com/klauspost/compress/zstd"
)

// 观察失败只降低记录可信度，不改变转发字节；单事件和总解压量都有上限。
const observationFrameLimit = 1 << 20
const observationDecodedLimit = 64 << 20

type responseObservation struct {
	started   time.Time          // 入口时间，用于首字计时。
	metrics   api.RequestMetrics // 已核实的用量和时间指标。
	model     string             // 响应报告的模型标识。
	kind      string             // 根据响应类型识别的请求类别。
	terminal  string             // 已观察到的终止结果。
	bareError bool               // 尚未被权威完成覆盖的裸错误。
	visible   bool               // 已经出现有效输出。
	lost      bool               // 解析不完整，不能据此确认成功。
	sse       bool               // 响应是否按SSE帧解析。
	line      []byte             // 当前有界行缓冲。
	data      []byte             // 当前事件的data字段。
	event     string             // SSE事件类型。
	frameSize int                // 当前帧累计字节，用于资源上限。
	skipFrame bool               // 超限后跳过剩余帧。
	decoded   int                // 当前请求已观察字节数。
	jsonBody  []byte             // 非流式响应的有界副本。
	pipe      *io.PipeWriter     // 压缩流观察管道，不连接上游。
	done      chan struct{}      // 解码结束信号，发布最终指标。
	arrived   time.Time          // 当前输入块到达时间，不包含统计处理或客户端写入时间。
	decodedAt time.Time          // 压缩观察器收到块的时间，由管道读取方更新。
}

func newResponseObservation(resp *http.Response, started time.Time, entry string) *responseObservation {
	o := &responseObservation{started: started, kind: "unknown", metrics: api.RequestMetrics{KnownFields: []string{}}}
	if strings.HasSuffix(entry, "/models") {
		o.kind = "auxiliary"
		return o
	}
	contentType, _, err := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if err != nil {
		o.lost = true
		return o
	}
	switch contentType {
	case "text/event-stream":
		o.sse = true
		o.kind = "stream"
	case "application/json":
		o.kind = "sync"
	default:
		o.lost = true
		return o
	}
	encodings := resp.Header.Values("Content-Encoding")
	if len(encodings) > 1 {
		o.lost = true
		return o
	}
	encoding := strings.ToLower(strings.TrimSpace(resp.Header.Get("Content-Encoding")))
	if encoding == "" || encoding == "identity" {
		return o
	}
	if encoding != "gzip" && encoding != "zstd" {
		o.lost = true
		return o
	}
	reader, writer := io.Pipe()
	o.pipe = writer
	o.done = make(chan struct{})
	// 管道只传递现有转发块；解码器不读取额外上游字节，也不拥有上游连接。
	go func() {
		defer close(o.done)
		defer reader.Close()
		var decoded io.Reader
		if encoding == "gzip" {
			gz, e := gzip.NewReader(&observationReader{reader: reader, observation: o})
			if e != nil {
				o.lost = true
				return
			}
			defer gz.Close()
			decoded = gz
		} else {
			zs, e := zstd.NewReader(&observationReader{reader: reader, observation: o}, zstd.WithDecoderConcurrency(1), zstd.WithDecoderLowmem(true), zstd.WithDecoderMaxMemory(16<<20), zstd.WithDecoderMaxWindow(16<<20))
			if e != nil {
				o.lost = true
				return
			}
			defer zs.Close()
			decoded = zs
		}
		buf := make([]byte, 32<<10)
		limited := io.LimitReader(decoded, observationDecodedLimit+1)
		for {
			n, e := limited.Read(buf)
			if n > 0 {
				o.arrived = o.decodedAt
				o.consume(buf[:n])
			}
			if e != nil {
				if e != io.EOF {
					o.lost = true
				}
				return
			}
		}
	}()
	return o
}

func (o *responseObservation) feed(p []byte) {
	o.feedAt(p, time.Now())
}

type observationReader struct {
	reader      io.Reader            // 解码器输入管道。
	observation *responseObservation // 仅解码协程写入的观察状态。
	remaining   int                  // 当前原始块尚未送入解码器的字节。
}

func (r *observationReader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if r.remaining == 0 {
		var header [12]byte
		if _, err := io.ReadFull(r.reader, header[:]); err != nil {
			return 0, err
		}
		r.observation.decodedAt = time.Unix(0, int64(binary.LittleEndian.Uint64(header[:8])))
		r.remaining = int(binary.LittleEndian.Uint32(header[8:]))
	}
	n, err := r.reader.Read(p[:min(len(p), r.remaining)])
	r.remaining -= n
	return n, err
}

func (o *responseObservation) feedAt(p []byte, arrived time.Time) {
	if len(p) == 0 {
		return
	}
	if o.pipe != nil {
		// 时间戳和块边界只用于内部观察，不进入客户端响应。
		var header [12]byte
		binary.LittleEndian.PutUint64(header[:8], uint64(arrived.UnixNano()))
		binary.LittleEndian.PutUint32(header[8:], uint32(len(p)))
		_, _ = o.pipe.Write(header[:])
		_, _ = o.pipe.Write(p)
		return
	}
	o.arrived = arrived
	o.consume(p)
}

func (o *responseObservation) consume(p []byte) {
	if o.kind == "unknown" || o.kind == "auxiliary" {
		return
	}
	o.decoded += len(p)
	if o.decoded > observationDecodedLimit {
		o.lost = true
		return
	}
	if !o.sse {
		if len(o.jsonBody)+len(p) > observationFrameLimit {
			o.lost = true
			o.jsonBody = nil
			return
		}
		if !o.lost {
			o.jsonBody = append(o.jsonBody, p...)
		}
		return
	}
	for _, b := range p {
		if b == '\n' {
			line := bytes.TrimSuffix(o.line, []byte{'\r'})
			if len(line) == 0 && !o.skipFrame {
				o.dispatch()
			} else if !o.skipFrame {
				o.readLine(line)
			}
			if len(line) == 0 {
				o.skipFrame = false
				o.frameSize = 0
				o.data = nil
				o.event = ""
			}
			o.line = o.line[:0]
			continue
		}
		o.frameSize++
		if o.frameSize > observationFrameLimit {
			o.skipFrame = true
			o.lost = true
			o.data = nil
		}
		// 跳过超大事件仍保留行是否为空的信息，避免其尾部被误当成新事件。
		if !o.skipFrame {
			o.line = append(o.line, b)
		} else if len(o.line) == 0 {
			o.line = append(o.line, 'x')
		}
	}
}

func (o *responseObservation) readLine(line []byte) {
	if len(line) == 0 || line[0] == ':' {
		return
	}
	field, value, _ := bytes.Cut(line, []byte{':'})
	value = bytes.TrimPrefix(value, []byte{' '})
	switch string(field) {
	case "event":
		o.event = string(value)
	case "data":
		if len(o.data) > 0 {
			o.data = append(o.data, '\n')
		}
		o.data = append(o.data, value...)
	}
}

func (o *responseObservation) dispatch() {
	if len(bytes.TrimSpace(o.data)) == 0 {
		return
	}
	if bytes.Equal(bytes.TrimSpace(o.data), []byte("[DONE]")) {
		return
	}
	o.observe(o.data, o.event)
}

func (o *responseObservation) finish() {
	if o.pipe != nil {
		_ = o.pipe.Close()
		<-o.done
	}
	if o.kind == "unknown" || o.kind == "auxiliary" {
		return
	}
	if o.sse {
		// 未经空行完成的尾事件不作为完成证据，避免截断 JSON 恰好可解析时假成功。
		if len(o.line) > 0 || len(o.data) > 0 {
			o.lost = true
		}
	} else if !o.lost {
		o.observe(o.jsonBody, "json")
	}
	o.line = nil
	o.data = nil
	o.jsonBody = nil
}

func (o *responseObservation) observe(data []byte, event string) {
	var root map[string]json.RawMessage
	if json.Unmarshal(data, &root) != nil || root == nil {
		o.lost = true
		return
	}
	if kind := jsonString(root["type"]); kind != "" {
		event = kind
	}
	response := root
	if nested, ok := root["response"]; ok {
		var inner map[string]json.RawMessage
		if json.Unmarshal(nested, &inner) != nil || inner == nil {
			o.lost = true
			return
		}
		response = inner
	}
	if model := jsonString(response["model"]); safeModel(model) {
		o.model = model
	}
	o.usage(response["usage"])
	if _, has := response["usage"]; !has && event != "json" {
		o.usage(root["usage"])
	}
	status := jsonString(response["status"])
	if status == "failed" || event == "response.failed" {
		o.protocolCompleted()
		o.terminal = "RESPONSE_FAILED"
		return
	}
	if status == "incomplete" || event == "response.incomplete" {
		o.protocolCompleted()
		o.terminal = "RESPONSE_INCOMPLETE"
		return
	}
	if status == "cancelled" || status == "canceled" || event == "response.cancelled" || event == "response.canceled" {
		o.protocolCompleted()
		o.terminal = "REQUEST_CANCELED"
		return
	}
	if event == "error" || (event == "json" && len(bytes.TrimSpace(root["error"])) > 0 && string(root["error"]) != "null") {
		o.bareError = true
		return
	}
	// 首字只认非空输出 delta（包含推理与工具参数），不认 created、心跳或错误。
	delta := jsonString(root["delta"])
	if delta != "" && (strings.HasSuffix(event, ".delta") && (strings.Contains(event, "refusal") || strings.Contains(event, "text") || strings.Contains(event, "reasoning") || strings.Contains(event, "function_call_arguments"))) {
		o.visible = true
		if !o.known("first_token_ms") {
			o.metrics.FirstTokenMS = max(0, int(o.arrived.Sub(o.started).Milliseconds()))
			o.mark("first_token_ms")
		}
	}
	var output []json.RawMessage
	_ = json.Unmarshal(response["output"], &output)
	if len(output) > 0 {
		o.visible = true
	}
	if event == "response.completed" || event == "response.done" || (event == "json" && (status == "completed" || jsonString(root["object"]) == "response.compaction")) {
		if status != "" && status != "completed" {
			o.lost = true
			return
		}
		// 明确失败终止不能被后续矛盾的完成事件覆盖；裸 error 可被权威完成覆盖。
		if o.terminal != "" && o.terminal != "RESPONSE_COMPLETED" {
			return
		}
		o.bareError = false
		o.protocolCompleted()
		if !o.visible && o.metrics.InputTokens == 0 && o.metrics.OutputTokens == 0 {
			o.terminal = "EMPTY_RESPONSE"
		} else {
			o.terminal = "RESPONSE_COMPLETED"
		}
	}
}

func (o *responseObservation) protocolCompleted() {
	if !o.known("protocol_completed_ms") {
		o.metrics.ProtocolCompletedMS = max(0, int(o.arrived.Sub(o.started).Milliseconds()))
		o.mark("protocol_completed_ms")
	}
}

func jsonString(raw json.RawMessage) string {
	var value string
	_ = json.Unmarshal(raw, &value)
	return value
}
func safeModel(value string) bool {
	if len(value) == 0 || len(value) > 128 {
		return false
	}
	for _, c := range value {
		if c <= ' ' || c > '~' {
			return false
		}
	}
	return true
}
func (o *responseObservation) known(key string) bool {
	for _, field := range o.metrics.KnownFields {
		if field == key {
			return true
		}
	}
	return false
}
func (o *responseObservation) mark(key string) {
	if !o.known(key) {
		o.metrics.KnownFields = append(o.metrics.KnownFields, key)
	}
}

func metricNumber(raw json.RawMessage) (int, bool) {
	var n json.Number
	if len(raw) == 0 || raw[0] == '"' || json.Unmarshal(raw, &n) != nil {
		return 0, false
	}
	v, err := n.Int64()
	if err != nil || v < 0 || v > 9007199254740991 {
		return 0, false
	}
	return int(v), true
}

func (o *responseObservation) usage(raw json.RawMessage) {
	var usage map[string]json.RawMessage
	if len(raw) == 0 || json.Unmarshal(raw, &usage) != nil || usage == nil {
		return
	}
	var inputDetails, outputDetails map[string]json.RawMessage
	_ = json.Unmarshal(usage["input_tokens_details"], &inputDetails)
	_ = json.Unmarshal(usage["output_tokens_details"], &outputDetails)
	type candidate struct {
		key    string
		raw    json.RawMessage
		target *int
	}
	values := []candidate{
		{"input_tokens", usage["input_tokens"], &o.metrics.InputTokens}, {"output_tokens", usage["output_tokens"], &o.metrics.OutputTokens},
		{"total_tokens", usage["total_tokens"], &o.metrics.TotalTokens}, {"cache_read_tokens", inputDetails["cached_tokens"], &o.metrics.CacheReadTokens},
		{"cache_write_tokens", inputDetails["cache_write_tokens"], &o.metrics.CacheWriteTokens}, {"reasoning_tokens", outputDetails["reasoning_tokens"], &o.metrics.ReasoningTokens},
	}
	// 兼容 Responses 中常见的显式缓存写入字段，标准嵌套字段优先（包括0）。
	if len(values[4].raw) == 0 {
		values[4].raw = inputDetails["cache_creation_tokens"]
	}
	if len(values[4].raw) == 0 {
		values[4].raw = usage["cache_creation_input_tokens"]
	}
	if len(values[3].raw) == 0 {
		values[3].raw = usage["cache_read_input_tokens"]
	}
	positive := false
	for _, c := range values {
		if v, ok := metricNumber(c.raw); ok && v > 0 {
			positive = true
		}
	}
	// 有些网关终止事件用全0占位，不能覆盖此前已报告的真实快照。
	if !positive && (o.metrics.InputTokens > 0 || o.metrics.OutputTokens > 0) {
		return
	}
	_, inputKnown := metricNumber(usage["input_tokens"])
	_, outputKnown := metricNumber(usage["output_tokens"])
	if inputKnown && outputKnown {
		// 完整快照替换旧快照；未提供的缓存/推理不能沿用上一份的值。
		first, hasFirst := o.metrics.FirstTokenMS, o.known("first_token_ms")
		completed, hasCompleted := o.metrics.ProtocolCompletedMS, o.known("protocol_completed_ms")
		o.metrics = api.RequestMetrics{KnownFields: []string{}}
		if hasFirst {
			o.metrics.FirstTokenMS = first
			o.mark("first_token_ms")
		}
		if hasCompleted {
			o.metrics.ProtocolCompletedMS = completed
			o.mark("protocol_completed_ms")
		}
	}
	for _, c := range values {
		if v, ok := metricNumber(c.raw); ok {
			*c.target = v
			o.mark(c.key)
		}
	}
	if len(usage["total_tokens"]) == 0 && o.known("input_tokens") && o.known("output_tokens") && o.metrics.InputTokens <= 9007199254740991-o.metrics.OutputTokens {
		o.metrics.TotalTokens = o.metrics.InputTokens + o.metrics.OutputTokens
		o.mark("total_tokens")
	}
}

func (o *responseObservation) apply(item *api.RequestActivity) {
	item.Model = o.model
	item.RequestType = o.kind
	item.Metrics = o.metrics
	item.Observation = api.RequestObservation{TerminalResult: o.terminal, MetricsStatus: "partial"}
	if item.Observation.TerminalResult == "" {
		item.Observation.TerminalResult = "unknown"
	}
	switch {
	case o.kind == "auxiliary":
		item.Observation.MetricsStatus = "not_applicable"
	case o.lost:
		item.Observation.MetricsStatus = "unavailable"
	case o.known("input_tokens") && o.known("output_tokens") && (!o.sse || o.known("first_token_ms")):
		item.Observation.MetricsStatus = "available"
	case !o.known("input_tokens") && !o.known("output_tokens") && !o.known("first_token_ms"):
		item.Observation.MetricsStatus = "not_reported"
	}
	// 协议失败与随后客户端关闭分开记录，失败事件中的用量仍可用。
	if item.Status >= 200 && item.Status < 300 && o.terminal != "" && o.terminal != "RESPONSE_COMPLETED" {
		item.Outcome = "error"
		item.Result = o.terminal
		item.ErrorStage = "stream"
		return
	}
	if item.Outcome == "error" || o.kind == "auxiliary" {
		return
	}
	switch {
	case o.terminal != "" && o.terminal != "RESPONSE_COMPLETED":
		item.Outcome = "error"
		item.Result = o.terminal
		item.ErrorStage = "stream"
	case o.bareError:
		item.Outcome = "error"
		item.Result = "RESPONSE_FAILED"
		item.ErrorStage = "stream"
	case o.lost:
		item.Result = "OBSERVATION_UNAVAILABLE"
		item.ErrorStage = "observation"
	case o.terminal == "RESPONSE_COMPLETED":
		item.Outcome = "success"
		item.Result = o.terminal
	case o.sse:
		item.Outcome = "error"
		item.Result = "MISSING_TERMINAL"
		item.ErrorStage = "stream"
	default:
		item.Result = "RESULT_UNCONFIRMED"
	}
}
