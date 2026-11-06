<script setup lang="ts">
import { computed, ref } from 'vue'
import { NAlert, NButton, NModal } from 'naive-ui'
import { RefreshCw, Copy, ChevronLeft, ChevronRight } from '@lucide/vue'
import { formatTimestamp } from '../request-format'
import type { Binding, PendingSession, Provider } from '../api/types.gen'

const props = defineProps<{
  kind: 'pending' | 'bound'; bindings: Binding[]; pending: PendingSession[]; providers: Provider[];
  choices: Record<string, string>; total: number; size: number; page: number; hasNext: boolean;
  loading: boolean; busy: boolean; writable: boolean; providerName: (id: string) => string; copySession: (id: string) => Promise<boolean>;
}>()
const emit = defineEmits<{
  turn: [direction: 'next' | 'previous' | 'reset']; size: [value: number];
  choose: [id: string, provider: string]; register: [id: string]; copy: [id: string];
}>()
const fullID = ref('')
const copyFeedback = ref('')
const copySucceeded = ref(false)
async function copyFullID() {
  copySucceeded.value = await props.copySession(fullID.value)
  copyFeedback.value = copySucceeded.value ? '会话 ID 已复制。' : '无法访问剪贴板，请选中上方完整 ID 后复制。'
}
const isPending = computed(() => props.kind === 'pending')
const title = computed(() => isPending.value ? '待登记会话' : '已绑定会话')
const items = computed(() => isPending.value ? props.pending : props.bindings)
</script>

<template>
  <section class="session-view list-view" :aria-label="title" :aria-busy="loading">
    <div class="list-toolbar"><p>{{ isPending ? '选择首次供应商后确认；路由器不会自动补发请求。' : '会话始终使用已绑定供应商。' }}</p><NButton secondary size="small" :disabled="loading || busy" :aria-label="isPending ? '刷新待登记' : '刷新已绑定'" @click="emit('turn', 'reset')"><RefreshCw :size="14" aria-hidden="true" />刷新</NButton></div>
    <div class="table-scroll data-viewport" tabindex="0" role="region" :aria-label="isPending ? '待登记会话表格，可横向滚动' : '已绑定会话表格，可横向滚动'">
      <table class="data-table session-table compact-table"><colgroup><col :style="{ width: isPending ? '31%' : '40%' }" /><col :style="{ width: isPending ? '20%' : '25%' }" /><col :style="{ width: isPending ? '20%' : '23%' }" /><col :style="{ width: isPending ? '17%' : '12%' }" /><col v-if="isPending" style="width:12%" /></colgroup>
        <thead><tr><th scope="col">会话 ID</th><th scope="col">{{ isPending ? '首次出现' : '绑定时间' }}</th><th scope="col">{{ isPending ? '原因' : '固定供应商' }}</th><th scope="col">{{ isPending ? '首次供应商' : '操作' }}</th><th v-if="isPending" scope="col">操作</th></tr></thead>
        <tbody>
          <tr v-for="item in items" :key="item.id"><td><div class="session-id"><button type="button" class="truncate session-id-value" :title="item.id" @click="fullID = item.id; copyFeedback = ''">{{ item.id }}</button><button v-if="isPending" type="button" class="copy-id" :aria-label="`复制 ${item.id}`" title="复制完整会话 ID" @click="emit('copy', item.id)"><Copy :size="14" aria-hidden="true" /></button></div></td>
            <td><time :datetime="isPending ? (item as PendingSession).first_seen : (item as Binding).created_at">{{ formatTimestamp(isPending ? (item as PendingSession).first_seen : (item as Binding).created_at) }}</time></td>
            <template v-if="isPending"><td><span class="truncate" :title="(item as PendingSession).reason">{{ (item as PendingSession).reason === 'unknown_origin' ? '历史来源待确认' : (item as PendingSession).reason }}</span></td><td><select :value="choices[item.id] || ''" :aria-label="`会话 ${item.id} 首次供应商`" :disabled="!writable || busy" @change="emit('choose', item.id, ($event.target as HTMLSelectElement).value)"><option disabled value="">请选择供应商</option><option v-for="provider in providers" :key="provider.id" :value="provider.id" :disabled="!provider.base_url">{{ provider.name }}{{ provider.base_url ? '' : '（未配置）' }}</option></select></td><td><NButton type="primary" size="small" :disabled="!writable || !choices[item.id]" :loading="busy" @click="emit('register', item.id)">确认供应商</NButton></td></template>
            <template v-else><td><span class="truncate" :title="providerName((item as Binding).provider_id)">{{ providerName((item as Binding).provider_id) }}</span></td><td><NButton text size="small" :aria-label="`复制 ${item.id}`" @click="emit('copy', item.id)"><Copy :size="14" aria-hidden="true" />复制 ID</NButton></td></template>
          </tr>
          <tr v-if="!items.length"><td :colspan="isPending ? 5 : 4" class="table-empty">{{ loading ? '正在读取会话…' : total ? '此页记录已变更，请刷新或返回上一页。' : isPending ? '没有等待登记的会话' : '还没有已绑定会话' }}</td></tr>
        </tbody>
      </table>
    </div>
    <div class="list-pagination"><span class="table-total" role="status">共 {{ total }} 条</span><div class="pagination-tools"><label>每页 <select :value="size" :aria-label="`${title}每页`" :disabled="loading || busy" @change="emit('size', Number(($event.target as HTMLSelectElement).value))"><option :value="20">20 条</option><option :value="50">50 条</option><option :value="100">100 条</option></select></label><NButton quaternary size="small" aria-label="上一页" :disabled="loading || busy || page <= 1" @click="emit('turn', 'previous')"><ChevronLeft :size="16" aria-hidden="true" /></NButton><span>第 {{ page }} 页</span><NButton quaternary size="small" aria-label="下一页" :disabled="loading || busy || !hasNext" @click="emit('turn', 'next')"><ChevronRight :size="16" aria-hidden="true" /></NButton></div></div>
    <NModal :show="!!fullID" preset="dialog" title="完整会话 ID" role="dialog" aria-label="完整会话 ID" @update:show="value => { if (!value) fullID = '' }"><p class="session-id-full">{{ fullID }}</p><NAlert v-if="copyFeedback" :type="copySucceeded ? 'success' : 'warning'" role="status">{{ copyFeedback }}</NAlert><template #action><NButton secondary @click="copyFullID">复制完整 ID</NButton><NButton @click="fullID = ''">关闭</NButton></template></NModal>
  </section>
</template>
