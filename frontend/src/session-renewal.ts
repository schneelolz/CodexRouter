/** 仅在页面可见时续期；失效认证交给调用方展示，不重新生成凭据。 */
export function startSessionRenewal(renew: () => Promise<unknown>, failure: (error: unknown) => void, visible: () => boolean = () => document.visibilityState === 'visible') {
  let stopped = false
  let active = false
  let timer: ReturnType<typeof setTimeout> | undefined
  const check = async () => {
    clearTimeout(timer)
    if (stopped || active || !visible()) return
    active = true
    try { await renew() } catch (error) { if (!stopped) failure(error) }
    finally { active = false; if (!stopped && visible()) timer = setTimeout(() => { void check() }, 5 * 60_000) }
  }
  const visibility = () => { clearTimeout(timer); if (visible()) void check() }
  document.addEventListener('visibilitychange', visibility)
  if (visible()) timer = setTimeout(() => { void check() }, 5 * 60_000)
  return () => { stopped = true; clearTimeout(timer); document.removeEventListener('visibilitychange', visibility) }
}
