import { afterEach, describe, expect, it, vi } from 'vitest'
import { startSessionRenewal } from './session-renewal'

describe('可见管理页续期', () => {
  afterEach(() => { vi.useRealTimers(); vi.unstubAllGlobals() })
  it('可见五分钟续期，隐藏八小时不续期，恢复可见立即检查并正确清理', async () => {
    vi.useFakeTimers()
    const events = new EventTarget()
    vi.stubGlobal('document', events)
    let visible = true
    const renew = vi.fn().mockResolvedValue(undefined)
    const stop = startSessionRenewal(renew, vi.fn(), () => visible)
    await vi.advanceTimersByTimeAsync(300000)
    expect(renew).toHaveBeenCalledTimes(1)
    visible = false; events.dispatchEvent(new Event('visibilitychange'))
    await vi.advanceTimersByTimeAsync(8 * 3600000)
    expect(renew).toHaveBeenCalledTimes(1)
    visible = true; events.dispatchEvent(new Event('visibilitychange')); await vi.advanceTimersByTimeAsync(0)
    expect(renew).toHaveBeenCalledTimes(2)
    stop(); await vi.advanceTimersByTimeAsync(600000)
    expect(renew).toHaveBeenCalledTimes(2)
  })
  it('并发可见事件不重复续期，失效错误交给界面，不自动申请新凭据', async () => {
    vi.useFakeTimers()
    const events = new EventTarget(); vi.stubGlobal('document', events)
    let reject: (error: unknown) => void = () => {}
    const renew = vi.fn(() => new Promise((_, fail) => { reject = fail }))
    const failure = vi.fn()
    const stop = startSessionRenewal(renew, failure, () => true)
    events.dispatchEvent(new Event('visibilitychange')); events.dispatchEvent(new Event('visibilitychange'))
    expect(renew).toHaveBeenCalledTimes(1)
    reject(new Error('expired')); await vi.advanceTimersByTimeAsync(0)
    expect(failure).toHaveBeenCalledOnce()
    stop()
  })
})
