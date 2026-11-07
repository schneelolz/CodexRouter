// Package daily 管理原Codex配置中的单个连接字段，不访问认证和历史。
package daily

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"codexrouter/internal/store"
	"github.com/pelletier/go-toml/v2"
	"github.com/pelletier/go-toml/v2/unstable"
)

// Status 描述配置接入而非实际请求成功。
type Status struct {
	State      string `json:"state"`
	ConfigPath string `json:"config_path"`
	BaseURL    string `json:"base_url"`
	HasBackup  bool   `json:"has_backup"`
	Message    string `json:"message"`
}

// Preview 仅展示固定字段的变更，不包含配置正文。
type Preview struct {
	Token      string `json:"token"`
	Action     string `json:"action"`
	ConfigPath string `json:"config_path"`
	Before     string `json:"before"`
	After      string `json:"after"`
	ExpiresAt  string `json:"expires_at"`
	Warning    string `json:"warning"`
}
type record struct {
	Version  int    `json:"version"`
	Active   bool   `json:"active"`
	Owner    string `json:"owner"`
	Original string `json:"original"`
	Present  bool   `json:"present"`
	Written  string `json:"written"`
}
type journal struct {
	Pending bool   `json:"pending"`
	Before  string `json:"before"`
	After   string `json:"after"`
	Next    record `json:"next"`
}
type snapshot struct {
	field                      bool
	value                      string
	raw                        string
	start, end, kvStart, kvEnd int
}
type proposal struct {
	token, hash, action string
	expiry              time.Time
}

// Manager 保存固定配置目标和短期预览，不接受网页传入路径。
type Manager struct {
	mu                                      sync.Mutex
	home, config, dir, base, owner, blocked string
	preview                                 *proposal
}

func failure(code string) error { return errors.New(code) }
func sum(b []byte) string       { v := sha256.Sum256(b); return hex.EncodeToString(v[:]) }

// New 绑定配置目录；不创建目录、不读取认证、不修改用户环境。
func New(home, base, owner string) (*Manager, error) {
	u, err := url.Parse(base)
	if err != nil || u.Scheme != "http" || u.Hostname() != "127.0.0.1" || u.Port() == "" || u.Path != "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return nil, failure("DAILY_UNAVAILABLE")
	}
	if home == "" || !filepath.IsAbs(home) {
		return nil, failure("DAILY_UNAVAILABLE")
	}
	home = filepath.Clean(home)
	if strings.HasPrefix(home, `\\`) {
		return nil, failure("DAILY_UNSUPPORTED")
	}
	return &Manager{home: home, config: filepath.Join(home, "config.toml"), dir: filepath.Join(home, ".codexrouter-routing"), base: base, owner: sum([]byte(ownerIdentity(owner)))}, nil
}

// Discover 使用现有CODEX_HOME；有地址覆盖时拒绝误报接入成功。
func Discover(base, owner string) (*Manager, error) {
	home := os.Getenv("CODEX_HOME")
	if home == "" {
		h, err := os.UserHomeDir()
		if err != nil {
			return nil, err
		}
		home = filepath.Join(h, ".codex")
	}
	m, err := New(home, base, owner)
	if err != nil {
		return nil, err
	}
	for _, key := range []string{"CODEX_APP_SERVER_OPENAI_BASE_URL", "CODEX_APP_SERVER_WS_URL", "CODEX_APP_SERVER_FORCE_CLI", "CODEX_CLI_PATH", "CODEX_APP_RUNTIME_CLI_PATH"} {
		if os.Getenv(key) != "" {
			m.blocked = "DAILY_ENV_CONFLICT"
		}
	}
	return m, nil
}
func inspect(raw []byte) (snapshot, error) {
	var out snapshot
	var doc map[string]any
	if len(raw) > 1<<20 || bytes.HasPrefix(raw, []byte{0xef, 0xbb, 0xbf}) || toml.Unmarshal(raw, &doc) != nil {
		return out, failure("DAILY_UNSUPPORTED")
	}
	if v, ok := doc["model_provider"]; ok && v != "openai" {
		return out, failure("DAILY_UNSUPPORTED")
	}
	if _, ok := doc["profile"]; ok {
		return out, failure("DAILY_UNSUPPORTED")
	}
	if _, ok := doc["profiles"]; ok {
		return out, failure("DAILY_UNSUPPORTED")
	}
	if models, ok := doc["model_providers"].(map[string]any); ok {
		if _, ok = models["openai"]; ok {
			return out, failure("DAILY_UNSUPPORTED")
		}
	}
	p := unstable.Parser{}
	p.Reset(raw)
	root := true
	for p.NextExpression() {
		e := p.Expression()
		if e.Kind == unstable.Table || e.Kind == unstable.ArrayTable {
			root = false
			continue
		}
		if !root || e.Kind != unstable.KeyValue {
			continue
		}
		it := e.Key()
		keys := []string{}
		for it.Next() {
			keys = append(keys, string(it.Node().Data))
		}
		if len(keys) != 1 || keys[0] != "openai_base_url" {
			continue
		}
		v := e.Value()
		if v.Kind != unstable.String {
			return out, failure("DAILY_UNSUPPORTED")
		}
		out = snapshot{field: true, value: string(v.Data), raw: string(p.Raw(v.Raw)), start: int(v.Raw.Offset), end: int(v.Raw.Offset + v.Raw.Length), kvStart: int(e.Raw.Offset), kvEnd: int(e.Raw.Offset + e.Raw.Length)}
	}
	if p.Error() != nil {
		return out, failure("DAILY_UNSUPPORTED")
	}
	if v, ok := doc["openai_base_url"]; ok {
		if _, ok := v.(string); !ok {
			return out, failure("DAILY_UNSUPPORTED")
		}
	}
	// 原值也不可把URL内嵌凭据写入记录或显示到页面。
	if out.field {
		u, err := url.Parse(out.value)
		if err != nil || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
			return out, failure("DAILY_UNSUPPORTED")
		}
	}
	return out, nil
}
func patch(raw []byte, s snapshot, value string, present bool) []byte {
	if s.field {
		if present {
			return append(append(append([]byte{}, raw[:s.start]...), []byte(value)...), raw[s.end:]...)
		}
		start, end := s.kvStart, s.kvEnd
		lineStart := bytes.LastIndexByte(raw[:start], '\n') + 1
		lineEnd := bytes.IndexByte(raw[end:], '\n')
		if lineEnd < 0 {
			lineEnd = len(raw)
		} else {
			lineEnd += end + 1
		}
		suffix := strings.TrimSpace(string(raw[end:lineEnd]))
		if suffix == "" && strings.TrimSpace(string(raw[lineStart:start])) == "" {
			start, end = lineStart, lineEnd
		}
		return append(append([]byte{}, raw[:start]...), raw[end:]...)
	}
	if !present {
		return append([]byte{}, raw...)
	}
	newline := "\n"
	if bytes.Contains(raw, []byte("\r\n")) {
		newline = "\r\n"
	}
	return append([]byte("openai_base_url = "+value+newline), raw...)
}
func (m *Manager) read() ([]byte, snapshot, error) {
	if m.blocked != "" {
		return nil, snapshot{}, failure(m.blocked)
	}
	if err := safePath(m.home, true); err != nil {
		return nil, snapshot{}, err
	}
	if err := safePath(m.config, false); err != nil {
		return nil, snapshot{}, err
	}
	f, err := os.Open(m.config)
	if err != nil {
		return nil, snapshot{}, failure("DAILY_UNAVAILABLE")
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, (1<<20)+1))
	if err != nil {
		return nil, snapshot{}, failure("DAILY_UNAVAILABLE")
	}
	s, err := inspect(raw)
	return raw, s, err
}
func (m *Manager) database(create bool) (*store.Store, error) {
	if !create {
		if _, err := os.Lstat(m.dir); errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
	}
	if err := safePath(m.home, true); err != nil {
		return nil, err
	}
	if err := safeOptionalDir(m.dir); err != nil {
		return nil, err
	}
	db, err := store.Open(m.dir)
	if err != nil {
		return nil, failure("DAILY_BUSY")
	}
	return db, nil
}
func readRecord(db *store.Store) (record, journal, error) {
	var rec record
	var j journal
	if db == nil {
		return rec, j, nil
	}
	if err := db.Read("record.json", &rec); err != nil {
		if !errors.Is(err, store.ErrNotFound) {
			return rec, j, failure("DAILY_RECOVERY_REQUIRED")
		}
	} else if !validRecord(rec) {
		return rec, j, failure("DAILY_RECOVERY_REQUIRED")
	}
	if err := db.Read("journal.json", &j); err != nil && !errors.Is(err, store.ErrNotFound) {
		return rec, j, failure("DAILY_RECOVERY_REQUIRED")
	}
	if j.Pending && (!validRecord(j.Next) || len(j.Before) != 64 || len(j.After) != 64) {
		return rec, j, failure("DAILY_RECOVERY_REQUIRED")
	}
	return rec, j, nil
}
func validRecord(rec record) bool {
	if rec.Version != 1 || len(rec.Owner) != 64 {
		return false
	}
	u, err := url.Parse(rec.Written)
	if err != nil || u.Scheme != "http" || u.Hostname() != "127.0.0.1" || u.Port() == "" || u.Path != "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return false
	}
	if !rec.Present {
		return rec.Original == ""
	}
	raw := []byte("openai_base_url=" + rec.Original)
	s, err := inspect(raw)
	return err == nil && s.field && s.raw == rec.Original && s.end == len(raw)
}
func reconcile(rec record, j journal, raw []byte) (record, error) {
	if !j.Pending {
		return rec, nil
	}
	if sum(raw) == j.After {
		return j.Next, nil
	}
	if sum(raw) == j.Before {
		return rec, nil
	}
	return rec, failure("DAILY_RECOVERY_REQUIRED")
}

// Status 不把配置已接入当成请求已收到；只返回脱敏状态。
func (m *Manager) Status() Status {
	m.mu.Lock()
	defer m.mu.Unlock()
	result := Status{State: "direct", ConfigPath: m.config, BaseURL: m.base, Message: "尚未接入；沿用原来的登录、历史和项目"}
	raw, s, err := m.read()
	if err != nil {
		result.State = "blocked"
		result.Message = Message(err)
		return result
	}
	db, err := m.database(false)
	if err != nil {
		result.State = "blocked"
		result.Message = Message(err)
		return result
	}
	if db != nil {
		defer db.Close()
	}
	rec, j, err := readRecord(db)
	if err == nil {
		rec, err = reconcile(rec, j, raw)
	}
	if err != nil {
		result.State = "blocked"
		result.Message = Message(err)
		return result
	}
	result.HasBackup = rec.Active
	if rec.Active {
		if rec.Owner != m.owner || s.value != rec.Written || !s.field {
			result.State = "conflict"
			result.Message = "连接字段或管理实例已改变，不会自动覆盖"
		} else if rec.Written != m.base {
			result.State = "port_changed"
			result.Message = "路由器地址变化，请预览后更新接入"
		} else {
			result.State = "configured"
			result.Message = "配置已接入；请保存工作并手动重开Codex，是否生效以实际请求为准"
		}
	} else if s.field && s.value == m.base {
		result.State = "conflict"
		result.Message = "配置已指向本机但缺少原字段备份。请保留现状并查找原数据目录，不会猜测恢复值。"
	} else if s.field {
		result.State = "custom"
		result.Message = "已有自定义连接地址；接入前将备份该字段，恢复时还原原值"
	}
	return result
}
func (m *Manager) prepare(action string, raw []byte, s snapshot, rec record) ([]byte, record, error) {
	if rec.Active && (rec.Owner != m.owner || !s.field || s.value != rec.Written) {
		return nil, rec, failure("DAILY_CONFLICT")
	}
	switch action {
	case "route":
		if !rec.Active {
			if s.field && s.value == m.base {
				return nil, rec, failure("DAILY_CONFLICT")
			}
			rec = record{Version: 1, Active: true, Owner: m.owner, Original: s.raw, Present: s.field}
		}
		rec.Written = m.base
		value, _ := json.Marshal(m.base)
		return patch(raw, s, string(value), true), rec, nil
	case "restore":
		if !rec.Active {
			return nil, rec, failure("DAILY_NO_BACKUP")
		}
		next := patch(raw, s, rec.Original, rec.Present)
		rec.Active = false
		return next, rec, nil
	default:
		return nil, rec, failure("DAILY_INVALID_ACTION")
	}
}

// Preview 创建5分钟单次确认，应用前重新检查文件摘要。
func (m *Manager) Preview(action string) (Preview, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	raw, s, err := m.read()
	if err != nil {
		return Preview{}, err
	}
	db, err := m.database(true)
	if err != nil {
		return Preview{}, err
	}
	defer db.Close()
	rec, j, err := readRecord(db)
	if err == nil {
		rec, err = reconcile(rec, j, raw)
	}
	if err != nil {
		return Preview{}, err
	}
	next, _, err := m.prepare(action, raw, s, rec)
	if err != nil {
		return Preview{}, err
	}
	if _, err = inspect(next); err != nil {
		return Preview{}, err
	}
	var random [32]byte
	if _, err = rand.Read(random[:]); err != nil {
		return Preview{}, failure("DAILY_UNAVAILABLE")
	}
	token := hex.EncodeToString(random[:])
	expiry := time.Now().Add(5 * time.Minute)
	m.preview = &proposal{token: sum([]byte(token)), hash: sum(raw), action: action, expiry: expiry}
	before := "未设置（使用客户端默认地址）"
	if s.field {
		before = s.value
	}
	after := m.base
	if action == "restore" {
		after = "移除本程序添加的连接字段"
		if rec.Present {
			var doc map[string]any
			_ = toml.Unmarshal([]byte("value="+rec.Original), &doc)
			after, _ = doc["value"].(string)
		}
	}
	return Preview{Token: token, Action: action, ConfigPath: m.config, Before: before, After: after, ExpiresAt: expiry.UTC().Format(time.RFC3339), Warning: "桌面版与CLI可能共用此配置。保存工作后手动重开；现有CLI共享后台不会被停止，可能仍沿用旧连接。路由器需保持运行；不修改登录认证、历史或项目。"}, nil
}

// Apply 先落盘字段备份和日志，再替换配置；失败时可判定替换前后状态。
func (m *Manager) Apply(token string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	p := m.preview
	m.preview = nil
	if p == nil || time.Now().After(p.expiry) || sum([]byte(token)) != p.token {
		return failure("DAILY_PREVIEW_EXPIRED")
	}
	raw, s, err := m.read()
	if err != nil {
		return err
	}
	if sum(raw) != p.hash {
		return failure("DAILY_CONFIG_CHANGED")
	}
	db, err := m.database(true)
	if err != nil {
		return err
	}
	defer db.Close()
	rec, j, err := readRecord(db)
	if err == nil {
		rec, err = reconcile(rec, j, raw)
	}
	if err != nil {
		return err
	}
	// 先结清上一条可识别的日志，再开始本次操作。
	if j.Pending {
		if rec.Version != 0 {
			if err = db.Write("record.json", rec); err != nil {
				return failure("DAILY_WRITE_FAILED")
			}
		}
		if err = db.Write("journal.json", journal{}); err != nil {
			return failure("DAILY_WRITE_FAILED")
		}
	}
	next, nextRec, err := m.prepare(p.action, raw, s, rec)
	if err != nil {
		return err
	}
	if _, err = inspect(next); err != nil {
		return err
	}
	if err = db.Write("journal.json", journal{Pending: true, Before: sum(raw), After: sum(next), Next: nextRec}); err != nil {
		return failure("DAILY_WRITE_FAILED")
	}
	if err = replaceConfig(m.config, raw, next); err != nil {
		return err
	}
	if err = db.Write("record.json", nextRec); err != nil {
		return failure("DAILY_RECOVERY_REQUIRED")
	}
	if err = db.Write("journal.json", journal{}); err != nil {
		return failure("DAILY_RECOVERY_REQUIRED")
	}
	return nil
}

// Message 把内部错误转成固定文案，不输出原配置或操作系统异常正文。
func Message(err error) string {
	switch err.Error() {
	case "DAILY_INVALID_ACTION":
		return "仅支持预览接入或恢复原配置"
	case "DAILY_ENV_CONFLICT":
		return "检测到进程级连接或客户端覆盖，请先检查外部启动器；未修改配置"
	case "DAILY_UNSUPPORTED":
		return "当前配置有不支持的供应商、配置档、路径或格式，已拒绝覆盖"
	case "DAILY_BUSY":
		return "其他实例正在处理此配置，请稍后重试"
	case "DAILY_CONFIG_CHANGED":
		return "预览后配置发生变化，请重新预览；未覆盖外部修改"
	case "DAILY_PREVIEW_EXPIRED":
		return "确认已失效或已使用，请重新预览"
	case "DAILY_CONFLICT":
		return "连接字段不再由本实例管理，已拒绝覆盖；请检查原管理实例或外部改动"
	case "DAILY_NO_BACKUP":
		return "没有可恢复的接入记录"
	case "DAILY_RECOVERY_REQUIRED":
		return "上次操作未完整结束且需要核查，请保留配置和备份，不要反复接入"
	case "DAILY_WRITE_FAILED":
		return "配置写入未完成，请刷新检查接入状态；备份记录已保留"
	default:
		return "日常配置不可用或不存在；未修改认证及历史"
	}
}
