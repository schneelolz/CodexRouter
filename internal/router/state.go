// Package router 提供与前端构建无关的路由及管理服务。
package router

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"codexrouter/internal/api"
	"codexrouter/internal/history"
	"codexrouter/internal/platform"
	"codexrouter/internal/store"
)

// Persistence 隔离版本化状态的原子存储。
type Persistence interface {
	Read(string, any) error
	Write(string, any) error
}

const (
	stateVersion = 2
	maxProviders = 8
	// BuiltinOfficialID 是始终存在、转发客户端官方身份的供应商。
	BuiltinOfficialID = "official"
	// BuiltinCompanyID 兼容旧版公司线路，并沿用旧凭据名称。
	BuiltinCompanyID = "company"
	// BuiltinOfficialURL 是首次运行时的官方基址。
	BuiltinOfficialURL = "https://chatgpt.com/backend-api/codex"
)

// providerRecord 是落盘的供应商定义；可用状态由凭据和绑定实时计算。
type providerRecord struct {
	ID        string           `json:"id"`
	Name      string           `json:"name"`
	Kind      api.ProviderKind `json:"kind"`
	BaseURL   string           `json:"base_url"`
	Model     string           `json:"model"`
	CreatedAt string           `json:"created_at"`
}

// diskState 是版本 2 的完整状态。
type diskState struct {
	Version   int                    `json:"version"`
	Config    api.Config             `json:"config"`
	Providers []providerRecord       `json:"providers"`
	Bindings  map[string]api.Binding `json:"bindings"`
}

// stateEnvelope 按版本分发状态内容，避免把不同版本的结构混为一体。
type stateEnvelope struct {
	Version   int             `json:"version"`
	Config    json.RawMessage `json:"config"`
	Providers json.RawMessage `json:"providers"`
	Bindings  json.RawMessage `json:"bindings"`
}

// legacyConfig 只用于读取 0.1.0 的版本 1 配置。
type legacyConfig struct {
	Version          int    `json:"version"`
	DefaultRoute     string `json:"default_route"`
	OfficialURL      string `json:"official_url"`
	CompanyURL       string `json:"company_url"`
	ConnectTimeoutMS int    `json:"connect_timeout_ms"`
	HeaderTimeoutMS  int    `json:"header_timeout_ms"`
	IdleTimeoutMS    int    `json:"idle_timeout_ms"`
	WriteTimeoutMS   int    `json:"write_timeout_ms"`
	MaxConcurrent    int    `json:"max_concurrent"`
	UpstreamsLocked  bool   `json:"upstreams_locked"`
}

// legacyBinding 只用于读取版本 1 的固定会话。
type legacyBinding struct {
	ID        string `json:"id"`
	Route     string `json:"route"`
	CreatedAt string `json:"created_at"`
}

type state struct {
	history       *history.Store // 请求清理不能访问本结构中的权威绑定。
	historyFailed func()         // 报告投影写入失败，不回滚已成功的权威绑定。
	historyDirty  bool           // 下次查询时从 JSON 重建失败的绑定投影。
	mu            sync.Mutex     // 串行化绑定和配置，写盘成功后才发布。
	disk          diskState
	store         Persistence
	creds         platform.Credentials
	pending       map[string]api.PendingSession
}

// DefaultConfig 返回保守的首次运行配置：官方线路为默认，模型透传。
func DefaultConfig() api.Config {
	return api.Config{Version: stateVersion, DefaultProviderID: BuiltinOfficialID, ConnectTimeoutMS: 10000, HeaderTimeoutMS: 30000, IdleTimeoutMS: 120000, WriteTimeoutMS: 30000, MaxConcurrent: 16}
}

// defaultProviders 保留旧版官方/公司两条线路及其凭据命名空间。
func defaultProviders(now string) []providerRecord {
	return []providerRecord{
		{ID: BuiltinOfficialID, Name: "官方线路", Kind: api.ProviderKindOfficial, BaseURL: BuiltinOfficialURL, CreatedAt: now},
		{ID: BuiltinCompanyID, Name: "公司线路", Kind: api.ProviderKindAPIKey, CreatedAt: now},
	}
}

// validID 限定会话标识与凭据名称的安全字符集。
func validID(id string) bool {
	if len(id) < 1 || len(id) > 128 {
		return false
	}
	for _, r := range id {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_') {
			return false
		}
	}
	return true
}

// validProviderID 排除与控制凭据同名的供应商，避免命名空间冲突。
func validProviderID(id string) bool {
	return validID(id) && len(id) <= 64 && id != "control"
}

func validProviderKind(kind api.ProviderKind) bool {
	return kind == api.ProviderKindOfficial || kind == api.ProviderKindAPIKey
}

var modelPattern = regexp.MustCompile(`^[\x21-\x7e]{0,128}$`)

// validateProviderText 拒绝控制字符，保留中文显示名称。
func validateProviderText(value string, max int) error {
	if value == "" || utf8.RuneCountInString(value) > max || !utf8.ValidString(value) {
		return errors.New("供应商名称须为 1 至 64 个字符")
	}
	for _, r := range value {
		if r < 0x20 || r == 0x7f {
			return errors.New("供应商名称不能包含控制字符")
		}
	}
	return nil
}

// validateBaseURL 只接受不含凭据、查询和片段的 HTTP(S) 基址。
func validateBaseURL(raw string, allowEmpty bool) error {
	if raw == "" {
		if allowEmpty {
			return nil
		}
		return errors.New("上游地址不能为空")
	}
	if len(raw) > 2048 {
		return errors.New("上游地址过长")
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Opaque != "" || strings.Contains(u.Path, "..") || strings.ContainsAny(raw, "\r\n\\") {
		return errors.New("上游地址必须是不含凭据、查询和片段的 HTTP(S) 基址")
	}
	if u.Scheme != "https" && u.Scheme != "http" {
		return errors.New("上游仅支持 HTTP(S)")
	}
	if u.Scheme == "http" {
		ip := net.ParseIP(u.Hostname())
		if ip == nil || !(ip.IsLoopback() || ip.IsPrivate()) {
			return errors.New("HTTP 上游仅允许明确的内网或 loopback IP")
		}
	}
	return nil
}

// validateProvider 校验落盘供应商的完整字段。
func validateProvider(p providerRecord) error {
	if !validProviderID(p.ID) || !validProviderKind(p.Kind) {
		return errors.New("供应商标识或认证方式无效")
	}
	if err := validateProviderText(p.Name, 64); err != nil {
		return err
	}
	if err := validateBaseURL(p.BaseURL, p.Kind == api.ProviderKindAPIKey); err != nil {
		return err
	}
	if !modelPattern.MatchString(p.Model) {
		return errors.New("模型覆盖须为 0 至 128 个可打印 ASCII 字符")
	}
	if _, err := time.Parse(time.RFC3339, p.CreatedAt); err != nil {
		return errors.New("供应商创建时间无效")
	}
	return nil
}

// providerUsable 表示该供应商已有基址，可以承接新会话或登记。
func providerUsable(p providerRecord) bool { return p.BaseURL != "" }

// findProvider 在切片中按标识查找。
func findProvider(list []providerRecord, id string) (providerRecord, bool) {
	for _, p := range list {
		if p.ID == id {
			return p, true
		}
	}
	return providerRecord{}, false
}

// validateConfig 校验默认供应商存在且可路由，并检查超时与并发边界。
func validateConfig(c api.Config, providers []providerRecord) error {
	if c.Version != stateVersion || !validProviderID(c.DefaultProviderID) {
		return errors.New("配置版本或默认供应商无效")
	}
	def, ok := findProvider(providers, c.DefaultProviderID)
	if !ok || !providerUsable(def) {
		return errors.New("所选默认供应商不存在或尚未配置上游地址")
	}
	for _, n := range []int{c.ConnectTimeoutMS, c.HeaderTimeoutMS, c.WriteTimeoutMS} {
		if n < 100 || n > 300000 {
			return errors.New("连接、响应头与写超时须在 100 至 300000 毫秒之间")
		}
	}
	if c.IdleTimeoutMS < 100 || c.IdleTimeoutMS > 3600000 {
		return errors.New("流空闲超时须在 100 至 3600000 毫秒之间")
	}
	if c.MaxConcurrent < 1 || c.MaxConcurrent > 128 {
		return errors.New("并发数须在 1 至 128 之间")
	}
	return nil
}

// decodeStrict 拒绝未知字段和尾随内容，避免静默接受结构变化。
func decodeStrict(data []byte, out any) error {
	if len(bytes.TrimSpace(data)) == 0 {
		return errors.New("缺少字段")
	}
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if err := d.Decode(out); err != nil {
		return err
	}
	if d.Decode(new(any)) != io.EOF {
		return errors.New("包含多余内容")
	}
	return nil
}

// loadState 读取并按版本迁移状态；损坏、未知版本或写入失败均拒绝启动。
func loadState(p Persistence) (*state, error) {
	s := &state{store: p, pending: make(map[string]api.PendingSession)}
	var env stateEnvelope
	err := p.Read("state.json", &env)
	if errors.Is(err, store.ErrNotFound) {
		s.disk = diskState{Version: stateVersion, Config: DefaultConfig(), Providers: defaultProviders(time.Now().UTC().Format(time.RFC3339)), Bindings: make(map[string]api.Binding)}
		if err = validateDisk(s.disk); err != nil {
			return nil, err
		}
		if err = p.Write("state.json", s.disk); err != nil {
			return nil, fmt.Errorf("初始化状态失败: %w", err)
		}
		return s, nil
	}
	if err != nil {
		return nil, fmt.Errorf("读取状态失败，未重建: %w", err)
	}
	disk, migrated, err := decodeState(env)
	if err != nil {
		return nil, err
	}
	if err = validateDisk(disk); err != nil {
		return nil, err
	}
	s.disk = disk
	if migrated {
		// 迁移结果立即落盘；失败时保持原文件，不用内存状态继续运行。
		if err = p.Write("state.json", s.disk); err != nil {
			return nil, fmt.Errorf("迁移状态版本失败: %w", err)
		}
	}
	return s, nil
}

func decodeState(env stateEnvelope) (diskState, bool, error) {
	switch env.Version {
	case 1:
		disk, err := migrateLegacy(env)
		return disk, true, err
	case stateVersion:
		var disk diskState
		disk.Version = env.Version
		if err := decodeStrict(env.Config, &disk.Config); err != nil {
			return diskState{}, false, fmt.Errorf("状态配置损坏，未重建: %w", err)
		}
		if err := decodeStrict(env.Providers, &disk.Providers); err != nil {
			return diskState{}, false, fmt.Errorf("供应商结构损坏，未重建: %w", err)
		}
		if err := decodeStrict(env.Bindings, &disk.Bindings); err != nil {
			return diskState{}, false, fmt.Errorf("绑定结构损坏，未重建: %w", err)
		}
		return disk, false, nil
	default:
		return diskState{}, false, errors.New("状态版本或绑定结构无效，未重建")
	}
}

// migrateLegacy 把版本 1 的官方/公司两条线路转换为供应商，保留原绑定与凭据名称。
func migrateLegacy(env stateEnvelope) (diskState, error) {
	var legacy legacyConfig
	if err := decodeStrict(env.Config, &legacy); err != nil {
		return diskState{}, fmt.Errorf("旧状态配置损坏，未重建: %w", err)
	}
	if legacy.Version != 1 {
		return diskState{}, errors.New("旧状态配置版本无效，未重建")
	}
	var bindings map[string]legacyBinding
	if err := decodeStrict(env.Bindings, &bindings); err != nil {
		return diskState{}, fmt.Errorf("旧状态绑定损坏，未重建: %w", err)
	}
	if bindings == nil {
		return diskState{}, errors.New("旧状态绑定结构缺失，未重建")
	}
	now := time.Now().UTC().Format(time.RFC3339)
	providers := defaultProviders(now)
	providers[0].BaseURL = legacy.OfficialURL
	providers[1].BaseURL = legacy.CompanyURL
	if legacy.DefaultRoute != BuiltinOfficialID && legacy.DefaultRoute != BuiltinCompanyID {
		return diskState{}, errors.New("旧状态默认线路无效，未重建")
	}
	disk := diskState{Version: stateVersion, Providers: providers, Bindings: make(map[string]api.Binding, len(bindings))}
	disk.Config = api.Config{Version: stateVersion, DefaultProviderID: legacy.DefaultRoute, ConnectTimeoutMS: legacy.ConnectTimeoutMS, HeaderTimeoutMS: legacy.HeaderTimeoutMS, IdleTimeoutMS: legacy.IdleTimeoutMS, WriteTimeoutMS: legacy.WriteTimeoutMS, MaxConcurrent: legacy.MaxConcurrent}
	for key, b := range bindings {
		if b.Route != BuiltinOfficialID && b.Route != BuiltinCompanyID {
			return diskState{}, errors.New("旧状态绑定线路无效，未重建")
		}
		disk.Bindings[key] = api.Binding{ID: b.ID, ProviderID: b.Route, CreatedAt: b.CreatedAt}
	}
	return disk, nil
}

// validateDisk 在发布状态前检查版本、供应商、配置和全部绑定。
func validateDisk(disk diskState) error {
	if disk.Version != stateVersion || disk.Bindings == nil || len(disk.Bindings) > 10000 {
		return errors.New("状态版本或绑定结构无效，未重建")
	}
	if len(disk.Providers) < 1 || len(disk.Providers) > maxProviders {
		return errors.New("供应商数量无效，未重建")
	}
	seen := make(map[string]bool, len(disk.Providers))
	for _, p := range disk.Providers {
		if seen[p.ID] || !validProviderID(p.ID) {
			return errors.New("供应商标识重复或无效，未重建")
		}
		seen[p.ID] = true
		if err := validateProvider(p); err != nil {
			return fmt.Errorf("供应商状态损坏，未重建: %w", err)
		}
	}
	official, ok := findProvider(disk.Providers, BuiltinOfficialID)
	if !ok || official.Kind != api.ProviderKindOfficial {
		return errors.New("缺少内置官方供应商，未重建")
	}
	if err := validateConfig(disk.Config, disk.Providers); err != nil {
		return err
	}
	// 仅内置官方供应商可以转发客户端官方身份，避免把官方令牌发给自定义地址。
	for _, p := range disk.Providers {
		if p.Kind == api.ProviderKindOfficial && p.ID != BuiltinOfficialID {
			return errors.New("仅内置官方供应商可使用官方身份，未重建")
		}
	}
	for id, b := range disk.Bindings {
		_, dateErr := time.Parse(time.RFC3339, b.CreatedAt)
		provider, known := findProvider(disk.Providers, b.ProviderID)
		if id != b.ID || !validID(id) || !known || dateErr != nil {
			return errors.New("绑定状态损坏，未重建")
		}
		// 已绑定供应商必须仍有可路由基址，否则重启后该会话无法转发。
		if !providerUsable(provider) {
			return errors.New("绑定供应商缺少上游地址，未重建")
		}
	}
	return nil
}

func (s *state) config() api.Config { s.mu.Lock(); defer s.mu.Unlock(); return s.disk.Config }

// saveConfig 只发布已成功落盘的配置；落盘失败时保持原配置。
func (s *state) saveConfig(c api.Config) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.saveConfigLocked(c)
}

func (s *state) saveConfigLocked(c api.Config) error {
	if err := validateConfig(c, s.disk.Providers); err != nil {
		return err
	}
	next := s.disk
	next.Config = c
	if err := s.store.Write("state.json", next); err != nil {
		return err
	}
	s.disk = next
	return nil
}

// bindingCounts 统计每个供应商的固定会话数量。
func bindingCounts(bindings map[string]api.Binding) map[string]int {
	counts := make(map[string]int)
	for _, b := range bindings {
		counts[b.ProviderID]++
	}
	return counts
}

// view 把落盘定义转换为带实时状态的页面视图；凭据读取失败不能谎报未配置。
func (s *state) view(p providerRecord, locked bool) (api.Provider, error) {
	configured := false
	if p.Kind == api.ProviderKindAPIKey && s.creds != nil {
		value, err := s.creds.Get(p.ID)
		if err != nil && !errors.Is(err, platform.ErrNotFound) {
			return api.Provider{}, errors.New("CREDENTIAL_UNAVAILABLE")
		}
		configured = err == nil && value != ""
	}
	return api.Provider{ID: p.ID, Name: p.Name, Kind: p.Kind, BaseURL: p.BaseURL, Model: p.Model, KeyConfigured: configured, UpstreamLocked: locked}, nil
}

// providers 返回全部供应商的页面视图；读取凭据时不持有状态锁。
func (s *state) providers() (api.Providers, error) {
	s.mu.Lock()
	records := append([]providerRecord{}, s.disk.Providers...)
	counts := bindingCounts(s.disk.Bindings)
	s.mu.Unlock()
	items := make([]api.Provider, 0, len(records))
	for _, p := range records {
		item, err := s.view(p, counts[p.ID] > 0)
		if err != nil {
			return api.Providers{}, err
		}
		items = append(items, item)
	}
	return api.Providers{Items: items}, nil
}

// providerRecord 返回落盘定义，供转发路径读取基址与模型覆盖。
func (s *state) providerRecord(id string) (providerRecord, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return findProvider(s.disk.Providers, id)
}

// newProviderID 生成不会再复用的标识，避免继承已删除供应商的凭据。
func newProviderID(existing []providerRecord) (string, error) {
	used := make(map[string]bool, len(existing))
	for _, p := range existing {
		used[p.ID] = true
	}
	for range 8 {
		var b [8]byte
		if _, err := rand.Read(b[:]); err != nil {
			return "", errors.New("系统随机源不可用")
		}
		id := "p" + hex.EncodeToString(b[:])
		if !used[id] {
			return id, nil
		}
	}
	return "", errors.New("PROVIDER_ID_FAILED")
}

// addProvider 追加新供应商；标识由后端生成并立即持久化。
func (s *state) addProvider(in api.ProviderCreateInput) (api.Provider, error) {
	// 新增只允许独立 API 密钥，官方身份仅保留给内置官方供应商。
	if in.Kind != api.ProviderCreateKindAPIKey {
		return api.Provider{}, errors.New("INVALID_INPUT")
	}
	rec := providerRecord{Name: strings.TrimSpace(in.Name), Kind: api.ProviderKindAPIKey, BaseURL: strings.TrimSpace(in.BaseURL), Model: strings.TrimSpace(in.Model), CreatedAt: time.Now().UTC().Format(time.RFC3339)}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.disk.Providers) >= maxProviders {
		return api.Provider{}, errors.New("PROVIDER_LIMIT")
	}
	id, err := newProviderID(s.disk.Providers)
	if err != nil {
		return api.Provider{}, err
	}
	rec.ID = id
	if err = validateProvider(rec); err != nil {
		return api.Provider{}, errors.New("INVALID_INPUT")
	}
	// 响应视图要在写盘前生成；凭据读取失败时不能写入后再把成功变更报成失败。
	view, err := s.view(rec, false)
	if err != nil {
		return api.Provider{}, err
	}
	next := s.disk
	next.Providers = append(append([]providerRecord{}, s.disk.Providers...), rec)
	if err = s.store.Write("state.json", next); err != nil {
		return api.Provider{}, errors.New("STORAGE_WRITE_FAILED")
	}
	s.disk = next
	return view, nil
}

// updateProvider 修改名称、基址与模型覆盖；已有绑定时基址锁定。
func (s *state) updateProvider(id string, in api.ProviderUpdateInput) (api.Provider, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	index := -1
	for i, p := range s.disk.Providers {
		if p.ID == id {
			index = i
		}
	}
	if index < 0 {
		return api.Provider{}, errors.New("PROVIDER_NOT_FOUND")
	}
	updated := s.disk.Providers[index]
	updated.Name = strings.TrimSpace(in.Name)
	updated.BaseURL = strings.TrimSpace(in.BaseURL)
	updated.Model = strings.TrimSpace(in.Model)
	if err := validateProvider(updated); err != nil {
		return api.Provider{}, errors.New("INVALID_INPUT")
	}
	locked := bindingCounts(s.disk.Bindings)[id] > 0
	if locked && updated.BaseURL != s.disk.Providers[index].BaseURL {
		return api.Provider{}, errors.New("PROVIDER_LOCKED")
	}
	// 旧的分步更新接口不能把现有密钥带到新地址；改址需使用统一保存入口重新录入。
	if updated.Kind == api.ProviderKindAPIKey && !sameBase(updated.BaseURL, s.disk.Providers[index].BaseURL) && s.creds != nil {
		key, err := s.creds.Get(id)
		if err != nil && !errors.Is(err, platform.ErrNotFound) {
			return api.Provider{}, errors.New("CREDENTIAL_UNAVAILABLE")
		}
		if key != "" {
			return api.Provider{}, errors.New("MODEL_QUERY_KEY_REQUIRED")
		}
	}
	// 默认供应商必须始终可路由，否则重启校验会拒绝已保存状态。
	if id == s.disk.Config.DefaultProviderID && !providerUsable(updated) {
		return api.Provider{}, errors.New("PROVIDER_DEFAULT")
	}
	// 响应视图要在写盘前生成；凭据读取失败时不能写入后再把成功变更报成失败。
	view, err := s.view(updated, locked)
	if err != nil {
		return api.Provider{}, err
	}
	next := s.disk
	next.Providers = append([]providerRecord{}, s.disk.Providers...)
	next.Providers[index] = updated
	if err = s.store.Write("state.json", next); err != nil {
		return api.Provider{}, errors.New("STORAGE_WRITE_FAILED")
	}
	s.disk = next
	return view, nil
}

// deleteProvider 删除未被绑定、非内置且非默认的供应商，并清理其凭据。
func (s *state) deleteProvider(id string) (api.Providers, error) {
	if id == BuiltinOfficialID {
		return api.Providers{}, errors.New("PROVIDER_BUILTIN")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	index := -1
	for i, p := range s.disk.Providers {
		if p.ID == id {
			index = i
		}
	}
	if index < 0 {
		return api.Providers{}, errors.New("PROVIDER_NOT_FOUND")
	}
	if s.disk.Config.DefaultProviderID == id {
		return api.Providers{}, errors.New("PROVIDER_DEFAULT")
	}
	counts := bindingCounts(s.disk.Bindings)
	if counts[id] > 0 {
		return api.Providers{}, errors.New("PROVIDER_BOUND")
	}
	rec := s.disk.Providers[index]
	next := s.disk
	next.Providers = append(append([]providerRecord{}, s.disk.Providers[:index]...), s.disk.Providers[index+1:]...)
	// 删除前生成剩余供应商视图，并按实际绑定数恢复锁定状态；
	// 凭据读取失败时不删除，避免把失败报在已经生效的变更上。
	items := make([]api.Provider, 0, len(next.Providers))
	for _, p := range next.Providers {
		item, err := s.view(p, counts[p.ID] > 0)
		if err != nil {
			return api.Providers{}, err
		}
		items = append(items, item)
	}
	// 快照待删密钥，写盘失败时用它补偿，避免供应商仍在状态文件里却丢了密钥。
	var previous string
	restorable := false
	if rec.Kind == api.ProviderKindAPIKey && s.creds != nil {
		value, err := s.creds.Get(rec.ID)
		if err != nil && !errors.Is(err, platform.ErrNotFound) {
			return api.Providers{}, errors.New("CREDENTIAL_UNAVAILABLE")
		}
		if err == nil && value != "" {
			previous, restorable = value, true
		}
		if err := s.creds.Delete(rec.ID); err != nil && !errors.Is(err, platform.ErrNotFound) {
			return api.Providers{}, errors.New("CREDENTIAL_UNAVAILABLE")
		}
	}
	if err := s.store.Write("state.json", next); err != nil {
		if restorable {
			if restoreErr := s.creds.Set(rec.ID, previous); restoreErr != nil {
				// 供应商仍存在但密钥已丢失，必须要求重新录入，不能谎称原配置完全有效。
				return api.Providers{}, errors.New("CREDENTIAL_RESTORE_FAILED")
			}
		}
		return api.Providers{}, errors.New("STORAGE_WRITE_FAILED")
	}
	s.disk = next
	return api.Providers{Items: items}, nil
}

// credentialStatus 只暴露“是否已配置”，不读取或回传密钥内容。
func (s *state) credentialStatus(id string) (api.CredentialStatus, error) {
	rec, ok := s.providerRecord(id)
	if !ok {
		return api.CredentialStatus{}, errors.New("PROVIDER_NOT_FOUND")
	}
	if rec.Kind != api.ProviderKindAPIKey {
		return api.CredentialStatus{}, errors.New("PROVIDER_NOT_API_KEY")
	}
	configured := false
	if s.creds != nil {
		if v, err := s.creds.Get(rec.ID); err == nil && v != "" {
			configured = true
		} else if err != nil && !errors.Is(err, platform.ErrNotFound) {
			return api.CredentialStatus{}, errors.New("CREDENTIAL_UNAVAILABLE")
		}
	}
	return api.CredentialStatus{ProviderID: rec.ID, Configured: configured}, nil
}

// setProviderKey 保存密钥；凭据名称就是供应商标识，沿用旧公司命名空间。
// 存在检查与凭据写入持有同一把状态锁，避免并发删除供应商后遗留孤立凭据。
func (s *state) setProviderKey(id, key string) (api.CredentialStatus, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, ok := findProvider(s.disk.Providers, id)
	if !ok {
		return api.CredentialStatus{}, errors.New("PROVIDER_NOT_FOUND")
	}
	if rec.Kind != api.ProviderKindAPIKey {
		return api.CredentialStatus{}, errors.New("PROVIDER_NOT_API_KEY")
	}
	if s.creds == nil {
		return api.CredentialStatus{}, errors.New("CREDENTIAL_UNAVAILABLE")
	}
	if err := s.creds.Set(rec.ID, key); err != nil {
		return api.CredentialStatus{}, errors.New("CREDENTIAL_UNAVAILABLE")
	}
	return api.CredentialStatus{ProviderID: rec.ID, Configured: true}, nil
}

// deleteProviderKey 幂等删除密钥；与供应商删除互斥，避免并发下留下孤立凭据。
func (s *state) deleteProviderKey(id string) (api.CredentialStatus, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, ok := findProvider(s.disk.Providers, id)
	if !ok {
		return api.CredentialStatus{}, errors.New("PROVIDER_NOT_FOUND")
	}
	if rec.Kind != api.ProviderKindAPIKey {
		return api.CredentialStatus{}, errors.New("PROVIDER_NOT_API_KEY")
	}
	if s.creds != nil {
		if err := s.creds.Delete(rec.ID); err != nil && !errors.Is(err, platform.ErrNotFound) {
			return api.CredentialStatus{}, errors.New("CREDENTIAL_UNAVAILABLE")
		}
	}
	return api.CredentialStatus{ProviderID: rec.ID, Configured: false}, nil
}

// credentialStatuses 列出全部密钥供应商的配置状态；读取失败显式返回错误。
func (s *state) credentialStatuses() (api.ProviderCredentials, error) {
	s.mu.Lock()
	records := append([]providerRecord{}, s.disk.Providers...)
	s.mu.Unlock()
	items := make([]api.CredentialStatus, 0, len(records))
	for _, p := range records {
		if p.Kind != api.ProviderKindAPIKey {
			continue
		}
		status, err := s.credentialStatus(p.ID)
		if err != nil {
			return api.ProviderCredentials{}, err
		}
		items = append(items, status)
	}
	return api.ProviderCredentials{Items: items}, nil
}

// resolve 返回已绑定会话的供应商；未知会话进入待登记。
func (s *state) resolve(id string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if b, ok := s.disk.Bindings[id]; ok {
		return b.ProviderID, nil
	}
	if _, ok := s.pending[id]; !ok {
		if len(s.pending) >= 256 {
			return "", errors.New("PENDING_LIMIT")
		}
		pending := api.PendingSession{ID: id, FirstSeen: time.Now().UTC().Format(time.RFC3339Nano), Reason: "没有可验证的新建或历史来源证据，请首次确认供应商"}
		if s.history != nil {
			if err := s.history.PutPending(pending); err != nil {
				s.historyFailed()
				return "", errors.New("STORAGE_WRITE_FAILED")
			}
		}
		s.pending[id] = pending
	}
	return "", errors.New("SESSION_REGISTRATION_REQUIRED")
}

// bindLocked 写入唯一绑定；写盘成功后才发布。
func (s *state) bindLocked(id, providerID string) (api.Binding, error) {
	b := api.Binding{ID: id, ProviderID: providerID, CreatedAt: time.Now().UTC().Format(time.RFC3339)}
	next := s.disk
	next.Bindings = make(map[string]api.Binding, len(s.disk.Bindings)+1)
	for k, v := range s.disk.Bindings {
		next.Bindings[k] = v
	}
	next.Bindings[id] = b
	if err := s.store.Write("state.json", next); err != nil {
		return api.Binding{}, errors.New("STORAGE_WRITE_FAILED")
	}
	s.disk = next
	if s.history != nil {
		if err := s.history.Bind(b); err != nil {
			s.historyDirty = true
			s.historyFailed()
		}
	}
	delete(s.pending, id)
	return b, nil
}

// register 只允许首次确认供应商，不允许改线。
func (s *state) register(in api.RegisterInput) (api.Binding, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !validID(in.ID) || !validProviderID(in.ProviderID) {
		return api.Binding{}, errors.New("INVALID_INPUT")
	}
	if b, ok := s.disk.Bindings[in.ID]; ok {
		if b.ProviderID != in.ProviderID {
			return b, errors.New("SESSION_ALREADY_BOUND")
		}
		return b, nil
	}
	if _, ok := s.pending[in.ID]; !ok {
		return api.Binding{}, errors.New("SESSION_NOT_PENDING")
	}
	rec, ok := findProvider(s.disk.Providers, in.ProviderID)
	if !ok || !providerUsable(rec) {
		return api.Binding{}, errors.New("UPSTREAM_NOT_CONFIGURED")
	}
	if len(s.disk.Bindings) >= 10000 {
		return api.Binding{}, errors.New("BINDING_LIMIT")
	}
	return s.bindLocked(in.ID, in.ProviderID)
}

// resolveEvidence 只在证据可靠时自动绑定新会话或已知父线程的 fork。
func (s *state) resolveEvidence(e threadEvidence) (string, error) {
	if !validID(e.ThreadID) || (e.Parent != "" && (!validID(e.Parent) || e.Parent == e.ThreadID)) {
		return "", errors.New("SESSION_ID_CONFLICT")
	}
	s.mu.Lock()
	if b, ok := s.disk.Bindings[e.ThreadID]; ok {
		s.mu.Unlock()
		return b.ProviderID, nil
	}
	var providerID string
	if e.History == "new" && e.Parent == "" {
		providerID = s.disk.Config.DefaultProviderID
	}
	// CLI cold_fork 与独立 app-server warm_fork 均已实测明确父线程；ephemeral 尚无请求证据。
	if (e.History == "cold_fork" || e.History == "warm_fork") && e.Parent != "" {
		if parent, ok := s.disk.Bindings[e.Parent]; ok {
			providerID = parent.ProviderID
		}
	}
	if providerID != "" {
		rec, ok := findProvider(s.disk.Providers, providerID)
		if !ok || !providerUsable(rec) {
			s.mu.Unlock()
			return "", errors.New("UPSTREAM_NOT_CONFIGURED")
		}
		if len(s.disk.Bindings) >= 10000 {
			s.mu.Unlock()
			return "", errors.New("BINDING_LIMIT")
		}
		b, err := s.bindLocked(e.ThreadID, providerID)
		if err != nil {
			s.mu.Unlock()
			return "", err
		}
		s.mu.Unlock()
		return b.ProviderID, nil
	}
	s.mu.Unlock()
	return s.resolve(e.ThreadID)
}

func (s *state) sessions() api.Sessions {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := api.Sessions{Bindings: []api.Binding{}, Pending: []api.PendingSession{}}
	for _, b := range s.disk.Bindings {
		out.Bindings = append(out.Bindings, b)
	}
	for _, p := range s.pending {
		out.Pending = append(out.Pending, p)
	}
	sort.Slice(out.Bindings, func(i, j int) bool { return out.Bindings[i].CreatedAt > out.Bindings[j].CreatedAt })
	sort.Slice(out.Pending, func(i, j int) bool { return out.Pending[i].FirstSeen > out.Pending[j].FirstSeen })
	return out
}

// providerIDs 返回按保存顺序排列的供应商标识，供状态接口列出最近结果。
func (s *state) providerIDs() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	ids := make([]string, 0, len(s.disk.Providers))
	for _, p := range s.disk.Providers {
		ids = append(ids, p.ID)
	}
	return ids
}
