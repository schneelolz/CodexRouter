package router

import (
	"net/http"
	"strings"
)

// copyProtocolHeaders 保留白名单的多值语义，排除 Connection 声明的逐跳头。
func copyProtocolHeaders(dst, src http.Header, names ...string) {
	for _, name := range names {
		hop := false
		for _, value := range src.Values("Connection") {
			for _, token := range strings.Split(value, ",") {
				if strings.EqualFold(strings.TrimSpace(token), name) {
					hop = true
				}
			}
		}
		if hop {
			continue
		}
		values := src.Values(name)
		if len(values) > 0 {
			dst.Del(name)
			for _, value := range values {
				dst.Add(name, value)
			}
		}
	}
}
