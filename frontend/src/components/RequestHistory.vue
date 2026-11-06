<script setup lang="ts">
import { computed, nextTick, onMounted, onUnmounted, ref } from 'vue'
import { NAlert, NButton, NModal, NTooltip } from 'naive-ui'
import { ArrowDown, ArrowUp, Database, Info, RefreshCw, ChevronLeft, ChevronRight, Copy, Check } from '@lucide/vue'
import { ApiClient, ApiError } from '../api/client'
import { metric, tokens, duration, outputRate, resultLabel, stages, latencySeverity, nonCachedInput, formatTimestamp, requestDuration, metricsExplanation, metricsStatusLabel, transportLabel, latencyHint } from '../request-format'
import type { RequestActivity, RequestPage } from '../api/types.gen'

const props = defineProps<{ api: ApiClient; writable: boolean; providerName: (id: string) => string }>()
const emit = defineEmits<{ failure: [error: unknown] }>()
const emptyPage = (): RequestPage => ({ items: [], next_cursor: '', total: 0, degraded: false, page: 1, total_pages: 0, snapshot: '' })
const page = ref<RequestPage>(emptyPage())
const limit = ref(20)
const filter = ref('success')
const tabs = [{ id: 'success', label: '用量明细' }, { id: 'error', label: '错误请求' }, { id: 'processing', label: '处理中' }, { id: 'unconfirmed', label: '待确认' }]
const selected = ref<RequestActivity | null>(null)
const tabButtons = ref<HTMLButtonElement[]>([])
const pageNumber = ref(1)
const loading = ref(false)
const error = ref('')
const viewport = ref<HTMLElement>()
const copied = ref('')
const copyError = ref('')
async function copySession(item: RequestActivity) {
  try { await navigator.clipboard.writeText(item.thread_id); copied.value = item.thread_id; copyError.value = '' }
  catch { copyError.value = '无法访问剪贴板，可在明细中选中完整会话 ID 复制。'; selected.value = item }
}
const lifetime = new AbortController()
let request: AbortController | undefined
let generation = 0
let timer: ReturnType<typeof setTimeout> | undefined
const shownPages = computed(() => Math.max(1, page.value.total_pages))
const jump = ref(1)
const pageButtons = computed(() => {
  const first = Math.max(1, Math.min(page.value.page - 2, shownPages.value - 4))
  return Array.from({ length: Math.min(5, shownPages.value) }, (_, index) => first + index)
})
const typeLabels: Record<string, string> = { stream: '流式', sync: '非流式', auxiliary: '辅助请求', unknown: '类型待确认' }
const exactFields = [['input_tokens', '输入总量（含缓存）'], ['output_tokens', '输出 Token'], ['total_tokens', '总 Token'], ['cache_read_tokens', '缓存命中'], ['cache_write_tokens', '缓存写入'], ['reasoning_tokens', '推理 Token']] as const

function failure(value: unknown) {
  error.value = value instanceof Error ? value.message : '历史读取失败，请重试。'
  if (value instanceof ApiError && ([401, 403].includes(value.status) || value.code === 'BACKEND_UNREACHABLE')) emit('failure', value)
}
async function load(fresh = false, replace = false) {
  if (lifetime.signal.aborted || (loading.value && !replace)) return
  request?.abort()
  const current = new AbortController()
  request = current
  const version = ++generation
  const signal = AbortSignal.any([lifetime.signal, current.signal])
  loading.value = true
  try {
    const next = await props.api.requestPage(limit.value, pageNumber.value, fresh ? '' : page.value.snapshot, filter.value, signal)
    if (version !== generation || signal.aborted) return
    page.value = next; pageNumber.value = next.page; jump.value = next.page; error.value = ''
  } catch (value) { if (!signal.aborted && version === generation) failure(value) }
  finally { if (version === generation) loading.value = false }
}
async function reset() { pageNumber.value = 1; page.value = { ...emptyPage(), degraded: page.value.degraded }; await load(true, true); await nextTick(); viewport.value?.scrollTo({ top: 0, left: 0 }) }
async function switchTab(id: string) {
  if (id === filter.value) return
  filter.value = id; selected.value = null; error.value = ''; await reset()
}
function tabKey(event: KeyboardEvent, index: number) {
  let next = index
  if (event.key === 'ArrowRight') next = (index + 1) % tabs.length
  else if (event.key === 'ArrowLeft') next = (index + tabs.length - 1) % tabs.length
  else if (event.key === 'Home') next = 0
  else if (event.key === 'End') next = tabs.length - 1
  else return
  event.preventDefault(); tabButtons.value[next]?.focus(); void switchTab(tabs[next]!.id)
}
async function changePage(value: number) { pageNumber.value = value; await load(); await nextTick(); viewport.value?.scrollTo({ top: 0 }) }
async function changeSize(value: number) { limit.value = value; await reset() }
async function jumpPage() { if (Number.isInteger(jump.value) && jump.value >= 1 && jump.value <= shownPages.value) await changePage(jump.value) }
function schedule() {
  timer = setTimeout(async () => {
    if (document.visibilityState === 'visible' && !selected.value) await load(pageNumber.value === 1)
    if (!lifetime.signal.aborted) schedule()
  }, 5000)
}
onMounted(() => { void load(true); schedule() })
onUnmounted(() => { lifetime.abort(); request?.abort(); clearTimeout(timer) })
</script>

<template>
  <section class="requests-view" aria-label="请求明细" :aria-busy="loading">
    <NAlert v-if="page.degraded" type="warning" class="feedback">本轮历史存储发生过故障，记录可能不完整。固定路由绑定保留。</NAlert>
    <NAlert v-if="error" type="error" class="feedback" role="alert">{{ error }} <NButton text @click="load()">重试读取</NButton></NAlert>
    <div class="request-controls">
      <div class="request-tabs" role="tablist" aria-label="请求分类">
        <button v-for="(tab, index) in tabs" :id="`request-tab-${tab.id}`" :key="tab.id" ref="tabButtons" type="button" role="tab" :aria-selected="filter === tab.id" aria-controls="request-panel" :tabindex="filter === tab.id ? 0 : -1" @click="switchTab(tab.id)" @keydown="tabKey($event, index)">{{ tab.label }}</button>
      </div>
      <NTooltip><template #trigger><NButton quaternary circle aria-label="刷新记录" :disabled="loading" @click="reset"><RefreshCw :size="17" aria-hidden="true" /></NButton></template>刷新记录</NTooltip>
    </div>
    <div id="request-panel" role="tabpanel" :aria-labelledby="`request-tab-${filter}`" tabindex="0">
      <div ref="viewport" class="table-scroll data-viewport" tabindex="0" role="region" aria-label="请求明细，可横向滚动">
        <table class="data-table request-table usage-table compact-table"><colgroup><col style="width:8%" /><col style="width:16%" /><col style="width:10%" /><col style="width:14%" /><col style="width:15%" /><col style="width:14%" /><col style="width:9%" /><col style="width:14%" /></colgroup><thead><tr><th scope="col">供应商</th><th scope="col">会话 ID</th><th scope="col">模型</th><th scope="col">请求</th><th scope="col">Token</th><th scope="col">延迟</th><th scope="col">结果</th><th scope="col">时间</th></tr></thead>
          <tbody><tr v-for="item in page.items" :key="item.id" :data-outcome="item.outcome">
            <td><span class="truncate" :title="item.provider_id ? providerName(item.provider_id) : '未选择供应商'">{{ item.provider_id ? providerName(item.provider_id) : '未选择供应商' }}</span></td>
            <td><div class="session-id"><span class="truncate" :title="item.thread_id || '无会话标识'">{{ item.thread_id || '无会话标识' }}</span><button v-if="item.thread_id" type="button" class="copy-id" :aria-label="`复制 ${item.thread_id}`" title="复制完整会话 ID" @click="copySession(item)"><Check v-if="copied === item.thread_id" :size="14" aria-hidden="true" /><Copy v-else :size="14" aria-hidden="true" /></button></div></td>
            <td><strong class="truncate" :title="item.model || '模型未报告'">{{ item.request_type === 'auxiliary' ? '模型目录' : item.model || '模型未报告' }}</strong></td>
            <td><div class="request-entry"><span class="truncate" :title="item.entry">{{ item.entry }}</span><span class="request-kind" :title="typeLabels[item.request_type]">{{ item.request_type === 'unknown' ? '待确认' : typeLabels[item.request_type] || '待确认' }}</span></div></td>
            <td><div class="request-cell request-token-cell"><span v-if="item.request_type === 'auxiliary'" class="metric-unavailable">不适用</span><div v-else class="token-lines"><div class="token-pair"><span class="token-input" :title="`非缓存输入：${tokens(nonCachedInput(item))}`"><ArrowDown :size="14" aria-hidden="true" /><span class="sr-only">非缓存输入 </span><b>{{ tokens(nonCachedInput(item)) }}</b></span><span class="token-output" :title="`输出：${tokens(metric(item, 'output_tokens'))}`"><ArrowUp :size="14" aria-hidden="true" /><span class="sr-only">输出 </span><b>{{ tokens(metric(item, 'output_tokens')) }}</b></span></div><div class="token-cache" :title="`缓存命中：${tokens(metric(item, 'cache_read_tokens'))}`"><Database :size="13" aria-hidden="true" /><span class="sr-only">缓存命中 </span>{{ tokens(metric(item, 'cache_read_tokens'), true) }}</div><span v-if="metricsStatusLabel(item)" class="metric-status" :title="metricsExplanation(item)">{{ metricsStatusLabel(item) }}</span></div><NTooltip><template #trigger><button type="button" class="request-info-button" :aria-label="`查看请求 ${item.id} 用量明细`" @click="selected = item"><Info :size="15" aria-hidden="true" /></button></template>{{ metricsExplanation(item) }} 点击查看精确明细。</NTooltip></div></td>
            <td><div class="request-cell latency-cell" :title="`${latencyHint('first')}；${latencyHint('duration')}`"><span class="latency-bar" aria-hidden="true"><span :class="`severity-${latencySeverity(metric(item, 'first_token_ms'), 'first')}`" /><span :class="`severity-${latencySeverity(requestDuration(item), 'duration')}`" /></span><dl class="latency-metrics"><dt :title="latencyHint('first')">首字</dt><dd :class="`latency-${latencySeverity(metric(item, 'first_token_ms'), 'first')}`">{{ item.request_type === 'auxiliary' || item.request_type === 'sync' ? '不适用' : duration(metric(item, 'first_token_ms')) }}</dd><dt :title="latencyHint('duration')">总耗时</dt><dd :class="`latency-${latencySeverity(requestDuration(item), 'duration')}`">{{ duration(requestDuration(item)) }}</dd><dt>输出 TPS</dt><dd>{{ outputRate(item) }}</dd></dl></div></td>
            <td><div class="request-cell result-content" :title="`${resultLabel(item)} · HTTP ${item.status} · ${item.result}`"><span class="request-result truncate" :class="`outcome-${item.outcome}`">{{ resultLabel(item) }}</span><span v-if="item.status" class="request-subline">HTTP {{ item.status }}</span></div></td>
            <td><time :datetime="item.time" :title="formatTimestamp(item.time, true)">{{ formatTimestamp(item.time) }}</time></td>
          </tr><tr v-if="!page.items.length"><td colspan="8" class="table-empty"><template v-if="loading">正在读取请求记录…</template><template v-else-if="filter === 'success'"><strong>暂无确认成功的生成请求</strong><p>这里仅显示已确认完成的生成请求，辅助请求和缺少完成证据的记录在其他标签。</p><div class="empty-request-actions"><NButton secondary @click="switchTab('error')">查看错误请求</NButton><NButton secondary @click="switchTab('unconfirmed')">查看待确认请求</NButton></div></template><template v-else>暂无匹配的请求记录</template></td></tr></tbody>
        </table>
      </div>
      <span class="sr-only" role="status">{{ copied ? '会话 ID 已复制' : '' }}</span><div class="request-pagination list-pagination"><span class="table-total" role="status">本次查询共 {{ page.total }} 条</span><div class="request-page-tools"><label>每页 <select :value="limit" :disabled="loading" @change="changeSize(Number(($event.target as HTMLSelectElement).value))"><option :value="20">20 条</option><option :value="50">50 条</option><option :value="100">100 条</option></select></label><nav class="page-numbers" aria-label="请求分页"><NButton quaternary circle aria-label="上一页" title="上一页" :disabled="loading || page.page <= 1" @click="changePage(page.page - 1)"><ChevronLeft :size="16" aria-hidden="true" /></NButton><NButton v-for="number in pageButtons" :key="number" quaternary :type="number === page.page ? 'primary' : 'default'" :aria-current="number === page.page ? 'page' : undefined" :aria-label="`第 ${number} 页`" :disabled="loading" @click="changePage(number)">{{ number }}</NButton><NButton quaternary circle aria-label="下一页" title="下一页" :disabled="loading || page.page >= shownPages" @click="changePage(page.page + 1)"><ChevronRight :size="16" aria-hidden="true" /></NButton></nav><form class="page-jump" @submit.prevent="jumpPage"><label>跳至 <input v-model.number="jump" aria-label="跳至页码" type="number" min="1" :max="shownPages" :disabled="loading" /></label><NButton attr-type="submit" quaternary :disabled="loading || !Number.isInteger(jump) || jump < 1 || jump > shownPages">跳转</NButton><span>/ {{ shownPages }} 页</span></form></div></div>
    </div>
    <NModal :show="selected !== null" preset="card" class="request-detail-dialog" title="请求用量明细" role="dialog" aria-label="请求用量明细" @update:show="value => { if (!value) selected = null }">
      <template v-if="selected"><NAlert v-if="copyError" type="warning">{{ copyError }}</NAlert><p>请求 {{ selected.id }} · {{ resultLabel(selected) }}</p><dl class="request-exact-metrics"><dt>统计说明</dt><dd>{{ metricsExplanation(selected) }}</dd><dt>供应商</dt><dd>{{ selected.provider_id ? providerName(selected.provider_id) : '未选择供应商' }}</dd><dt>模型</dt><dd>{{ selected.model || '模型未报告' }}</dd><dt>请求</dt><dd>{{ selected.entry }} · {{ typeLabels[selected.request_type] }}</dd><dt>结果码</dt><dd>{{ selected.result }}</dd><dt v-if="selected.error_stage">阶段</dt><dd v-if="selected.error_stage">{{ stages[selected.error_stage] || selected.error_stage }}</dd><dt>会话 ID</dt><dd>{{ selected.thread_id || '无会话标识' }}</dd><dt>协议终止结果</dt><dd>{{ selected.observation?.terminal_result && selected.observation.terminal_result !== 'unknown' ? resultLabel({ ...selected, result: selected.observation.terminal_result }) : '未保存或未确认' }}</dd><dt>连接结束原因</dt><dd>{{ transportLabel(selected) }}</dd><dt>非缓存输入</dt><dd>{{ selected.request_type === 'auxiliary' ? '不适用' : tokens(nonCachedInput(selected)) }}</dd><template v-for="field in exactFields" :key="field[0]"><dt>{{ field[1] }}</dt><dd>{{ selected.request_type === 'auxiliary' ? '不适用' : tokens(metric(selected, field[0])) }}</dd></template><dt>首字耗时</dt><dd>{{ selected.request_type === 'auxiliary' || selected.request_type === 'sync' ? '不适用' : duration(metric(selected, 'first_token_ms')) }}</dd><dt>输出 TPS</dt><dd>{{ outputRate(selected) }}</dd><dt :title="latencyHint('duration')">总耗时</dt><dd>{{ duration(requestDuration(selected)) }}</dd><dt>连接结束耗时</dt><dd>{{ selected.phase === 'received' ? '—' : duration(selected.elapsed_ms) }}</dd><dt>时间</dt><dd>{{ formatTimestamp(selected.time, true) }}</dd></dl><p class="metric-explanation">非缓存输入扣除已报告的缓存读取和写入；缓存属于输入，推理属于输出，均不重复计入总量。“—”表示未报告。TPS 使用输出除以总耗时；存在明确终止事件时，总耗时排除其后的连接收尾等待。</p><p class="metric-explanation">{{ latencyHint('first') }}；{{ latencyHint('duration') }}。缺失指标为灰色。</p><NButton @click="selected = null">关闭明细</NButton></template>
    </NModal>
  </section>
</template>
