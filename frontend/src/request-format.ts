import type { RequestActivity, RequestMetrics } from './api/types.gen'

/** 指标缺失与真实0分开显示，防止把未采集数据当成免费或无耗时。 */
export function metric(item: RequestActivity, key: keyof Omit<RequestMetrics, 'known_fields'>): number | undefined {
  return item.metrics.known_fields.includes(key) ? item.metrics[key] : undefined
}

/** 使用千分位或紧凑缓存格式，精确值另在明细中展示。 */
export function tokens(value: number | undefined, compact = false): string {
  if (value === undefined || !Number.isFinite(value)) return '—'
  if (compact && value >= 1_000_000) return `${(value / 1_000_000).toFixed(1)}M`
  if (compact && value >= 1_000) return `${(value / 1_000).toFixed(1)}K`
  return value.toLocaleString('en-US')
}

/** 按时长自动选择单位，避免用户手动换算毫秒。 */
export function duration(ms: number | undefined): string {
  if (ms === undefined || !Number.isFinite(ms) || ms < 0) return '—'
  if (ms < 1000) return `${ms}ms`
  if (ms < 60_000) return `${(ms / 1000).toFixed(2)}s`
  const seconds = Math.round(ms / 1000)
  if (seconds < 3600) return `${Math.floor(seconds / 60)}m ${seconds % 60}s`
  return `${Math.floor(seconds / 3600)}h ${Math.floor(seconds % 3600 / 60)}m`
}

/** 参考sub2api固定版本的毫秒阈值，文字提示与分档共享同一份定义。 */
export const latencyThresholds = { first: [10000, 30000, 60000], duration: [60000, 180000, 300000] } as const

/** 分档说明用于悬停与手机精确明细；数值由集中阈值推导。 */
export function latencyHint(kind: 'first' | 'duration'): string {
  const [warn, slow, critical] = latencyThresholds[kind]
  const scale = kind === 'first' ? 1000 : 60000
  const unit = kind === 'first' ? '秒' : '分钟'
  return `${kind === 'first' ? '首字' : '总耗时'}：绿＜${warn / scale}${unit}，黄${warn / scale}～不足${slow / scale}${unit}，橙${slow / scale}～不足${critical / scale}${unit}，红≥${critical / scale}${unit}`
}

/** 首字与总耗时按各自阈值分档；未知不计为正常。 */
export function latencySeverity(ms: number | undefined, kind: 'first' | 'duration'): string {
  if (ms === undefined || !Number.isFinite(ms) || ms < 0) return 'unknown'
  const [warn, slow, critical] = latencyThresholds[kind]
  return ms >= critical! ? 'critical' : ms >= slow! ? 'slow' : ms >= warn! ? 'warn' : 'good'
}

/** 非缓存输入仅在三个互斥桶可核实时计算，不把未报告缓存当作0。 */
export function nonCachedInput(item: RequestActivity): number | undefined {
  const input = metric(item, 'input_tokens'), read = metric(item, 'cache_read_tokens'), write = metric(item, 'cache_write_tokens')
  if ([input, read, write].some(value => value === undefined || !Number.isSafeInteger(value) || value < 0)) return undefined
  const result = input! - read! - write!
  return result < 0 ? undefined : result
}

/** 展示本地时区完整时间；只有原记录存在小数秒时才展示毫秒。 */
export function formatTimestamp(value: string, precise = false): string {
  const date = new Date(value)
  if (!Number.isFinite(date.getTime())) return '—'
  const pad = (n: number, length = 2) => String(n).padStart(length, '0')
  const time = `${date.getFullYear()}/${pad(date.getMonth() + 1)}/${pad(date.getDate())} ${pad(date.getHours())}:${pad(date.getMinutes())}:${pad(date.getSeconds())}`
  return precise && /\.\d+(?:Z|[+-]\d{2}:\d{2})$/i.test(value) ? `${time}.${pad(date.getMilliseconds(), 3)}` : time
}

/** 有协议终止时间时排除上游拖延关闭的收尾等待，旧记录回退连接耗时。 */
export function requestDuration(item: RequestActivity): number | undefined {
  if (item.phase !== 'finished') return undefined
  return metric(item, 'protocol_completed_ms') ?? item.elapsed_ms
}

/** 与参考表格一致，输出速率使用完整总耗时，可能包含推理token。 */
export function outputRate(item: RequestActivity): string {
  if (item.request_type === 'auxiliary') return '不适用'
  const output = metric(item, 'output_tokens')
  const ms = requestDuration(item)
  if (ms === undefined || output === undefined || output < 0 || ms <= 0) return '—'
  return `${(output * 1000 / ms).toFixed(1)} tok/s`
}

/** 固定错误分类采用中文说明，原始上游错误不进入界面。 */
export function resultLabel(item: RequestActivity): string {
  const labels: Record<string, string> = {
    RESPONSE_COMPLETED: '生成完成', RESPONSE_FAILED: '上游生成失败', RESPONSE_INCOMPLETE: '生成未完成', EMPTY_RESPONSE: '上游返回空结果',
    REQUEST_CANCELED: '请求已取消', STREAM_INTERRUPTED: '响应流中断', STREAM_IDLE_TIMEOUT: '响应等待超时',
    MISSING_TERMINAL: '未收到完成事件', OBSERVATION_UNAVAILABLE: '无法确认响应结果', RESULT_UNCONFIRMED: '结果待确认',
    PROCESS_INTERRUPTED: '程序退出时请求未结束', ROUTER_SHUTDOWN: '路由器退出', CLIENT_WRITE_FAILED: '客户端接收中断',
    SESSION_UNKNOWN: '会话待登记', SESSION_REGISTRATION_REQUIRED: '会话待登记', SESSION_ID_REQUIRED: '缺少会话标识', SESSION_ID_CONFLICT: '会话标识冲突',
    SESSION_METADATA_INVALID: '会话信息无效', UPSTREAM_TIMEOUT: '上游连接或响应超时', UPSTREAM_UNREACHABLE: '上游不可达',
    WEBSOCKET_UNSUPPORTED: 'WebSocket 握手未支持', REQUEST_BODY_INVALID: '请求正文无效或超限',
    SESSION_PROVIDER_MISSING: '会话供应商不存在', INVALID_PATH: '请求地址无效', INVALID_INPUT: '请求参数无效',
    UPSTREAM_DNS_FAILED: '上游域名解析失败', UPSTREAM_TLS_FAILED: '上游证书验证失败', UPSTREAM_CONNECTION_CLOSED: '上游提前断开连接',
    CREDENTIAL_UNAVAILABLE: '供应商凭据不可用', UPSTREAM_NOT_CONFIGURED: '供应商尚未配置', CONCURRENCY_LIMIT: '达到并发上限',
    NO_RESPONSE: '未产生响应', '处理中': '处理中',
  }
  if (labels[item.result]) return labels[item.result]!
  if (item.result.startsWith('HTTP_')) return item.status >= 200 && item.status < 300 ? (item.request_type === 'auxiliary' ? '辅助请求已返回' : '仅确认 HTTP 返回') : 'HTTP 请求失败'
  return '请求未完成'
}

/** 记录中的稳定阶段映射为便于排查的名称。 */
export const stages: Record<string, string> = { routing: '本地路由', upstream: '上游连接', stream: '响应流', client: '客户端接收', process: '程序运行', observation: '结果识别' }

/** 逐字段known_fields仍是数值依据；状态只解释缺失原因。 */
export function metricsExplanation(item: RequestActivity): string {
  if (item.request_type === 'auxiliary') return '不适用：辅助请求没有生成首字、输出 Token 或 TPS。'
  const status = item.observation?.metrics_status || 'legacy'
  if (status === 'pending' || item.phase === 'received') return '请求处理中，已取得的指标保留，完整用量等待上游返回。'
  if (status === 'legacy') return '旧记录缺少采集信息；已保存的指标保留，缺失项无法补造。'
  if (status === 'unavailable') return '解析未完成：响应格式、编码、截断或观察上限使统计不完整，已取得的指标仍保留。'
  if (status === 'not_reported') return '上游未返回可用的生成用量或首字事件。'
  if (status === 'partial') return '上游只返回了部分指标；未报告项以“—”显示，不视为 0。'
  if (nonCachedInput(item) === undefined) return '缓存读取或写入未完整报告，无法计算非缓存输入；原始输入总量可在明细查看。'
  return '已采集生成用量；缓存属于输入，推理属于输出，不重复计入总量。'
}

/** 展示传输收尾事实，避免把正常终止后的客户端关闭误解为生成失败。 */
export function transportLabel(item: RequestActivity): string {
  const value = item.observation?.transport_result
  const labels: Record<string, string> = { EOF: '连接正常结束', REQUEST_CANCELED: '客户端关闭连接', CLIENT_WRITE_FAILED: '客户端写入或刷新失败', STREAM_INTERRUPTED: '上游响应流中断', STREAM_IDLE_TIMEOUT: '等待上游超时', ROUTER_SHUTDOWN: '路由器关闭' }
  return value ? (labels[value] || value) : '旧记录未保存'
}

/** 简短状态留在表格，完整解释可在信息按钮及精确明细中查看。 */
export function metricsStatusLabel(item: RequestActivity): string {
  const status = item.observation?.metrics_status || 'legacy'
  const labels: Record<string, string> = { legacy: '旧记录缺少指标', unavailable: '解析未完成', not_reported: '上游未返回指标', partial: '部分指标未返回', pending: '等待生成指标' }
  return labels[status] || (status === 'available' && nonCachedInput(item) === undefined ? '缓存明细不完整' : '')
}
