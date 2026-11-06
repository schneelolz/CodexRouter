// 由 scripts/generate-contract.mjs 生成，请勿手改。

/** DailyStatus */
export interface DailyStatus {
  state: string
  config_path: string
  base_url: string
  has_backup: boolean
  message: string
}

/** DailyAction */
export interface DailyAction {
  action: "route" | "restore"
}

/** DailyApply */
export interface DailyApply {
  token: string
}

/** DailyPreview */
export interface DailyPreview {
  token: string
  action: string
  config_path: string
  before: string
  after: string
  expires_at: string
  warning: string
}

/** RequestActivity */
export interface RequestActivity {
  id: number
  time: string
  entry: string
  thread_id: string
  provider_id: string
  phase: string
  result: string
  status: number
  elapsed_ms: number
  outcome: string
  model: string
  request_type: string
  error_stage: string
  metrics: RequestMetrics
  observation: RequestObservation
}

/** Activity */
export interface Activity {
  received: number
  items: Array<RequestActivity>
  degraded: boolean
}

/** ProviderSaveInput */
export interface ProviderSaveInput {
  provider_id: string
  name: string
  base_url: string
  model: string
  key: string
}

/** ModelQueryInput */
export interface ModelQueryInput {
  provider_id: string
  base_url: string
  key: string
}

/** ModelItem */
export interface ModelItem {
  id: string
}

/** Models */
export interface Models {
  items: Array<ModelItem>
}

/** Client */
export interface Client {
  target: "cli" | "desktop"
  installed: boolean
  version: string
  state: string
  mode: "routed" | "direct"
  profile: string
  message: string
}

/** Clients */
export interface Clients {
  items: Array<Client>
}

/** ClientAction */
export interface ClientAction {
  target: "cli" | "desktop"
  action: "start" | "login" | "check" | "stop" | "direct" | "routed"
}

export type ProviderKind = "official" | "api_key"

/** 定义已保存的供应商、模型覆盖和当前可用状态。 */
export interface Provider {
  id: string
  name: string
  kind: ProviderKind
  base_url: string
  model: string
  key_configured: boolean
  upstream_locked: boolean
}

/** 列出全部供应商。 */
export interface Providers {
  items: Array<Provider>
}

export type ProviderCreateKind = "api_key"

/** 新建供应商；标识由后端生成，避免复用旧凭据命名空间。 */
export interface ProviderCreateInput {
  name: string
  kind: ProviderCreateKind
  base_url: string
  model: string
}

/** 更新供应商；标识与认证方式不可修改，有绑定后基址锁定。 */
export interface ProviderUpdateInput {
  name: string
  base_url: string
  model: string
}

/** 定义非敏感路由配置；供应商单独管理。 */
export interface Config {
  version: 2
  default_provider_id: string
  connect_timeout_ms: number
  header_timeout_ms: number
  idle_timeout_ms: number
  write_timeout_ms: number
  max_concurrent: number
}

/** 保存默认供应商与超时；供应商本身通过供应商接口维护。 */
export interface ConfigInput {
  default_provider_id: string
  connect_timeout_ms: number
  header_timeout_ms: number
  idle_timeout_ms: number
  write_timeout_ms: number
  max_concurrent: number
}

/** 说明管理 API 兼容版本和当前协议边界。 */
export interface Capabilities {
  api_version: string
  protocol_status: string
  transports: Array<string>
}

/** 表示某个供应商最近一次请求结果，不能代表当前可达性。 */
export interface ProviderStatus {
  provider_id: string
  result: "unverified" | "success" | "failure"
  checked_at: string
  code: string
}

/** 表示本地状态及每个供应商的最近结果。 */
export interface Status {
  local: string
  active_requests: number
  upstreams: Array<ProviderStatus>
}

/** 设置某个密钥供应商的 API 密钥，仅用于写入。 */
export interface CredentialInput {
  key: string
}

/** 只返回指定供应商的密钥是否已配置。 */
export interface CredentialStatus {
  provider_id: string
  configured: boolean
}

/** 列出密钥供应商的配置状态，不含密钥内容。 */
export interface ProviderCredentials {
  items: Array<CredentialStatus>
}

/** 定义持久化的固定会话绑定。 */
export interface Binding {
  id: string
  provider_id: string
  created_at: string
}

/** 定义尚未确认来源的会话。 */
export interface PendingSession {
  id: string
  first_seen: string
  reason: string
}

/** 列出已绑定与待登记会话。 */
export interface Sessions {
  bindings: Array<Binding>
  pending: Array<PendingSession>
}

/** 只允许首次确认供应商，不允许改线。 */
export interface RegisterInput {
  id: string
  provider_id: string
}

/** 定义无正文及凭据的近期诊断。 */
export interface Diagnostic {
  time: string
  code: string
  provider_id: string
}

/** 返回有界诊断集合。 */
export interface Diagnostics {
  items: Array<Diagnostic>
}

/** 定义稳定错误码与可读说明。 */
export interface ErrorDetail {
  code: string
  message: string
}

/** 统一管理接口错误格式。 */
export interface ErrorResponse {
  error: ErrorDetail
}

/** 交换一次性引导令牌。 */
export interface AuthInput {
  token: string
}

/** 返回当前会话的 CSRF 令牌，仅保存在页面内存。 */
export interface AuthSession {
  csrf_token: string
}

/** 脱敏请求历史的数据库游标分页；HTTP正常不等于语义完成 */
export interface RequestPage {
  items: Array<RequestActivity>
  next_cursor: string
  total: number
  degraded: boolean
  page: number
  total_pages: number
  snapshot: string
}

/** 独立的会话查询页，不等同于请求列表 */
export interface SessionPage {
  bindings: Array<Binding>
  pending: Array<PendingSession>
  next_cursor: string
  total: number
  kind: "bound" | "pending"
}

/** 只影响已结束请求的滚动天数；0为不限期 */
export interface HistoryPolicy {
  days: number
}

/** 用户明确确认清除已结束请求；不影响绑定及待登记 */
export interface ClearHistoryInput {
  confirm: boolean
}

/** 本次删除的已结束请求数量 */
export interface ClearHistoryResult {
  deleted: number
}

/** 只保存用量与时间元数据，不保存原始事件；缓存及推理子集不得重复计入总量。 */
export interface RequestMetrics {
  input_tokens: number
  output_tokens: number
  total_tokens: number
  cache_read_tokens: number
  cache_write_tokens: number
  reasoning_tokens: number
  first_token_ms: number
  known_fields: Array<string>
  protocol_completed_ms: number
}

/** 保存协议终止与传输结束的独立脱敏事实；旧记录不反推结果或用量。 */
export interface RequestObservation {
  terminal_result: string
  transport_result: string
  metrics_status: string
}
