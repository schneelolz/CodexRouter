import { createApp } from 'vue'
import App from './App.vue'
import './style.css'

async function start() {
  if (import.meta.env.DEV && import.meta.env.MODE === 'mock') {
    const { createMockApi } = await import('./mock')
    window.fetch = createMockApi()
  }
  createApp(App).mount('#app')
}
void start()
