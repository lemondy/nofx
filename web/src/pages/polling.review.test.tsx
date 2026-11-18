import { act, cleanup, fireEvent, render } from '@testing-library/react'
import { afterEach, expect, test, vi } from 'vitest'
import FactorMiningPage from './FactorMiningPage'
import PaperPromotionPage from './PaperPromotionPage'
afterEach(() => {
  cleanup()
  vi.useRealTimers()
  vi.unstubAllGlobals()
})
test.each([FactorMiningPage, PaperPromotionPage])(
  'overlapping visibility refreshes schedule a single timer',
  async (Page) => {
    vi.useFakeTimers()
    const pending: ((v: Response) => void)[] = []
    vi.stubGlobal(
      'fetch',
      vi.fn(() => new Promise<Response>((resolve) => pending.push(resolve)))
    )
    render(<Page language="en" />)
    fireEvent(document, new Event('visibilitychange'))
    await act(async () => {
      for (const resolve of pending)
        resolve({ ok: true, json: async () => null } as Response)
    })
    expect(vi.getTimerCount()).toBe(1)
  }
)

test.each([FactorMiningPage, PaperPromotionPage])(
  'late older errors cannot overwrite a newer successful refresh',
  async (Page) => {
    vi.useFakeTimers()
    const pending: ((v: Response) => void)[] = []
    vi.stubGlobal(
      'fetch',
      vi.fn(() => new Promise<Response>((resolve) => pending.push(resolve)))
    )
    const { container } = render(<Page language="en" />)
    const oldCount = pending.length
    fireEvent(document, new Event('visibilitychange'))
    await act(async () => {
      for (const resolve of pending.slice(oldCount))
        resolve({ ok: true, json: async () => null } as Response)
    })
    await act(async () => {
      for (const resolve of pending.slice(0, oldCount))
        resolve({ ok: false, status: 500 } as Response)
    })
    expect(container).not.toHaveTextContent('Request failed')
    expect(vi.getTimerCount()).toBe(1)
  }
)
