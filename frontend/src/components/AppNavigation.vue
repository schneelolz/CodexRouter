<script setup lang="ts">
import { onMounted, onUnmounted, reactive, ref, watch } from 'vue'
import { navigation, pages } from '../navigation'
import type { Panel } from '../navigation'

const props = defineProps<{ active: Panel; pending: number }>()
const emit = defineEmits<{ navigate: [panel: Panel] }>()
const media = window.matchMedia('(max-width: 760px)')
const mobile = ref(media.matches)
const resize = () => { mobile.value = media.matches }
onMounted(() => media.addEventListener('change', resize))
onUnmounted(() => media.removeEventListener('change', resize))
const expanded = reactive<Record<string, boolean>>({})
const lastChild = reactive<Record<string, Panel>>({ requests: 'requests', sessions: 'sessions-pending' })
watch(() => props.active, panel => {
  const group = navigation.find(item => item.children?.includes(panel))
  if (group) { expanded[group.id] = true; lastChild[group.id] = panel }
}, { immediate: true })
function activate(item: typeof navigation[number]) {
  if (item.page) emit('navigate', item.page)
  else if (mobile.value) emit('navigate', lastChild[item.id]!)
  else expanded[item.id] = !expanded[item.id]
}
</script>

<template>
  <nav class="primary-navigation" aria-label="主要导航">
    <div v-for="item in navigation" :key="item.id" class="nav-entry">
      <button type="button" :aria-label="item.label" :aria-current="item.page === active ? 'page' : undefined" :aria-expanded="item.children && !mobile ? !!expanded[item.id] : undefined" :aria-controls="item.children && !mobile ? `nav-${item.id}` : undefined" :class="{ active: item.page === active, 'group-active': item.children?.includes(active) }" @click="activate(item)">
        <svg viewBox="0 0 24 24" aria-hidden="true"><path :d="item.path" /></svg>
        <span class="desktop-nav-label">{{ item.label }}</span><span class="mobile-nav-label">{{ item.shortLabel }}</span>
        <span v-if="item.id === 'sessions' && pending" class="nav-count">{{ pending }}</span>
        <svg v-if="item.children" class="nav-chevron" :class="{ expanded: expanded[item.id] }" viewBox="0 0 24 24" aria-hidden="true"><path d="m9 5 7 7-7 7" /></svg>
      </button>
      <div v-if="item.children" v-show="expanded[item.id]" :id="`nav-${item.id}`" class="nav-children">
        <button v-for="child in item.children" :key="child" type="button" class="nav-subitem" :aria-label="pages[child].label" :aria-description="child === 'sessions-pending' && pending ? `${pending} 个待登记会话` : undefined" :class="{ active: active === child }" :aria-current="active === child ? 'page' : undefined" @click="emit('navigate', child)"><span>{{ pages[child].label }}</span><span v-if="child === 'sessions-pending' && pending" class="nav-count">{{ pending }}</span></button>
      </div>
    </div>
  </nav>
</template>
