import { fireEvent, render, screen } from '@testing-library/react'
import { MemoryRouter, useLocation } from 'react-router-dom'
import { describe, expect, it, vi } from 'vitest'
import HeaderBar from './HeaderBar'

function Location() {
  const l = useLocation()
  return <output data-testid="location">{l.pathname + l.hash}</output>
}

describe('research menu in the top nav', () => {
  it('puts a single 研究 entry after 排行榜 with the three pages as its menu', () => {
    render(
      <MemoryRouter>
        <HeaderBar language="zh" isLoggedIn />
      </MemoryRouter>
    )
    const names = screen.getAllByRole('button').map((b) => b.textContent)
    const rank = names.indexOf('排行榜')
    expect(names[rank + 1]).toBe('研究')
    expect(names.slice(rank + 2, rank + 5)).toEqual([
      '因子挖掘',
      '晋升报告',
      '研究记录',
    ])
    expect(names).not.toContain('系统质量')
  })

  it('opens the default page from the parent and a specific page from the menu', () => {
    const onPageChange = vi.fn()
    render(
      <MemoryRouter>
        <HeaderBar language="zh" isLoggedIn onPageChange={onPageChange} />
        <Location />
      </MemoryRouter>
    )
    fireEvent.click(screen.getByRole('button', { name: '研究' }))
    expect(screen.getByTestId('location')).toHaveTextContent(/^\/research$/)
    fireEvent.click(screen.getByRole('button', { name: '晋升报告' }))
    expect(screen.getByTestId('location')).toHaveTextContent(
      '/research#paper-promotion'
    )
    expect(onPageChange).toHaveBeenCalledWith('research')
  })

  it('requires login before opening research pages', () => {
    const onLoginRequired = vi.fn()
    render(
      <MemoryRouter>
        <HeaderBar language="zh" onLoginRequired={onLoginRequired} />
        <Location />
      </MemoryRouter>
    )
    fireEvent.click(screen.getByRole('button', { name: '因子挖掘' }))
    expect(onLoginRequired).toHaveBeenCalled()
    expect(screen.getByTestId('location')).toHaveTextContent(/^\/$/)
  })
})
