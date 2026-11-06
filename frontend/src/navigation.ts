/** 管理页面标识；仅调整前端导航，不改变管理API。 */
export type Panel = 'overview' | 'requests' | 'history' | 'clients' | 'providers' | 'sessions-pending' | 'sessions-bound' | 'diagnostics'

/** 页面标题与说明集中定义，菜单、标题和快捷入口采用相同文案。 */
export const pages: Record<Panel, { label: string; description: string }> = {
  overview: { label: '线路总览', description: '选择新会话的起点，已有会话继续使用固定供应商。' },
  requests: { label: '请求明细', description: '查看请求用量、延迟与结果。' },
  history: { label: '历史管理', description: '设置请求保留期限，管理已结束的请求历史。' },
  clients: { label: '客户端与接入', description: '接入日常 Codex，或打开独立测试环境。' },
  providers: { label: '供应商设置', description: '管理上游地址、模型、凭据及连接设置。' },
  'sessions-pending': { label: '待登记会话', description: '确认会话来源并选择首次供应商，再由客户端重试原请求。' },
  'sessions-bound': { label: '已绑定会话', description: '查看会话固定供应商，绑定持续保留。' },
  diagnostics: { label: '近期诊断', description: '查看最近的脱敏结果，不保存正文或认证信息。' },
}

/** 一级菜单和可选子页，供桌面侧栏与手机二级导航共享。 */
export const navigation: { id: string; label: string; shortLabel: string; path: string; page?: Panel; children?: Panel[] }[] = [
  { id: 'overview', label: '线路总览', shortLabel: '总览', page: 'overview', path: 'M3 3h7v7H3zM14 3h7v7h-7zM3 14h7v7H3zM14 14h7v7h-7z' },
  { id: 'requests', label: '请求管理', shortLabel: '请求', children: ['requests', 'history'], path: 'M4 3h16v18H4zM8 7h8M8 12h8M8 17h4' },
  { id: 'clients', label: '客户端与接入', shortLabel: '客户端', page: 'clients', path: 'M3 4h18v12H3zM8 21h8M12 16v5' },
  { id: 'providers', label: '供应商设置', shortLabel: '供应商', page: 'providers', path: 'M5 6h14M5 12h14M5 18h14M9 3v6M15 9v6M9 15v6' },
  { id: 'sessions', label: '会话管理', shortLabel: '会话', children: ['sessions-pending', 'sessions-bound'], path: 'M4 4h16v12H9l-5 4zM8 8h8M8 12h5' },
  { id: 'diagnostics', label: '近期诊断', shortLabel: '诊断', page: 'diagnostics', path: 'M4 3h16v18H4zM8 7h8M8 12h8M8 17h4' },
]
