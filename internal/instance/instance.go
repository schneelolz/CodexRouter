// Package instance 只提供经过持有者凭据认证的“打开当前实例”控制。
package instance

import (
	"bytes"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"sync"
	"time"
)

// Path 是本地实例控制入口，不提供任意执行能力。
const Path = "/internal/instance/open"

type message struct {
	Nonce string `json:"nonce"`
	Proof string `json:"proof"`
}

func sign(key, action, nonce string) string {
	h := hmac.New(sha256.New, []byte(key))
	h.Write([]byte(action + ":" + nonce))
	return hex.EncodeToString(h.Sum(nil))
}

// Handler 只允许有密钥的同用户实例打开页面，带过期和防重放检查。
func Handler(key string, open func() error) http.Handler {
	var mu sync.Mutex
	seen := map[string]time.Time{}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" || r.Header.Get("Origin") != "" || r.URL.RawQuery != "" {
			http.Error(w, "拒绝外部控制", 403)
			return
		}
		var in message
		d := json.NewDecoder(io.LimitReader(r.Body, 2048))
		d.DisallowUnknownFields()
		if d.Decode(&in) != nil || d.Decode(new(any)) != io.EOF || len(in.Nonce) != 75 || len(in.Proof) != 64 {
			http.Error(w, "无效控制", 403)
			return
		}
		stamp, stampErr := strconv.ParseInt(in.Nonce[:10], 10, 64)
		if stampErr != nil || time.Now().Unix()-stamp > 30 || stamp-time.Now().Unix() > 5 {
			http.Error(w, "控制已过期", 403)
			return
		}
		expected := sign(key, "open", in.Nonce)
		if !hmac.Equal([]byte(in.Proof), []byte(expected)) {
			http.Error(w, "控制认证失败", 403)
			return
		}
		mu.Lock()
		now := time.Now()
		for n, t := range seen {
			if now.Sub(t) > time.Minute {
				delete(seen, n)
			}
		}
		if _, ok := seen[in.Nonce]; ok || len(seen) >= 64 {
			mu.Unlock()
			http.Error(w, "控制已过期", 403)
			return
		}
		seen[in.Nonce] = now
		mu.Unlock()
		if open == nil || open() != nil {
			http.Error(w, "打开失败", 503)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		_ = json.NewEncoder(w).Encode(message{in.Nonce, sign(key, "opened", in.Nonce)})
	})
}

// Open 向固定本机地址发送签名动作，只有验证响应后才视为已有可信实例。
func Open(base, key string) error {
	if len(key) < 32 {
		return errors.New("本地控制凭据无效")
	}
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return err
	}
	nonce := strconv.FormatInt(time.Now().Unix(), 10) + "-" + hex.EncodeToString(b)
	body, _ := json.Marshal(message{nonce, sign(key, "open", nonce)})
	req, err := http.NewRequest("POST", base+Path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: 3 * time.Second, Transport: &http.Transport{Proxy: nil}, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	defer client.CloseIdleConnections()
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	var result message
	if resp.StatusCode != 200 || json.NewDecoder(io.LimitReader(resp.Body, 2048)).Decode(&result) != nil || result.Nonce != nonce || !hmac.Equal([]byte(result.Proof), []byte(sign(key, "opened", nonce))) {
		return errors.New("未能确认可信的已运行实例；没有更换端口")
	}
	return nil
}
