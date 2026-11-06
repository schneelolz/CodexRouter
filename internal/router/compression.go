package router

import (
	"bytes"
	"io"
	"strings"

	"github.com/klauspost/compress/zstd"
)

const maxEvidenceBody = 16 << 20

// 只为首次会话证据解码；原始正文和编码头仍原样转发，不保存明文。
func decodeRequestEvidence(raw []byte, encodings []string) ([]byte, bool) {
	if len(raw) > maxEvidenceBody || len(encodings) > 1 {
		return nil, false
	}
	encoding := ""
	if len(encodings) == 1 {
		encoding = strings.ToLower(strings.TrimSpace(encodings[0]))
	}
	switch encoding {
	case "", "identity":
		return raw, true
	case "zstd":
		decoder, err := zstd.NewReader(bytes.NewReader(raw),
			zstd.WithDecoderConcurrency(1),
			zstd.WithDecoderLowmem(true),
			zstd.WithDecoderMaxMemory(maxEvidenceBody),
			zstd.WithDecoderMaxWindow(maxEvidenceBody),
		)
		if err != nil {
			return nil, false
		}
		defer decoder.Close()
		decoded, err := io.ReadAll(io.LimitReader(decoder, maxEvidenceBody+1))
		if err != nil || len(decoded) > maxEvidenceBody {
			return nil, false
		}
		return decoded, true
	default:
		// 不猜测或剥离未知、叠加的编码，无法核实来源时保持待登记。
		return nil, false
	}
}
