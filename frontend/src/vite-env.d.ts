/// <reference types="vite/client" />

// 供原生 TypeScript 检查识别单文件组件；vue-tsc 仍使用真实的 SFC 类型。
declare module '*.vue' {
  import type { DefineComponent } from 'vue'
  const component: DefineComponent<Record<string, unknown>, Record<string, unknown>, unknown>
  export default component
}
