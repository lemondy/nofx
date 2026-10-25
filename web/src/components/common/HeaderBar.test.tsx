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

describe('funds tab in the top nav', () => {
  it('places 资金 after 看板 and navigates to /funds on click', () => {
    const onPageChange = vi.fn()
    render(
      <MemoryRouter>
        <HeaderBar language="zh" isLoggedIn onPageChange={onPageChange} />
        <Location />
      </MemoryRouter>
    )
    const names = screen.getAllByRole('button').map((b) => b.textContent)
    const dashboard = names.indexOf('看板')
    expect(dashboard).toBeGreaterThan(-1)
    expect(names[dashboard + 1]).toBe('资金')

    fireEvent.click(screen.getByRole('button', { name: '资金' }))
    expect(screen.getByTestId('location')).toHaveTextContent(/^\/funds$/)
    expect(onPageChange).toHaveBeenCalledWith('funds')
  })

  it('labels the funds tab as Funds in English', () => {
    render(
      <MemoryRouter>
        <HeaderBar language="en" isLoggedIn />
      </MemoryRouter>
    )
    expect(screen.getByRole('button', { name: 'Funds' })).toBeInTheDocument()
  })

  it('requires login before opening the funds page', () => {
    const onLoginRequired = vi.fn()
    render(
      <MemoryRouter>
        <HeaderBar language="zh" onLoginRequired={onLoginRequired} />
        <Location />
      </MemoryRouter>
    )
    fireEvent.click(screen.getByRole('button', { name: '资金' }))
    expect(onLoginRequired).toHaveBeenCalledWith('资金')
    expect(screen.getByTestId('location')).toHaveTextContent(/^\/$/)
  })
})
