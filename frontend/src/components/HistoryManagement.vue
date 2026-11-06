<script setup lang="ts">
import { onMounted, onUnmounted, ref } from 'vue'
import { NAlert, NButton, NModal } from 'naive-ui'
import { ApiClient, ApiError } from '../api/client'

const props = defineProps<{ api: ApiClient; writable: boolean }>()
const emit = defineEmits<{ failure: [error: unknown] }>()
const days = ref(0)
const loaded = ref(false)
const loading = ref(false)
const busy = ref(false)
const error = ref('')
const notice = ref('')
const confirmation = ref(false)
const lifetime = new AbortController()
function failure(value: unknown) {
  error.value = value instanceof Error ? value.message : '历史设置读取失败。'
  if (value instanceof ApiError && [401, 403].includes(value.status)) emit('failure', value)
}
async function load() {
  loading.value = true
  try { days.value = (await props.api.historyPolicy(lifetime.signal)).days; loaded.value = true; error.value = '' }
  catch (value) { if (!lifetime.signal.aborted) failure(value) }
  finally { loading.value = false }
}
async function save() {
  if (!props.writable || busy.value || !loaded.value || !Number.isInteger(days.value) || days.value < 0 || days.value > 3650) return
  busy.value = true; error.value = ''; notice.value = ''
  try { await props.api.saveHistoryPolicy(days.value, lifetime.signal); notice.value = '保留策略已保存并执行，之后启动时及每小时清理。' }
  catch (value) { failure(value) } finally { busy.value = false }
}
async function clear() {
  if (!props.writable || busy.value) return
  busy.value = true; error.value = ''; notice.value = ''
  try { const result = await props.api.clearHistory(lifetime.signal); confirmation.value = false; notice.value = `已清除 ${result.deleted} 条已结束请求，活动请求及会话绑定保留。` }
  catch (value) { failure(value) } finally { busy.value = false }
}
onMounted(() => { void load() })
onUnmounted(() => lifetime.abort())
</script>

<template>
  <section class="history-management" aria-label="历史管理" :aria-busy="loading || busy">
    <NAlert v-if="error && !confirmation" type="error" role="alert" class="feedback">{{ error }} <NButton text @click="load">重试读取</NButton></NAlert>
    <NAlert v-if="notice" type="success" role="status" class="feedback">{{ notice }}</NAlert>
    <form class="retention-settings" @submit.prevent="save">
      <h2>请求历史保留</h2>
      <p>0 表示不限期；1～3650 表示保留最近相应天数。保存后立即清理过期的已结束请求。</p>
      <div class="table-actions"><label for="history-days">保留天数</label><input id="history-days" v-model.number="days" type="number" min="0" max="3650" step="1" required :disabled="!writable || !loaded || busy" /><NButton attr-type="submit" type="primary" :loading="busy" :disabled="!writable || !loaded || busy || !Number.isInteger(days) || days < 0 || days > 3650">保存保留策略</NButton></div>
    </form>
    <div class="history-danger"><h2>清除请求历史</h2><p>清理不可撤销。活动请求、会话登记、固定路由绑定、供应商和凭据均保留。</p><NButton type="error" secondary :disabled="!writable || busy || loading" @click="confirmation = true">清除全部请求历史</NButton></div>
    <NModal v-model:show="confirmation" preset="dialog" title="确认清除请求历史" role="dialog" aria-label="确认清除请求历史" :mask-closable="!busy" :close-on-esc="!busy" :closable="!busy">
      <NAlert v-if="error" type="error" role="alert">{{ error }}</NAlert><p>仅删除本次操作开始前已结束的请求记录，无法撤销。正在执行及随后产生的请求、待登记会话、固定路由绑定、供应商和凭据均保留。</p>
      <template #action><NButton :disabled="busy" @click="confirmation = false">取消</NButton><NButton type="error" :loading="busy" :disabled="!writable || busy" @click="clear">确认清除</NButton></template>
    </NModal>
  </section>
</template>
