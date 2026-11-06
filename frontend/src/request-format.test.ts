import { describe, expect, it } from 'vitest'
import { duration, metric, outputRate, tokens, latencySeverity, nonCachedInput, formatTimestamp, requestDuration, resultLabel, metricsExplanation, metricsStatusLabel, transportLabel, latencyHint, latencyThresholds } from './request-format'
import type { RequestActivity } from './api/types.gen'

describe('请求指标格式与口径', () => {
  const item = { phase: 'finished', elapsed_ms: 26640, metrics: { input_tokens: 0, output_tokens: 630, total_tokens: 0, cache_read_tokens: 0, cache_write_tokens: 0, reasoning_tokens: 0, first_token_ms: 0, known_fields: ['output_tokens', 'cache_read_tokens'] } } as RequestActivity
  it('缺失不冒充0，缓存真实0仍可展示', () => {
    expect(tokens(metric(item, 'input_tokens'))).toBe('—')
    expect(tokens(metric(item, 'cache_read_tokens'))).toBe('0')
    expect(tokens(144746)).toBe('144,746')
    expect(tokens(144400, true)).toBe('144.4K')
    expect(tokens(1200000, true)).toBe('1.2M')
  })
  it('毫秒、秒、分、小时边界友好换算', () => {
    expect(duration(undefined)).toBe('—')
    expect(duration(0)).toBe('0ms')
    expect(duration(999)).toBe('999ms')
    expect(duration(24160)).toBe('24.16s')
    expect(duration(68000)).toBe('1m 8s')
    expect(duration(3660000)).toBe('1h 1m')
  })
  it('TPS使用完整耗时，处理中或缺失时不制造数据', () => {
    expect(outputRate(item)).toBe('23.6 tok/s')
    expect(outputRate({ ...item, phase: 'received' })).toBe('—')
    expect(outputRate({ ...item, elapsed_ms: 0 })).toBe('—')
    expect(outputRate({ ...item, metrics: { ...item.metrics, known_fields: [] } })).toBe('—')
  })
  it('首字6.49秒与总耗时68秒分开为绿/黄，边界各自分档', () => {
    expect(latencySeverity(6490, 'first')).toBe('good')
    expect(latencySeverity(68000, 'duration')).toBe('warn')
    for (const [kind, cuts] of [['first', [10000, 30000, 60000]], ['duration', [60000, 180000, 300000]]] as const) {
      for (const [index, threshold] of cuts.entries()) {
        expect(latencySeverity(threshold, kind)).toBe(['warn', 'slow', 'critical'][index])
        expect(latencySeverity(threshold - 1, kind)).toBe(['good', 'warn', 'slow'][index])
      }
      expect(latencySeverity(undefined, kind)).toBe('unknown')
      expect(latencySeverity(0, kind)).toBe('good')
    }
  })
  it('非缓存输入缺任何一个桶时未知，不把缺失当0或产生负数', () => {
    const complete = { ...item, metrics: { ...item.metrics, input_tokens: 61083, cache_read_tokens: 60900, cache_write_tokens: 0, known_fields: ['input_tokens', 'cache_read_tokens', 'cache_write_tokens'] } }
    expect(nonCachedInput(complete)).toBe(183)
    expect(nonCachedInput({ ...complete, metrics: { ...complete.metrics, known_fields: ['input_tokens', 'cache_read_tokens'] } })).toBeUndefined()
    expect(nonCachedInput({ ...complete, metrics: { ...complete.metrics, input_tokens: 1 } })).toBeUndefined()
  })
  it('日期本地时区补零，毫秒仅在原值存在小数秒时显示', () => {
    const date = new Date(2026, 0, 2, 3, 4, 5, 123)
    expect(formatTimestamp(date.toISOString())).toBe('2026/01/02 03:04:05')
    expect(formatTimestamp(date.toISOString(), true)).toBe('2026/01/02 03:04:05.123')
    expect(formatTimestamp(date.toISOString().replace('.123', ''), true)).toBe('2026/01/02 03:04:05')
    expect(formatTimestamp('invalid')).toBe('—')
  })
  it('明确协议完成与连接收尾耗时分开，TPS排除收尾等待', () => {
    const completed = { ...item, elapsed_ms: 10000, metrics: { ...item.metrics, protocol_completed_ms: 1000, known_fields: ['output_tokens', 'protocol_completed_ms'] } }
    expect(requestDuration(completed)).toBe(1000)
    expect(outputRate(completed)).toBe('630.0 tok/s')
    expect(resultLabel({ ...item, result: 'WEBSOCKET_UNSUPPORTED' })).toBe('WebSocket 握手未支持')
  })
})


describe('指标状态说明', () => {
  const base = { phase: 'finished', request_type: 'stream', elapsed_ms: 1000, metrics: { output_tokens: 0, known_fields: ['output_tokens'] }, observation: { terminal_result: 'unknown', transport_result: 'REQUEST_CANCELED', metrics_status: 'partial' } } as RequestActivity
  it('真实零速率与辅助请求不适用分开', () => {
    expect(outputRate(base)).toBe('0.0 tok/s')
    expect(outputRate({ ...base, request_type: 'auxiliary' })).toBe('不适用')
    expect(metricsExplanation({ ...base, request_type: 'auxiliary' })).toContain('不适用')
  })
  it('未报告、旧记录和解析未完成有独立说明，既有数值不丢弃', () => {
    for (const [status, label] of [['legacy', '旧记录缺少指标'], ['unavailable', '解析未完成'], ['not_reported', '上游未返回指标'], ['partial', '部分指标未返回']]) {
      const item = { ...base, observation: { ...base.observation, metrics_status: status! } }
      expect(metricsStatusLabel(item)).toBe(label)
      expect(metricsExplanation(item)).not.toBe('')
      expect(metric(item, 'output_tokens')).toBe(0)
    }
    expect(transportLabel(base)).toBe('客户端关闭连接')
  })
})


it('延迟说明与sub2api集中阈值保持一致', () => {
  expect(latencyThresholds.first).toEqual([10000, 30000, 60000])
  expect(latencyThresholds.duration).toEqual([60000, 180000, 300000])
  expect(latencyHint('first')).toBe('首字：绿＜10秒，黄10～不足30秒，橙30～不足60秒，红≥60秒')
  expect(latencyHint('duration')).toBe('总耗时：绿＜1分钟，黄1～不足3分钟，橙3～不足5分钟，红≥5分钟')
})
