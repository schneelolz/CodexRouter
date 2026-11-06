// Package clients 管理由路由器创建的独立客户端，不读写日常 Codex 配置。
package clients

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Client 是可公开显示的本机客户端状态，不含认证内容。
type Client struct {
	Target    string `json:"target"`
	Installed bool   `json:"installed"`
	Version   string `json:"version"`
	State     string `json:"state"`
	Mode      string `json:"mode"`
	Profile   string `json:"profile"`
	Message   string `json:"message"`
}

// Status 汇总专用客户端状态。
type Status struct {
	Items []Client `json:"items"`
}

// Input 只接受固定目标与动作，不接受任意命令或路径。
type Input struct {
	Target string `json:"target"`
	Action string `json:"action"`
}

type managedFile struct {
	Version int    `json:"version"`
	Mode    string `json:"mode"`
	SHA256  string `json:"sha256"`
	BaseURL string `json:"base_url"`
}
type installation struct{ Path, Version string }
type child struct {
	cmd      *exec.Cmd
	closeJob func()
	done     chan struct{}
	action   string
}

// Manager 只管理当前进程创建的客户端及其专用目录。
type Manager struct {
	mu                 sync.Mutex
	root, base, helper string
	installed          map[string]installation
	active             map[string]*child
	last               map[string]string
	closed             bool
}

// 平台实现于初始化时安装；缺少平台适配时明确拒绝启动客户端。
var detect = func() map[string]installation { return map[string]installation{} }
var detectCLI = func() installation { return detect()["cli"] }
var startOwned = func(*exec.Cmd, bool) (func(), error) { return nil, errors.New("当前平台未实现客户端启动") }
var openConsole = func() error { return errors.New("当前平台未实现客户端窗口") }
var hideCommand = func(*exec.Cmd) {}
var replace = os.Rename

// New 创建专用客户端管理器；base 必须是本机 HTTP 基址。
func New(root, base string) (*Manager, error) {
	u, err := url.Parse(base)
	if err != nil || u.Scheme != "http" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" || net.ParseIP(u.Hostname()) == nil || !net.ParseIP(u.Hostname()).IsLoopback() || u.Port() == "" {
		return nil, errors.New("本机客户端地址无效")
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	if err = os.MkdirAll(abs, 0700); err != nil {
		return nil, err
	}
	if err = safeDirectory(abs); err != nil {
		return nil, err
	}
	helper, err := os.Executable()
	if err != nil {
		return nil, err
	}
	m := &Manager{root: abs, base: base, helper: helper, installed: detect(), active: map[string]*child{}, last: map[string]string{}}
	return m, nil
}
func digest(b []byte) string                    { sum := sha256.Sum256(b); return hex.EncodeToString(sum[:]) }
func (m *Manager) profile(target string) string { return filepath.Join(m.root, target) }
func (m *Manager) config(target, mode string) []byte {
	text := "# CodexRouter managed test profile. Do not edit while managed.\nmodel_provider = \"openai\"\ncli_auth_credentials_store = \"file\"\nweb_search = \"disabled\"\n"
	if mode == "routed" {
		text += fmt.Sprintf("openai_base_url = %q\n", m.base)
	}
	text += "[analytics]\nenabled = false\n[feedback]\nenabled = false\n"
	return []byte(text)
}
func (m *Manager) ensureProfile(target, requested string) (string, error) {
	root := m.profile(target)
	for _, p := range []string{root, filepath.Join(root, "codex-home"), filepath.Join(root, "work"), filepath.Join(root, "home"), filepath.Join(root, "user-data")} {
		if err := safeDirectory(p); err != nil {
			return "", err
		}
	}
	config := filepath.Join(root, "codex-home", "config.toml")
	record := filepath.Join(root, "managed-profile.json")
	if err := regularFile(record); err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	raw, err := os.ReadFile(record)
	if errors.Is(err, os.ErrNotExist) {
		if _, err = os.Lstat(config); err == nil {
			return "", errors.New("PROFILE_CONFLICT")
		}
		if !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		mode := "routed"
		if requested != "" {
			mode = requested
		}
		b := m.config(target, mode)
		file, err := os.OpenFile(config, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if err != nil {
			return "", err
		}
		_, err = file.Write(b)
		closeErr := file.Close()
		if err != nil {
			return "", err
		}
		if closeErr != nil {
			return "", closeErr
		}
		rec, _ := json.Marshal(managedFile{1, mode, digest(b), m.base})
		if err = replaceFile(record, rec); err != nil {
			_ = os.Remove(config)
			return "", err
		}
		return mode, nil
	}
	if err != nil {
		return "", err
	}
	var meta managedFile
	if json.Unmarshal(raw, &meta) != nil || meta.Version != 1 || (meta.Mode != "direct" && meta.Mode != "routed") {
		return "", errors.New("PROFILE_CONFLICT")
	}
	if err = regularFile(config); err != nil {
		return "", err
	}
	current, err := os.ReadFile(config)
	if err != nil {
		return "", err
	}
	if requested == "" {
		requested = meta.Mode
	}
	updated, err := m.mergeConfig(current, target, meta, requested)
	if err != nil {
		return "", err
	}
	if string(updated) == string(current) {
		return requested, nil
	}
	// 配置变更前仅备份本程序生成的配置，不复制任何认证文件。
	if err = replaceFile(filepath.Join(root, "config.previous.toml"), current); err != nil {
		return "", err
	}
	if err = replaceFile(config, updated); err != nil {
		return "", err
	}
	rec, _ := json.Marshal(managedFile{1, requested, digest(updated), m.base})
	if err = replaceFile(record, rec); err != nil {
		_ = replaceFile(config, current)
		return "", err
	}
	return requested, nil
}
func safeDirectory(p string) error {
	if info, err := os.Lstat(p); err == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return errors.New("PROFILE_CONFLICT")
		}
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return os.Mkdir(p, 0700)
}
func regularFile(p string) error {
	f, err := os.Lstat(p)
	if err != nil {
		return err
	}
	if !f.Mode().IsRegular() {
		return errors.New("PROFILE_CONFLICT")
	}
	return nil
}
func replaceFile(p string, b []byte) error {
	f, err := os.CreateTemp(filepath.Dir(p), ".managed-*")
	if err != nil {
		return err
	}
	name := f.Name()
	defer os.Remove(name)
	if _, err = f.Write(b); err != nil {
		f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	return replace(name, p)
}

// Environment 只为新子进程重建环境，避免日常认证和代理覆盖专用配置。
func Environment(parent []string, profile, base string) []string {
	clean := make([]string, 0, len(parent)+6)
	for _, e := range parent {
		key, _, ok := strings.Cut(e, "=")
		upper := strings.ToUpper(key)
		if !ok || strings.HasPrefix(upper, "CODEX_") || strings.HasPrefix(upper, "OPENAI_") || strings.HasPrefix(upper, "CHATGPT_") || strings.HasPrefix(upper, "PI_") || upper == "HTTP_PROXY" || upper == "HTTPS_PROXY" || upper == "ALL_PROXY" || upper == "NO_PROXY" || upper == "ELECTRON_RUN_AS_NODE" || upper == "NODE_OPTIONS" || strings.HasPrefix(upper, "OTEL_") {
			continue
		}
		clean = append(clean, e)
	}
	// 不伪造Windows用户身份目录：当前桌面原生宿主依赖真实USERPROFILE。
	// Codex配置和Electron数据独立，但机器沙箱服务及听写共享锁仍是系统级资源。
	clean = append(clean, "CODEX_HOME="+filepath.Join(profile, "codex-home"), "CODEX_ELECTRON_USER_DATA_PATH="+filepath.Join(profile, "user-data"), "OTEL_SDK_DISABLED=true")
	if base != "" {
		clean = append(clean, "CODEX_APP_SERVER_OPENAI_BASE_URL="+base)
	}
	return clean
}

// Status 只报告安装信息、专用配置模式与由本管理器跟踪的进程。
func (m *Manager) Status() Status {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := Status{Items: []Client{}}
	for _, target := range []string{"cli", "desktop"} {
		install := m.installed[target]
		item := Client{Target: target, Installed: install.Path != "", Version: install.Version, State: "ready", Mode: "routed", Profile: m.profile(target), Message: "专用测试环境；登录由本人完成，未代表真实上游验证通过"}
		if raw, err := os.ReadFile(filepath.Join(m.profile(target), "managed-profile.json")); err == nil {
			var rec managedFile
			if json.Unmarshal(raw, &rec) == nil && (rec.Mode == "routed" || rec.Mode == "direct") {
				item.Mode = rec.Mode
			}
		}
		if !item.Installed {
			item.State = "not_installed"
			item.Message = "未检测到可安全启动的客户端"
		} else if active := m.active[target]; active != nil {
			item.State = "running"
			item.Message = "专用客户端已启动；是否登录和生成成功请在客户端确认"
		}
		if m.last[target] != "" {
			item.Message = m.last[target]
		}
		out.Items = append(out.Items, item)
	}
	return out
}

// Action 执行固定白名单动作，只操作独立测试环境。
func (m *Manager) Action(in Input) (Status, error) {
	if in.Target != "cli" && in.Target != "desktop" {
		return Status{}, errors.New("INVALID_CLIENT_ACTION")
	}
	if in.Action != "start" && in.Action != "login" && in.Action != "check" && in.Action != "stop" && in.Action != "direct" && in.Action != "routed" {
		return Status{}, errors.New("INVALID_CLIENT_ACTION")
	}
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return Status{}, errors.New("CLIENT_MANAGER_CLOSED")
	}
	active := m.active[in.Target]
	if in.Action == "stop" {
		if active != nil {
			active.closeJob()
		}
		m.mu.Unlock()
		if active != nil {
			select {
			case <-active.done:
			case <-time.After(8 * time.Second):
				return Status{}, errors.New("CLIENT_STOP_TIMEOUT")
			}
		}
		return m.Status(), nil
	}
	if active != nil {
		m.mu.Unlock()
		return Status{}, errors.New("CLIENT_RUNNING")
	}
	requested := ""
	if in.Action == "direct" || in.Action == "routed" {
		requested = in.Action
	}
	mode, err := m.ensureProfile(in.Target, requested)
	if err != nil {
		m.mu.Unlock()
		return Status{}, err
	}
	if requested != "" {
		m.last[in.Target] = "专用环境已切换，下次启动生效；日常配置未修改"
		m.mu.Unlock()
		return m.Status(), nil
	}
	install := m.installed[in.Target]
	if install.Path == "" {
		m.mu.Unlock()
		return Status{}, errors.New("CLIENT_NOT_INSTALLED")
	}
	profile := m.profile(in.Target)
	base := ""
	if mode == "routed" {
		base = m.base
	}
	if in.Action == "check" {
		if in.Target != "cli" {
			m.mu.Unlock()
			return Status{}, errors.New("INVALID_CLIENT_ACTION")
		}
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		cmd := exec.CommandContext(ctx, install.Path, "login", "status", "-c", "cli_auth_credentials_store=\"file\"")
		cmd.Dir = filepath.Join(profile, "work")
		cmd.Env = Environment(os.Environ(), profile, base)
		hideCommand(cmd)
		m.mu.Unlock()
		output, err := cmd.CombinedOutput()
		timedOut := ctx.Err() != nil
		cancel()
		m.mu.Lock()
		if err == nil {
			m.last[in.Target] = "专用CLI官方登录有效；尚不代表本次真实生成通过"
		} else if timedOut {
			m.last[in.Target] = "登录状态检查超时"
		} else if strings.Contains(strings.ToLower(string(output)), "not logged") {
			m.last[in.Target] = "需要在专用窗口完成官方登录"
		} else {
			m.last[in.Target] = "登录状态未确认，请在专用窗口检查"
		}
		m.mu.Unlock()
		return m.Status(), nil
	}
	var cmd *exec.Cmd
	if in.Target == "desktop" {
		cmd = exec.Command(install.Path, "--user-data-dir="+filepath.Join(profile, "user-data"), "--open-project", filepath.Join(profile, "work"))
	} else {
		cmd = exec.Command(m.helper, "client-console", "--profile", profile, "--action", in.Action)
	}
	cmd.Dir = filepath.Join(profile, "work")
	cmd.Env = Environment(os.Environ(), profile, base)
	stop, err := startOwned(cmd, in.Target == "cli")
	if err != nil {
		m.last[in.Target] = "专用客户端启动失败"
		m.mu.Unlock()
		return Status{}, errors.New("CLIENT_START_FAILED")
	}
	c := &child{cmd: cmd, closeJob: stop, done: make(chan struct{}), action: in.Action}
	m.active[in.Target] = c
	m.last[in.Target] = ""
	go func() {
		err := cmd.Wait()
		stop()
		m.mu.Lock()
		if m.active[in.Target] == c {
			delete(m.active, in.Target)
			if err != nil {
				m.last[in.Target] = "专用客户端已退出，请在客户端检查登录或启动错误"
			}
		}
		m.mu.Unlock()
		close(c.done)
	}()
	m.mu.Unlock()
	return m.Status(), nil
}

// Close 仅结束本管理器启动并持有句柄的客户端，不按进程名称批量关闭。
func (m *Manager) Close() {
	m.mu.Lock()
	m.closed = true
	owned := make([]*child, 0, len(m.active))
	for _, c := range m.active {
		owned = append(owned, c)
		c.closeJob()
	}
	m.mu.Unlock()
	for _, c := range owned {
		select {
		case <-c.done:
		case <-time.After(8 * time.Second):
		}
	}
}

// RunConsole 在专用窗口运行已检测的 CLI，登录输出只在该窗口显示。
func RunConsole(profile, action string) (result error) {
	if err := openConsole(); err != nil {
		return errors.New("CLIENT_CONSOLE_FAILED")
	}
	defer func() {
		if result != nil {
			fmt.Fprintln(os.Stderr, "专用客户端未正常结束，请检查登录或安装状态。")
		}
		fmt.Fprintln(os.Stdout, "\n专用客户端已结束，按回车关闭此窗口。")
		_, _ = bufio.NewReader(os.Stdin).ReadString('\n')
	}()
	if action != "start" && action != "login" {
		return errors.New("无效客户端动作")
	}
	abs, err := filepath.Abs(profile)
	if err != nil || abs != profile {
		return errors.New("专用目录无效")
	}
	if filepath.Clean(os.Getenv("CODEX_HOME")) != filepath.Join(profile, "codex-home") {
		return errors.New("专用环境不匹配")
	}
	fmt.Fprintln(os.Stdout, "CodexRouter 独立测试窗口，正在检测 CLI；不使用日常登录或历史。")
	install := detectCLI()
	if install.Path == "" {
		return errors.New("未检测到Codex CLI")
	}
	fmt.Fprintln(os.Stdout, "CodexRouter 专用测试窗口 · 日常配置未修改")
	args := []string{"--no-daemon", "-C", filepath.Join(profile, "work")}
	if action == "login" {
		args = []string{"login", "-c", "cli_auth_credentials_store=\"file\""}
	}
	cmd := exec.CommandContext(context.Background(), install.Path, args...)
	cmd.Env = os.Environ()
	cmd.Dir = filepath.Join(profile, "work")
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}
