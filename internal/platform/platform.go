// Package platform 隔离系统目录、浏览器和凭据能力。
package platform

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"regexp"
)

// ErrNotFound 表示指定凭据不存在。
var ErrNotFound = errors.New("凭据不存在")

// ErrUnsupported 表示当前平台尚未实现凭据后端。
var ErrUnsupported = errors.New("当前平台尚未实现安全凭据存储")

// Credentials 提供受操作系统保护的凭据存储，不提供明文回退。
type Credentials interface {
	// Get 读取指定名称的凭据；不存在时返回 ErrNotFound。
	Get(name string) (string, error)
	// Set 保存或替换凭据，只通过操作系统安全后端持久化。
	Set(name, value string) error
	// Delete 删除凭据；重复删除保持幂等。
	Delete(name string) error
}

// Message 显示本机运行提示；Windows在初始化时安装原生实现。
func Message(title, text string, isError bool) { messageImpl(title, text, isError) }

// Tray 运行托盘循环；非Windows明确返回不支持。
func Tray(ctx context.Context, open func() error, quit func()) error {
	return trayImpl(ctx, open, quit)
}

var messageImpl = func(title, text string, _ bool) { fmt.Fprintln(os.Stderr, title+": "+text) }
var trayImpl = func(context.Context, func() error, func()) error { return ErrUnsupported }

var credentialName = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,64}$`)

func validateCredentialName(name string) error {
	if !credentialName.MatchString(name) {
		return errors.New("凭据名称不合法")
	}
	return nil
}

func validateBrowserURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil {
		return errors.New("浏览器地址必须是本机 HTTP 或 HTTPS 地址")
	}
	host := u.Hostname()
	if host != "localhost" {
		ip := net.ParseIP(host)
		if ip == nil || !ip.IsLoopback() {
			return fmt.Errorf("浏览器地址必须位于 loopback")
		}
	}
	return nil
}
