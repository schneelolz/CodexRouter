<script setup lang="ts">
import { NButton, NInput } from 'naive-ui'
export interface RelayDraft { name: string; base_url: string; model: string }
const props = defineProps<{ draft: RelayDraft; fieldId: string; keyValue: string; keyConfigured?: boolean; locked?: boolean; disabled: boolean; busy: boolean }>()
const emit = defineEmits<{ 'update:keyValue': [value: string]; save: []; query: [] }>()
</script>

<template>
  <form class="relay-form" @submit.prevent="emit('save')">
    <div class="provider-fields">
      <div class="field"><label :for="`${fieldId}-name`">供应商名称 <span class="required-label">必填</span></label><NInput v-model:value="props.draft.name" :input-props="{ id: `${fieldId}-name`, autocomplete: 'off', maxlength: 64, required: true }" :disabled="disabled" placeholder="例如 公司、备用中转" /></div>
      <div class="field"><label :for="`${fieldId}-url`">BaseUrl <span class="required-label">必填</span></label><NInput v-model:value="props.draft.base_url" :input-props="{ id: `${fieldId}-url`, autocomplete: 'off', required: true, spellcheck: false }" :disabled="disabled || locked" placeholder="https://api.example.com/v1" /><p>{{ locked ? '已有会话使用该地址，因此不能修改。' : '填写供应商提供的完整 API 基址，路径原样保留。' }}</p></div>
      <div class="field"><label :for="`${fieldId}-key`">API Key <span class="required-label">{{ keyConfigured ? '已配置' : '必填' }}</span></label><NInput :value="keyValue" @update:value="emit('update:keyValue', $event)" type="password" :placeholder="keyConfigured ? '留空保持原密钥' : '输入该供应商的 API Key'" :input-props="{ id: `${fieldId}-key`, autocomplete: 'new-password', required: !keyConfigured, spellcheck: false }" :disabled="disabled" /><p>密钥保存后不回显。修改地址时需重新输入密钥。</p></div>
      <div class="field"><label :for="`${fieldId}-model`">默认模型 <span class="optional-label">可选</span></label><div class="model-input"><NInput v-model:value="props.draft.model" :input-props="{ id: `${fieldId}-model`, autocomplete: 'off', maxlength: 128 }" :disabled="disabled" placeholder="留空沿用客户端模型" /><NButton secondary :disabled="disabled || !props.draft.base_url.trim() || (!keyConfigured && !keyValue.trim())" @click="emit('query')"><template #icon><svg viewBox="0 0 24 24" aria-hidden="true"><path d="M12 3v12m-5-5 5 5 5-5M4 16v5h16v-5" /></svg></template>拉取模型</NButton></div><p>可从列表选择，也可手动填写。修改后会影响该供应商已有会话的后续请求。</p></div>
    </div>
    <div class="provider-actions"><NButton attr-type="submit" type="primary" :disabled="disabled || !props.draft.name.trim() || !props.draft.base_url.trim() || (!keyConfigured && !keyValue.trim())" :loading="busy"><template #icon><svg viewBox="0 0 24 24" aria-hidden="true"><path d="m5 12 4 4L19 6" /></svg></template>保存供应商</NButton><slot name="actions" /></div>
  </form>
</template>
