import { afterEach, describe, expect, it, vi } from 'vitest'
import { cleanup, fireEvent, render, screen } from '@testing-library/react'
import { NofxSelect } from './select'

const options = [
  { value: 'a', label: 'mac-nofx' },
  { value: 'b', label: '因子量化-加密（程序化模拟）' },
]

afterEach(() => {
  cleanup()
  vi.restoreAllMocks()
})

// jsdom has no layout: stub the trigger rect, viewport and menu width.
function openAt(triggerLeft: number, triggerWidth: number, menuWidth: number) {
  vi.spyOn(document.documentElement, 'clientWidth', 'get').mockReturnValue(1000)
  vi.spyOn(HTMLElement.prototype, 'offsetWidth', 'get').mockReturnValue(
    menuWidth
  )
  vi.spyOn(HTMLElement.prototype, 'getBoundingClientRect').mockReturnValue({
    left: triggerLeft,
    right: triggerLeft + triggerWidth,
    width: triggerWidth,
    top: 10,
    bottom: 40,
    height: 30,
    x: triggerLeft,
    y: 10,
    toJSON: () => ({}),
  })
  render(<NofxSelect value="a" onChange={() => {}} options={options} />)
  fireEvent.click(screen.getByRole('combobox'))
  return screen.getByTitle(options[1].label).parentElement as HTMLElement
}

describe('NofxSelect menu position', () => {
  it('stays left-aligned with the trigger when it fits', () => {
    const menu = openAt(100, 120, 300)
    expect(menu.style.left).toBe('100px')
  })

  it('right-aligns with a trigger near the right edge', () => {
    const menu = openAt(870, 120, 300)
    expect(menu.style.left).toBe('690px')
  })

  it('never starts left of the viewport gap', () => {
    const menu = openAt(900, 90, 1200)
    expect(menu.style.left).toBe('8px')
  })
})
