<script setup lang="ts">
import { computed, onMounted, onUnmounted, reactive, ref } from 'vue'
import { NAlert, NButton, NConfigProvider, NInput, NPopconfirm, NSpin, NTag, NModal, zhCN } from 'naive-ui'
import type { GlobalThemeOverrides } from 'naive-ui'
import RelayForm from './components/RelayForm.vue'
import RequestHistory from './components/RequestHistory.vue'
import HistoryManagement from './components/HistoryManagement.vue'
import AppNavigation from './components/AppNavigation.vue'
import SessionTable from './components/SessionTable.vue'
import { navigation, pages } from './navigation'
import type { Panel } from './navigation'
import { formatTimestamp } from './request-format'
import { startSessionRenewal } from './session-renewal'
import type { RelayDraft } from './components/RelayForm.vue'
import { ApiClient, ApiError, assertCompatible, consumeBootstrap } from './api/client'
import type { Capabilities, Config, Diagnostics, Provider, ProviderStatus, Sessions, Status, Client, ClientAction, DailyStatus, DailyPreview, Activity } from './api/types.gen'

const theme: GlobalThemeOverrides = {
  common: { primaryColor: '#245da8', primaryColorHover: '#1b4e91', primaryColorPressed: '#183e73', borderRadius: '6px', fontFamily: '"Segoe UI Variable", "Microsoft YaHei UI", sans-serif', fontSize: '15px' },
  Button: { heightMedium: '42px', heightLarge: '46px' },
  Input: { heightMedium: '44px' },
  Tag: { textColorSuccess: '#146545', colorSuccess: '#e3f3eb' },
}
const isMock = import.meta.env.MODE === 'mock'
const panel = ref<Panel>('overview')
const currentTitle = computed(() => pages[panel.value].label)
const currentGroup = computed(() => navigation.find(item => item.children?.includes(panel.value)))
const isListPage = computed(() => ['requests', 'sessions-pending', 'sessions-bound', 'diagnostics'].includes(panel.value))
const sessionKind = computed(() => panel.value === 'sessions-pending' ? 'pending' : 'bound')
function navigate(value: Panel) { panel.value = value; notice.value = ''; actionError.value = ''; configNotice.value = '' }
async function changeSessionSize(value: number) { sessionLimits[sessionKind.value] = value; await turnSessions(sessionKind.value, 'reset') }
let api = new ApiClient()
let lifetime = new AbortController()
let bootstrapToken = consumeBootstrap(window.location, window.history)
let timer: ReturnType<typeof setTimeout> | undefined
let refreshing = false
let stopRenewal: (() => void) | undefined
const connectionEpoch = ref(0)
const loading = ref(true)
const connection = ref<'connecting' | 'connected' | 'disconnected' | 'unauthorized' | 'incompatible'>('connecting')
const error = ref('')
const actionError = ref('')
const notice = ref('')
const configNotice = ref('')
const busy = ref(false)
const lastUpdated = ref('')
const config = ref<Config>()
const draft = ref<Config>()
const capabilities = ref<Capabilities>()
const status = ref<Status>()
const clientItems = ref<Client[]>([])
const daily = ref<DailyStatus>()
const dailyDialog = ref(false)
const dailyPlan = ref<DailyPreview>()
const dailyError = ref('')
const dailyActionKind = ref<'route' | 'restore'>('route')
const dailyConfirmTitle = computed(() => dailyActionKind.value === 'restore' ? '确认恢复接入前配置' : '确认接入日常 Codex')
const clientView = ref<'daily' | 'test'>('daily')
const activity = ref<Activity>({ received: 0, items: [], degraded: false })
const dailyLabel = computed(() => ({ configured: '配置已接入', direct: '尚未接入', custom: '已有自定义地址', conflict: '配置冲突', port_changed: '地址待更新', blocked: '暂不能接入', unavailable: '未启用' })[daily.value?.state ?? 'unavailable'] ?? '待检查')
async function previewDaily(action: 'route' | 'restore') {
  dailyError.value = ''
  dailyActionKind.value = action
  await act(async () => { dailyPlan.value = await api.dailyPreview({ action }, lifetime.signal); dailyDialog.value = true }, '')
}
async function applyDaily() {
  if (!dailyPlan.value || !canWrite.value) return
  busy.value = true; dailyError.value = ''
  try {
    daily.value = await api.dailyApply({ token: dailyPlan.value.token }, lifetime.signal)
    dailyDialog.value = false; dailyPlan.value = undefined
    notice.value = dailyActionKind.value === 'restore' ? '已恢复接入前的连接配置。请保存工作，手动重开 Codex；之后新启动的客户端使用恢复后的原连接。' : '已写入 CodexRouter 连接地址。请保存工作，手动重开日常 Codex。登录和历史目录未变，实际接入请核对新请求。'
    await refresh()
  } catch (value) {
    dailyError.value = value instanceof Error ? value.message : '应用失败，请关闭后重新预览。'
    dailyPlan.value = undefined
    if (value instanceof ApiError && (value.status === 401 || value.status === 403 || value.code === 'BACKEND_UNREACHABLE')) handleError(value)
  } finally { busy.value = false }
}
const clientLabel = (item: Client) => item.target === 'cli' ? 'Codex CLI' : 'Codex 桌面版'
const clientState = (item: Client) => item.state === 'running' ? '已启动' : item.state === 'not_installed' ? '客户端未安装' : '可以启动'
async function runClient(item: Client, action: ClientAction['action']) {
  await act(async () => { clientItems.value = (await api.clientAction({ target: item.target, action }, lifetime.signal)).items }, action === 'login' ? '请在专用客户端中由本人完成登录。' : '专用客户端操作已完成。日常配置未修改。')
}
const providers = ref<Provider[]>([])
const sessions = ref<Sessions>({ bindings: [], pending: [] })
const sessionTotals = reactive({ bound: 0, pending: 0 })
const sessionCursors = reactive({ bound: [''], pending: [''] })
const sessionNext = reactive({ bound: '', pending: '' })
const sessionLimits = reactive({ bound: 20, pending: 20 })
const pageLoading = ref(false)
const sessionError = ref('')
async function readSessionPage(kind: 'bound' | 'pending') {
  try { return await api.sessionPage(kind, sessionLimits[kind], sessionCursors[kind].at(-1), lifetime.signal) }
  catch (value) { if (value instanceof ApiError && value.code === 'HISTORY_UNAVAILABLE') { sessionError.value = value.message; return null }; throw value }
}
const providerDialog = ref(false)
const editingProvider = ref('')
const editedProvider = computed(() => providers.value.find(p => p.id === editingProvider.value))
function openProvider(id = '') { editingProvider.value = id; providerDialog.value = true }
async function turnSessions(kind: 'bound' | 'pending', direction: 'next' | 'previous' | 'reset') {
  if (refreshing || busy.value) return
  if (direction === 'reset') sessionCursors[kind] = ['']
  else if (direction === 'next' && sessionNext[kind]) sessionCursors[kind].push(sessionNext[kind])
  else if (direction === 'previous' && sessionCursors[kind].length > 1) sessionCursors[kind].pop()
  await refresh()
}
async function copySession(id: string) {
  try { await navigator.clipboard.writeText(id); notice.value = '会话 ID 已复制。'; return true }
  catch { actionError.value = '无法访问剪贴板，请选中完整会话 ID 后复制。'; return false }
}
const diagnostics = ref<Diagnostics>({ items: [] })
const providerDraft = reactive<Record<string, { name: string; base_url: string; model: string }>>({})
const providerKeys = reactive<Record<string, string>>({})
const pendingProviders = reactive<Record<string, string>>({})
const newProvider = reactive<RelayDraft & { key: string }>({ name: '', base_url: '', model: '', key: '' })
const selectedProvider = ref('')
const selectedMode = ref<'official' | 'relay'>('official')
const relayProviders = computed(() => providers.value.filter(p => p.kind === 'api_key'))
const desiredProvider = computed(() => selectedMode.value === 'official' ? 'official' : selectedProvider.value)
const selectedRelay = computed(() => relayProviders.value.find(p => p.id === selectedProvider.value))
const readyToApply = computed(() => selectedMode.value === 'official' || !!(selectedRelay.value?.base_url && selectedRelay.value.key_configured))
const modelOpen = ref(false)
const modelLoading = ref(false)
const modelError = ref('')
const modelItems = ref<string[]>([])
const modelSearch = ref('')
const modelTarget = ref('')
const modelPage = ref(1)
const filteredModels = computed(() => modelItems.value.filter(id => id.toLowerCase().includes(modelSearch.value.toLowerCase())))
const visibleModels = computed(() => filteredModels.value.slice(0, modelPage.value * 40))
const canWrite = computed(() => connection.value === 'connected' && !busy.value && !modelLoading.value)
function chooseMode(mode: 'official' | 'relay') {
  selectedMode.value = mode
  if (mode === 'relay' && !selectedRelay.value) selectedProvider.value = relayProviders.value.find(p => p.base_url && p.key_configured)?.id ?? ''
}
async function queryModels(id: string) {
  if (!canWrite.value) return
  modelTarget.value = id
  modelSearch.value = ''; modelPage.value = 1; modelItems.value = []; modelError.value = ''; modelOpen.value = true; modelLoading.value = true
  const target = id ? providerDraft[id] : newProvider
  try {
    const result = await api.queryModels({ provider_id: id, base_url: target.base_url, key: id ? (providerKeys[id] ?? '') : newProvider.key }, lifetime.signal)
    modelItems.value = result.items.map(item => item.id)
  } catch (value) {
    if (lifetime.signal.aborted) return
    modelError.value = value instanceof Error ? value.message : '拉取失败，请手动填写模型。'
    if (value instanceof ApiError && (value.status === 401 || value.status === 403 || value.code === 'BACKEND_UNREACHABLE')) handleError(value)
  } finally { modelLoading.value = false }
}
function chooseModel(id: string) {
  const target = modelTarget.value ? providerDraft[modelTarget.value] : newProvider
  if (target) target.model = id
  modelOpen.value = false
}
const usable = (p: Provider) => p.base_url !== ''
const providerName = (id: string) => id === 'local' ? '本机服务' : providers.value.find(x => x.id === id)?.name ?? id
const providerStatus = (id: string) => status.value?.upstreams.find(x => x.provider_id === id)
const formatTime = (time: string) => time ? formatTimestamp(time) : '尚无记录'
const stateLabel = (value?: ProviderStatus) => !value || value.result === 'unverified' ? '未验证' : value.result === 'success' ? '最近成功' : '最近失败'
const stateType = (value?: ProviderStatus) => value?.result === 'success' ? 'success' : value?.result === 'failure' ? 'error' : 'default'
const connectionLabel = computed(() => ({ connecting: '正在连接', connected: '本地服务运行中', disconnected: '本地服务不可达', unauthorized: '管理认证已失效', incompatible: '接口版本不兼容' })[connection.value])

function handleError(value: unknown) {
  const failure = value instanceof ApiError ? value : new ApiError('UNKNOWN', '读取失败，请刷新重试。')
  connection.value = failure.code === 'API_VERSION_INCOMPATIBLE' ? 'incompatible'
    : failure.status === 401 || failure.status === 403 ? 'unauthorized' : 'disconnected'
  error.value = connection.value === 'unauthorized' ? '管理认证已失效。请从托盘重新打开管理页面，或双击现用 CodexRouter 程序。' : failure.message
  if (connection.value === 'unauthorized' || connection.value === 'incompatible') { stopRenewal?.(); stopRenewal = undefined }
}

function applyConfig(next: Config) {
  const previous = config.value
  if (!previous || desiredProvider.value === previous.default_provider_id) {
    selectedMode.value = next.default_provider_id === 'official' ? 'official' : 'relay'
    selectedProvider.value = next.default_provider_id
  }
  if (!previous || !draft.value) {
    draft.value = { ...next }
  } else {
    for (const field of ['connect_timeout_ms', 'header_timeout_ms', 'idle_timeout_ms', 'write_timeout_ms', 'max_concurrent'] as const) {
      if (draft.value[field] === previous[field]) draft.value[field] = next[field]
    }
    draft.value.default_provider_id = next.default_provider_id
  }
  config.value = next
}

function applyProviders(next: Provider[]) {
  const previous = new Map(providers.value.map(item => [item.id, item]))
  let lockedReset = false
  for (const item of next) {
    const pending = providerDraft[item.id]
    const prev = previous.get(item.id)
    if (!pending || !prev) {
      providerDraft[item.id] = { name: item.name, base_url: item.base_url, model: item.model }
      continue
    }
    if (item.upstream_locked || pending.base_url === prev.base_url) {
      if (item.upstream_locked && pending.base_url !== item.base_url) lockedReset = true
      pending.base_url = item.base_url
    }
    if (pending.name === prev.name) pending.name = item.name
    if (pending.model === prev.model) pending.model = item.model
  }
  for (const id of Object.keys(providerDraft)) {
    if (!next.some(item => item.id === id)) {
      delete providerDraft[id]
      delete providerKeys[id]
    }
  }
  providers.value = next
  if (!next.some(item => item.id === selectedProvider.value)) selectedProvider.value = config.value?.default_provider_id ?? next[0]?.id ?? ''
  if (lockedReset) configNotice.value = '该供应商已有固定会话，上游基址已锁定。未保存的地址已恢复为实际配置，其他未保存设置继续保留。'
}

async function refresh() {
  if (refreshing || lifetime.signal.aborted) return
  const scope = lifetime
  refreshing = true
  pageLoading.value = true
  try {
    const [nextStatus, nextBound, nextPending, nextDiagnostics, nextCapabilities, nextConfig, nextProviders, nextClients, nextDaily, nextActivity] = await Promise.all([
      api.status(lifetime.signal), readSessionPage('bound'), readSessionPage('pending'), api.diagnostics(lifetime.signal), api.capabilities(lifetime.signal), api.config(lifetime.signal), api.providers(lifetime.signal), api.clients(lifetime.signal), api.daily(lifetime.signal), api.activity(lifetime.signal),
    ])
    if (scope !== lifetime || scope.signal.aborted) return
    assertCompatible(nextCapabilities)
    applyConfig(nextConfig)
    applyProviders(nextProviders.items)
    clientItems.value = nextClients.items
    daily.value = nextDaily
    activity.value = nextActivity
    capabilities.value = nextCapabilities
    status.value = nextStatus
    if (nextBound && nextPending) {
      sessions.value = { bindings: nextBound.bindings, pending: nextPending.pending }
      sessionTotals.bound = nextBound.total; sessionTotals.pending = nextPending.total
      sessionNext.bound = nextBound.next_cursor; sessionNext.pending = nextPending.next_cursor
      sessionError.value = ''
    }
    diagnostics.value = nextDiagnostics
    lastUpdated.value = new Date().toISOString()
    connection.value = 'connected'
    error.value = ''
  } catch (value) {
    if (scope === lifetime && !scope.signal.aborted) handleError(value)
  } finally { if (scope === lifetime) { refreshing = false; pageLoading.value = false } }
}

function schedule() {
  clearTimeout(timer)
  if (lifetime.signal.aborted || ['unauthorized', 'incompatible'].includes(connection.value)) return
  const scope = lifetime
  timer = setTimeout(async () => {
    if (scope !== lifetime || scope.signal.aborted) return
    if (document.visibilityState !== 'hidden') await refresh()
    if (scope === lifetime) schedule()
  }, 5000)
}

async function connect() {
  lifetime.abort()
  lifetime = new AbortController()
  api = new ApiClient()
  connectionEpoch.value++
  const scope = lifetime
  const currentApi = api
  refreshing = false
  busy.value = false
  stopRenewal?.(); stopRenewal = undefined
  loading.value = true
  error.value = ''
  connection.value = 'connecting'
  clearTimeout(timer)
  try {
    const token = bootstrapToken
    bootstrapToken = null
    await currentApi.authenticate(token, scope.signal)
    if (scope !== lifetime || scope.signal.aborted) return
    const nextCapabilities = await currentApi.capabilities(scope.signal)
    const nextConfig = await currentApi.config(scope.signal)
    const nextProviders = await currentApi.providers(scope.signal)
    if (scope !== lifetime || scope.signal.aborted) return
    assertCompatible(nextCapabilities)
    capabilities.value = nextCapabilities
    applyConfig(nextConfig)
    applyProviders(nextProviders.items)
    await refresh()
    if (scope === lifetime && !scope.signal.aborted && !error.value) {
      stopRenewal = startSessionRenewal(() => currentApi.renew(scope.signal), value => { if (scope === lifetime && !scope.signal.aborted) handleError(value) })
    }
  } catch (value) {
    if (scope === lifetime && !scope.signal.aborted) handleError(value)
  } finally {
    if (scope === lifetime) { loading.value = false; schedule() }
  }
}

async function act(work: () => Promise<void>, success: string) {
  if (!canWrite.value) return
  const scope = lifetime
  busy.value = true
  actionError.value = ''
  notice.value = ''
  try {
    await work()
    if (scope !== lifetime || scope.signal.aborted) return
    notice.value = success
    await refresh()
  } catch (value) {
    if (scope !== lifetime || scope.signal.aborted) return
    if (value instanceof ApiError && (value.status === 401 || value.status === 403 || value.code === 'BACKEND_UNREACHABLE')) handleError(value)
    actionError.value = value instanceof Error ? value.message : '保存失败，请重试。'
  } finally { if (scope === lifetime) busy.value = false }
}

const configInput = (defaultProvider: string, timeouts: Config) => ({
  default_provider_id: defaultProvider, connect_timeout_ms: timeouts.connect_timeout_ms, header_timeout_ms: timeouts.header_timeout_ms,
  idle_timeout_ms: timeouts.idle_timeout_ms, write_timeout_ms: timeouts.write_timeout_ms, max_concurrent: timeouts.max_concurrent,
})

async function saveDefault() {
  const desired = desiredProvider.value
  await act(async () => {
    const latest = await api.config(lifetime.signal)
    applyConfig(latest)
    const saved = await api.saveConfig(configInput(desired, latest), lifetime.signal)
    applyConfig(saved)
    selectedProvider.value = saved.default_provider_id
    selectedMode.value = saved.default_provider_id === 'official' ? 'official' : 'relay'
  }, '已保存新会话默认供应商。已有会话保持原绑定。')
}

async function saveTimeouts() {
  await act(async () => {
    const latest = await api.config(lifetime.signal)
    applyConfig(latest)
    const saved = await api.saveConfig(configInput(latest.default_provider_id, draft.value!), lifetime.signal)
    applyConfig(saved)
    draft.value = { ...saved }
  }, '超时与并发设置已保存。')
}

async function saveProvider(id: string) {
  const pending = providerDraft[id]
  if (!pending) return
  await act(async () => {
    const saved = await api.saveRelay({ provider_id: id, ...pending, key: providerKeys[id] ?? '' }, lifetime.signal)
    providerDraft[id] = { name: saved.name, base_url: saved.base_url, model: saved.model }
    providerKeys[id] = ''
    providerDialog.value = false
  }, '供应商已保存。默认模型对下一条请求生效。')
}

async function addProvider() {
  await act(async () => {
    await api.saveRelay({ provider_id: '', name: newProvider.name, base_url: newProvider.base_url, model: newProvider.model, key: newProvider.key }, lifetime.signal)
    newProvider.name = ''; newProvider.base_url = ''; newProvider.model = ''; newProvider.key = ''
    providerDialog.value = false
  }, '供应商已添加。可在线路总览选择中转模式并应用。')
}

async function removeProvider(id: string) {
  await act(async () => { await api.deleteProvider(id, lifetime.signal) }, '供应商已删除，其系统凭据同时清除。')
}


async function deleteKey(id: string) {
  await act(async () => { await api.deleteKey(id, lifetime.signal) }, '密钥已删除。该供应商请求将暂停，直到重新配置密钥。')
}

async function registerSession(id: string) {
  const providerId = pendingProviders[id]
  if (!providerId) { actionError.value = '请先为该会话选择供应商。'; return }
  await act(async () => { await api.register({ id, provider_id: providerId }, lifetime.signal); sessionCursors.pending = ['']; sessionCursors.bound = [''] }, '会话供应商已确认。请在客户端重试原请求，路由器不会自动补发。')
}

function visibilityChanged() {
  if (document.visibilityState === 'visible' && connection.value === 'connected') void refresh()
}
function bootstrapChanged() {
  const token = consumeBootstrap(window.location, window.history)
  if (token === null) return
  bootstrapToken = token
  void connect()
}
onMounted(() => { void connect(); document.addEventListener('visibilitychange', visibilityChanged); window.addEventListener('hashchange', bootstrapChanged) })
onUnmounted(() => {
  stopRenewal?.()
  lifetime.abort()
  clearTimeout(timer)
  for (const id of Object.keys(providerKeys)) delete providerKeys[id]
  newProvider.key = ''
  document.removeEventListener('visibilitychange', visibilityChanged)
  window.removeEventListener('hashchange', bootstrapChanged)
})
</script>

<template>
  <NConfigProvider :locale="zhCN" :theme-overrides="theme">
    <a class="skip-link" href="#main-content">跳到主要内容</a>
    <div class="shell">
      <aside class="sidebar">
        <div class="brand"><img src="/favicon.svg" width="34" height="34" alt="" /><div>CodexRouter<span>本地线路管理</span></div></div>
        <AppNavigation :active="panel" :pending="sessionTotals.pending" @navigate="navigate" />
        <div class="sidebar-note"><span class="local-symbol" aria-hidden="true">●</span> 仅本机访问<p>会话一经绑定，始终使用原供应商。</p></div>
      </aside>

      <div class="workspace">
        <header class="topbar">
          <span class="connection" :class="connection"><span aria-hidden="true"></span>{{ connectionLabel }}</span>
          <span class="refresh-time">{{ lastUpdated ? `页面更新于 ${formatTime(lastUpdated)}` : '等待读取状态' }}</span>
          <NButton secondary :disabled="loading || busy" @click="connect">刷新</NButton>
        </header>
        <main class="page-main" id="main-content" tabindex="-1" :aria-busy="loading">
          <div class="page-heading"><div><h1>{{ currentTitle }}</h1><p>{{ pages[panel].description }}</p></div><NTag v-if="isMock" type="warning" :bordered="false">合成演示数据</NTag><NTag v-else :bordered="false">测试构建</NTag></div>

          <NAlert v-if="error" type="error" class="feedback" title="当前数据不能代表实时状态" role="alert">{{ error }}<p v-if="lastUpdated">下方保留的是 {{ formatTime(lastUpdated) }} 的旧数据。</p></NAlert>
          <nav v-if="currentGroup" class="page-subnav" aria-label="二级导航"><button v-for="child in currentGroup.children" :key="child" type="button" :class="{ active: panel === child }" :aria-current="panel === child ? 'page' : undefined" @click="navigate(child)">{{ pages[child].label }}</button></nav>
          <NAlert v-if="sessionError" type="warning" class="feedback" role="alert">{{ sessionError }} 会话列表可能为旧数据，其他管理功能仍可使用。</NAlert>
          <NAlert v-if="actionError && !providerDialog" type="error" class="feedback" role="alert">{{ actionError }}</NAlert>
          <NAlert v-if="configNotice" type="info" class="feedback" role="status">{{ configNotice }}</NAlert>
          <NAlert v-if="notice" type="success" class="feedback" role="status">{{ notice }}</NAlert>
          <div v-if="loading && !config" class="loading-state"><NSpin size="large" /><p>正在读取本地配置…</p></div>
          <div v-else-if="!config" class="empty-state"><h2>{{ connection === 'unauthorized' ? '需要重新打开管理页面' : '管理页面尚未连接' }}</h2><p>{{ connection === 'unauthorized' ? '请双击托盘图标或现用 CodexRouter 程序，使用新打开的页面。' : '请确认服务正在运行。' }}</p><NButton v-if="connection !== 'unauthorized'" @click="connect">重新连接</NButton></div>

          <div v-else class="page-content" :class="{ 'list-page': isListPage }" :key="isListPage ? 'list' : panel">
            <section v-if="panel === 'overview'" aria-label="线路总览">
              <div class="activity-summary">
                <div><h2>{{ activity.received ? '已收到客户端请求' : '尚未收到客户端请求' }}</h2><p>本次运行累计 {{ activity.received }} 条入口请求，活动生成 {{ status?.active_requests ?? 0 }} 条。</p><p>日常接入：{{ dailyLabel }}。请到请求明细核对实际时间和供应商。</p></div>
                <div class="table-actions"><NButton secondary :disabled="loading || busy" @click="connect">刷新请求</NButton><NButton type="primary" secondary @click="navigate('requests')">查看请求明细</NButton></div>
              </div>
              <NAlert v-if="activity.degraded" type="warning" class="feedback">本轮历史记录或会话索引出现过存储故障，记录可能不完整；原路由绑定保留。</NAlert>
              <div class="route-section">
                <div class="section-heading"><div><h2>新会话使用哪种模式</h2><p>选择模式后保存并应用。已有会话保持原供应商。</p></div><span class="saved-route">当前：{{ config.default_provider_id === 'official' ? '官方模式' : `中转 · ${providerName(config.default_provider_id)}` }}</span></div>
                <div class="route-options" role="group" aria-label="新会话模式">
                  <button class="route-option" :class="{ selected: selectedMode === 'official' }" :aria-pressed="selectedMode === 'official'" :disabled="!canWrite" @click="chooseMode('official')"><span class="route-top"><svg class="mode-icon" viewBox="0 0 24 24" aria-hidden="true"><path d="m12 3 8 4v5c0 5-8 9-8 9s-8-4-8-9V7zM8 12l3 3 5-6" /></svg><span class="radio-dot" aria-hidden="true"></span></span><strong>官方模式</strong><span>使用专用客户端的官方账号登录</span><span class="route-destination">模型跟随客户端设置</span></button>
                  <button class="route-option" :class="{ selected: selectedMode === 'relay' }" :aria-pressed="selectedMode === 'relay'" :disabled="!canWrite" @click="chooseMode('relay')"><span class="route-top"><svg class="mode-icon" viewBox="0 0 24 24" aria-hidden="true"><path d="M4 4v16M4 12h8m0 0 8-8m-8 8 8 8" /><circle cx="20" cy="4" r="2" /><circle cx="20" cy="20" r="2" /></svg><span class="radio-dot" aria-hidden="true"></span></span><strong>中转模式</strong><span>选择自己的中转供应商和 API Key</span><span class="route-destination">公司和其他中转都在这里管理</span></button>
                </div>
                <div v-if="selectedMode === 'relay'" class="relay-selection">
                  <div class="field"><label for="relay-provider">中转供应商</label><select id="relay-provider" v-model="selectedProvider" :disabled="!canWrite"><option value="" disabled>请选择已添加的供应商</option><option v-for="item in relayProviders" :key="item.id" :value="item.id">{{ item.name }}{{ !item.base_url || !item.key_configured ? '（待完善配置）' : '' }}</option></select><p v-if="selectedRelay" class="endpoint">{{ selectedRelay.base_url || '尚未填写地址' }}<br>{{ selectedRelay.model ? `默认模型：${selectedRelay.model}` : '模型跟随客户端' }}</p></div>
                  <NAlert v-if="!readyToApply" type="info">请先添加供应商，或补全所选供应商的地址和密钥。</NAlert><NButton text type="primary" :disabled="!canWrite" @click="panel = 'providers'; if (!relayProviders.length) openProvider()">{{ relayProviders.length ? '管理中转供应商' : '添加供应商' }}</NButton>
                </div>
                <div class="section-footer"><p>保存后请在客户端新建对话，历史会话不会迁移。</p><NButton type="primary" :loading="busy" :disabled="!canWrite || !readyToApply || !desiredProvider || desiredProvider === config.default_provider_id" @click="saveDefault">保存并应用</NButton></div>
              </div>

              <button v-if="sessionTotals.pending" class="pending-banner" @click="navigate('sessions-pending')"><span class="pending-mark" aria-hidden="true">!</span><span><strong>{{ sessionTotals.pending }} 个会话等待首次登记</strong><span>确认来源后选择供应商，再由客户端重试请求。</span></span><span class="text-link">前往登记</span></button>

              <div class="section-heading upstream-heading"><div><h2>供应商最近结果</h2><p>只反映最近一次请求。没有发起请求时，保持“未验证”。</p></div><span class="request-count">活动请求 {{ status?.active_requests ?? '—' }}</span></div>
              <div class="upstream-list">
                <article v-for="item in providers" :key="item.id" class="upstream-row">
                  <div><h3>{{ item.name }}</h3><p class="endpoint">{{ item.base_url || '尚未设置上游地址' }}</p></div>
                  <div class="upstream-result"><NTag :type="connection === 'connected' ? stateType(providerStatus(item.id)) : 'warning'" :bordered="false">{{ connection === 'connected' ? stateLabel(providerStatus(item.id)) : '数据已过期' }}</NTag><span>{{ formatTime(providerStatus(item.id)?.checked_at ?? '') }}</span></div>
                </article>
              </div>
              <p class="protocol-note">兼容性验收尚未完成。当前支持 {{ capabilities?.transports.join('、') || '未读取' }}；CLI、桌面版与各供应商实机结果以交付验证记录为准。</p>
            </section>

            <RequestHistory :key="connectionEpoch" v-if="panel === 'requests'" :api="api" :writable="canWrite" :provider-name="providerName" @failure="handleError" />
            <HistoryManagement :key="connectionEpoch" v-if="panel === 'history'" :api="api" :writable="canWrite" @failure="handleError" />

            <section v-if="panel === 'clients'" aria-label="客户端与接入">
              <div class="client-tabs" role="group" aria-label="使用环境"><button :aria-pressed="clientView === 'daily'" :class="{active: clientView === 'daily'}" @click="clientView = 'daily'">日常使用</button><button :aria-pressed="clientView === 'test'" :class="{active: clientView === 'test'}" @click="clientView = 'test'">独立测试</button></div>
              <div v-if="clientView === 'daily'" class="daily-section">
                <div class="section-heading"><div><h2>接入原来的 Codex</h2><p>保留原登录、历史和项目，不创建另一套工作环境。</p></div><NTag :type="daily?.state === 'configured' ? 'info' : 'default'" :bordered="false">{{ dailyLabel }}</NTag></div>
                <p>{{ daily?.message || '正在检查日常配置' }}</p>
                <dl class="daily-details"><dt>配置文件</dt><dd>{{ daily?.config_path || '未确定' }}</dd><dt>本机接入地址</dt><dd>{{ daily?.base_url || '未确定' }}</dd><dt>修改范围</dt><dd>仅 openai_base_url；不改登录认证、模型设置和历史目录。</dd></dl>
                <div class="client-actions"><NButton type="primary" :disabled="!canWrite || !daily || ['blocked','conflict','unavailable'].includes(daily.state)" @click="previewDaily('route')">{{ daily?.state === 'configured' ? '查看接入预览' : '接入 CodexRouter' }}</NButton><NButton secondary :disabled="!canWrite || !daily?.has_backup || ['blocked','conflict'].includes(daily?.state ?? '')" @click="previewDaily('restore')">恢复接入前配置</NButton></div>
                <NAlert type="warning" class="daily-warning">首次接入、地址更新或恢复后，请保存工作并手动重开 Codex。桌面版与 CLI 可能共用配置；已有 CLI 共享后台可能继续沿用旧连接，本程序不会停止它。路由器需要保持运行，网页可以关闭。</NAlert>
                <p class="protocol-note">配置已接入不等于实际收到请求。新建对话后，到请求明细核对请求时间和供应商。未知历史须先确认原供应商，不能自动迁移。</p>
              </div>
              <template v-else>
              <NAlert type="info" class="feedback">仅用于排查和验收。此环境不共享日常登录、历史和项目；同一测试目录登录成功后应保留登录。退出路由器会停止它拥有的测试客户端，不关闭日常 Codex。</NAlert>
              <div class="quick-start">
                <div><span class="step-number">1</span><strong>配置供应商</strong><p>填写地址和密钥，选择新会话默认供应商。</p><NButton secondary @click="panel = 'providers'">前往供应商设置</NButton></div>
                <div><span class="step-number">2</span><strong>启动专用客户端</strong><p>官方首次使用需在专用窗口登录。日常客户端保持原样。</p></div>
                <div><span class="step-number">3</span><strong>开始对话</strong><p>成功生成后到线路总览和已绑定会话查看绑定。</p></div>
              </div>
              <NAlert type="info" class="feedback">这里管理独立测试环境。启动成功不等于真实上游已验证；日常 Codex 配置不会被接管。退出路由器会停止由它启动的测试客户端。</NAlert>
              <div v-if="!clientItems.length" class="empty-state"><h2>当前实例未启用客户端管理</h2><p>请双击配套主程序启动，开发测试实例可能不提供此功能。</p></div>
              <article v-for="item in clientItems" :key="item.target" class="client-card">
                <div class="section-heading"><div><h2>{{ clientLabel(item) }}</h2><p>{{ item.version || '未检测到版本' }}</p></div><NTag :type="item.state === 'running' ? 'success' : 'default'" :bordered="false">{{ clientState(item) }}</NTag></div>
                <p>{{ item.message }}</p>
                <p v-if="item.target === 'desktop'" class="protocol-note">使用独立桌面数据和登录。桌面兼容性尚需真实验收；如果窗口没有打开，请查看近期诊断与版本说明。</p>
                <p class="saved-route">下次启动：{{ item.mode === 'direct' ? '直连官方（不经过路由器）' : '通过本机路由器' }}</p>
                <div class="client-actions">
                  <NButton type="primary" :disabled="!canWrite || !item.installed || item.state === 'running'" :loading="busy" @click="runClient(item, 'start')">{{ item.target === 'cli' ? '启动测试 CLI' : '启动测试桌面版' }}</NButton>
                  <NButton v-if="item.target === 'cli'" secondary :disabled="!canWrite || !item.installed || item.state === 'running'" @click="runClient(item, 'login')">登录官方账号</NButton>
                  <NButton v-if="item.target === 'cli'" secondary :disabled="!canWrite || !item.installed || item.state === 'running'" @click="runClient(item, 'check')">检查官方登录</NButton>
                  <NPopconfirm @positive-click="runClient(item, 'stop')"><template #trigger><NButton secondary type="error" :disabled="!canWrite || item.state !== 'running'">停止专用客户端</NButton></template>只停止本程序启动的专用客户端。正在生成的请求会中断。</NPopconfirm>
                </div>
                <div class="client-actions"><NButton text :disabled="!canWrite || item.state === 'running' || item.mode === 'direct'" @click="runClient(item, 'direct')">恢复测试环境直连</NButton><NButton text :disabled="!canWrite || item.state === 'running' || item.mode === 'routed'" @click="runClient(item, 'routed')">接入本机路由器</NButton></div>
                <details class="profile-details"><summary>专用数据位置</summary><p class="endpoint">{{ item.profile }}</p><p>配置由程序管理，外部修改时会拒绝覆盖。</p></details>
              </article>
              </template>
            </section>

            <section v-if="panel === 'providers' && draft" aria-label="供应商设置">
                <div class="section-heading"><div><h2>中转供应商</h2><p>官方模式在线路总览选择。已有会话的上游地址保持锁定。</p></div><NButton type="primary" :disabled="!canWrite || relayProviders.length >= 7" @click="openProvider()">添加供应商</NButton></div>
              <div class="table-scroll" tabindex="0" role="region" aria-label="中转供应商表格，可横向滚动">
                <table class="data-table provider-table compact-table"><colgroup><col style="width:10%" /><col style="width:24%" /><col style="width:13%" /><col style="width:12%" /><col style="width:13%" /><col style="width:28%" /></colgroup><thead><tr><th scope="col">名称</th><th scope="col">BaseUrl</th><th scope="col">默认模型</th><th scope="col">凭据</th><th scope="col">状态</th><th scope="col">操作</th></tr></thead><tbody>
                  <tr v-for="provider in relayProviders" :key="provider.id"><td><strong class="truncate" :title="provider.name">{{ provider.name }}</strong></td><td><span class="truncate" :title="provider.base_url">{{ provider.base_url || '尚未设置' }}</span></td><td><span class="truncate" :title="provider.model">{{ provider.model || '跟随客户端' }}</span></td><td><NTag :type="provider.key_configured ? 'success' : 'warning'" :bordered="false">{{ provider.key_configured ? '密钥已配置' : '密钥未配置' }}</NTag></td><td><span v-if="provider.id === config.default_provider_id">新会话默认 </span>{{ provider.upstream_locked ? '地址已锁定' : '可编辑地址' }}</td><td><div class="table-actions">
                    <NButton size="small" secondary :disabled="!canWrite" :aria-label="`编辑 ${provider.name}`" @click="openProvider(provider.id)">编辑</NButton>
                    <NPopconfirm @positive-click="removeProvider(provider.id)"><template #trigger><NButton size="small" type="error" quaternary :disabled="!canWrite || provider.upstream_locked || provider.id === config.default_provider_id" :aria-label="`删除供应商 ${provider.name}`">删除供应商</NButton></template>仅未绑定且非默认的供应商可删除，其系统凭据一并清除。确认删除？</NPopconfirm>
                    <NPopconfirm @positive-click="deleteKey(provider.id)"><template #trigger><NButton size="small" type="error" quaternary :disabled="!canWrite || !provider.key_configured" :aria-label="`删除密钥 ${provider.name}`">删除密钥</NButton></template>删除后该供应商无法发起请求，需要重新录入。确认删除？</NPopconfirm>
                  </div></td></tr>
                  <tr v-if="!relayProviders.length"><td colspan="6" class="table-empty">尚无中转供应商，请添加后返回总览应用。</td></tr>
                </tbody></table>
              </div>

              <form class="settings-form timeout-section" @submit.prevent="saveTimeouts">
                <div class="section-heading"><div><h2>超时与并发</h2><p>对所有供应商生效，分别限制等待阶段。</p></div></div>
                <div class="number-grid">
                  <div class="field"><label for="connect-timeout">连接超时（毫秒）</label><input id="connect-timeout" v-model.number="draft.connect_timeout_ms" type="number" min="100" max="300000" required :disabled="!canWrite" /></div>
                  <div class="field"><label for="header-timeout">响应头超时（毫秒）</label><input id="header-timeout" v-model.number="draft.header_timeout_ms" type="number" min="100" max="300000" required :disabled="!canWrite" /></div>
                  <div class="field"><label for="idle-timeout">流空闲超时（毫秒）</label><input id="idle-timeout" v-model.number="draft.idle_timeout_ms" type="number" min="100" max="3600000" required :disabled="!canWrite" /></div>
                  <div class="field"><label for="write-timeout">客户端写停滞（毫秒）</label><input id="write-timeout" v-model.number="draft.write_timeout_ms" type="number" min="100" max="300000" required :disabled="!canWrite" /></div>
                  <div class="field"><label for="max-concurrent">最大并发请求</label><input id="max-concurrent" v-model.number="draft.max_concurrent" type="number" min="1" max="128" required :disabled="!canWrite" /></div>
                </div>
                <div class="section-footer"><p>不使用短统一总超时。</p><NButton attr-type="submit" type="primary" :disabled="!canWrite" :loading="busy">保存超时与并发</NButton></div>
              </form>
            </section>

            <SessionTable v-if="panel === 'sessions-pending' || panel === 'sessions-bound'" :kind="sessionKind" :bindings="sessions.bindings" :pending="sessions.pending" :providers="providers" :choices="pendingProviders" :total="sessionTotals[sessionKind]" :size="sessionLimits[sessionKind]" :page="sessionCursors[sessionKind].length" :has-next="!!sessionNext[sessionKind]" :loading="pageLoading" :busy="busy" :writable="canWrite" :provider-name="providerName" :copy-session="copySession" @turn="direction => turnSessions(sessionKind, direction)" @size="changeSessionSize" @choose="(id, provider) => { pendingProviders[id] = provider }" @register="registerSession" @copy="copySession" />

            <section v-if="panel === 'diagnostics'" class="diagnostics-view list-view" aria-label="近期诊断">
              <div class="list-toolbar"><p>仅显示最近的脱敏记录。</p><NButton secondary size="small" :disabled="loading || busy" @click="connect">刷新诊断</NButton></div>
              <div class="table-scroll data-viewport" tabindex="0" role="region" aria-label="诊断表格，可横向滚动"><table class="data-table compact-table diagnostic-table"><colgroup><col style="width:28%" /><col style="width:44%" /><col style="width:28%" /></colgroup><thead><tr><th scope="col">时间</th><th scope="col">结果码</th><th scope="col">供应商</th></tr></thead><tbody><tr v-for="(item, index) in diagnostics.items" :key="index"><td><time :datetime="item.time">{{ formatTime(item.time) }}</time></td><td><code class="truncate" :title="item.code">{{ item.code }}</code></td><td><span class="truncate" :title="providerName(item.provider_id)">{{ providerName(item.provider_id) }}</span></td></tr><tr v-if="!diagnostics.items.length"><td colspan="3" class="table-empty">暂无诊断记录</td></tr></tbody></table></div>
              <div class="list-pagination"><span class="table-total" role="status">本次显示 {{ diagnostics.items.length }} 条</span></div>
            </section>
          </div>
        </main>
      </div>
    </div>
    <NModal v-model:show="providerDialog" preset="card" :title="editingProvider ? '编辑供应商' : '添加供应商'" class="model-dialog provider-dialog" :mask-closable="!busy && !modelLoading" :close-on-esc="!busy && !modelLoading" :closable="!busy && !modelLoading" role="dialog" :aria-label="editingProvider ? '编辑供应商' : '添加供应商'">
      <NAlert v-if="actionError" type="error" class="feedback" role="alert">{{ actionError }}</NAlert>
      <RelayForm v-if="editingProvider && editedProvider" :draft="providerDraft[editingProvider]!" :field-id="`provider-${editingProvider}`" :key-value="providerKeys[editingProvider] ?? ''" @update:key-value="providerKeys[editingProvider] = $event" :key-configured="editedProvider.key_configured" :locked="editedProvider.upstream_locked" :disabled="!canWrite" :busy="busy" @save="saveProvider(editingProvider)" @query="queryModels(editingProvider)" />
      <RelayForm v-else-if="!editingProvider" :draft="newProvider" field-id="new-provider" v-model:key-value="newProvider.key" :disabled="!canWrite" :busy="busy" @save="addProvider" @query="queryModels('')" />
      <NAlert v-else type="warning">该供应商已在其他操作中被删除，请关闭后刷新列表。</NAlert>
      <template #footer><div class="model-footer"><NButton :disabled="busy || modelLoading" @click="providerDialog = false">关闭编辑</NButton></div></template>
    </NModal>
    <NModal v-model:show="dailyDialog" preset="card" :title="dailyConfirmTitle" class="model-dialog" :mask-closable="!busy" :close-on-esc="!busy" :closable="!busy" role="dialog" :aria-label="dailyConfirmTitle">
      <NAlert v-if="dailyError" type="error" role="alert">{{ dailyError }} 请关闭后重新预览。</NAlert>
      <template v-if="dailyPlan"><p>{{ dailyPlan.warning }}</p><dl class="daily-details"><dt>配置文件</dt><dd>{{ dailyPlan.config_path }}</dd><dt>字段</dt><dd>openai_base_url</dd><dt>修改前</dt><dd>{{ dailyPlan.before }}</dd><dt>修改后</dt><dd>{{ dailyPlan.after }}</dd></dl><p class="protocol-note">仅备份并修改此字段，其他配置原样保留。确认 5 分钟内有效，配置变化会拒绝覆盖。</p></template>
      <template #footer><div class="model-footer"><NButton :disabled="busy" @click="dailyDialog = false; dailyPlan = undefined">取消</NButton><NButton type="primary" :disabled="!dailyPlan || !canWrite" :loading="busy" @click="applyDaily">{{ dailyActionKind === 'restore' ? '确认恢复' : '确认接入' }}</NButton></div></template>
    </NModal>
    <NModal v-model:show="modelOpen" preset="card" title="选择默认模型" class="model-dialog" :mask-closable="!modelLoading" :close-on-esc="!modelLoading" :closable="!modelLoading" :auto-focus="true" :trap-focus="true" role="dialog" aria-label="选择默认模型">
      <p class="model-disclaimer">模型列表来自所填供应商。拉取成功只表示列表接口可用，不代表生成已通过。</p>
      <div v-if="modelLoading" class="model-loading" role="status"><NSpin /><p>正在拉取模型，最多等待约 6 秒…</p></div>
      <NAlert v-else-if="modelError" type="error" role="alert">{{ modelError }}</NAlert>
      <template v-else>
        <div class="field model-search"><label for="model-search">搜索模型</label><NInput v-model:value="modelSearch" @update:value="modelPage = 1" :input-props="{ id: 'model-search' }" placeholder="输入模型名称" /></div>
        <p class="model-count" role="status">共 {{ modelItems.length }} 个模型，匹配 {{ filteredModels.length }} 个</p>
        <div v-if="!filteredModels.length" class="empty-state compact"><p>{{ modelItems.length ? '没有匹配的模型，请换个关键词。' : '供应商返回了空列表，可关闭弹窗后手动填写。' }}</p></div>
        <ul v-else class="model-list"><li v-for="id in visibleModels" :key="id"><button @click="chooseModel(id)"><span>{{ id }}</span><span class="model-select-label">选择</span></button></li></ul>
        <NButton v-if="visibleModels.length < filteredModels.length" secondary class="model-more" @click="modelPage++">显示更多模型</NButton>
      </template>
      <template #footer><div class="model-footer"><NButton v-if="modelError" secondary :disabled="!canWrite" @click="queryModels(modelTarget)">重新拉取</NButton><NButton :disabled="modelLoading" @click="modelOpen = false">关闭</NButton></div></template>
    </NModal>
  </NConfigProvider>
</template>
