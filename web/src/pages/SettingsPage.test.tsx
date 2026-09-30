import React from 'react'
import { beforeEach, afterEach, describe, expect, it, vi } from 'vitest'
import {
  render,
  fireEvent,
  screen,
  waitFor,
  cleanup,
} from '@testing-library/react'
import { SettingsPage } from './SettingsPage'

const { logout, success, error } = vi.hoisted(() => ({
  logout: vi.fn(),
  success: vi.fn(),
  error: vi.fn(),
}))
vi.mock('../contexts/AuthContext', () => ({
  useAuth: () => ({
    user: { id: 'u', email: 'u@test.com' },
    token: 'real-auth-token',
    logout,
  }),
}))
vi.mock('../contexts/LanguageContext', () => ({
  useLanguage: () => ({ language: 'en' }),
}))
vi.mock('../components/trader/ExchangeConfigModal', () => ({
  ExchangeConfigModal: () => null,
}))
vi.mock('../components/trader/ModelConfigModal', () => ({
  ModelConfigModal: () => null,
}))
vi.mock('../components/trader/TelegramConfigModal', () => ({
  TelegramConfigModal: () => null,
}))
vi.mock('sonner', () => ({ toast: { success, error } }))

describe('Settings password security', () => {
  beforeEach(() => {
    vi.clearAllMocks()
    localStorage.clear()
  })
  afterEach(() => {
    cleanup()
    vi.unstubAllGlobals()
  })
  function submit() {
    render(<SettingsPage />)
    fireEvent.change(screen.getByLabelText('Current Password'), {
      target: { value: 'old-password' },
    })
    fireEvent.change(screen.getByPlaceholderText('At least 8 characters'), {
      target: { value: 'new-password' },
    })
    fireEvent.click(screen.getByRole('button', { name: 'Update Password' }))
  }
  it('uses the auth context token, sends current password and signs out on success', async () => {
    const fetchMock = vi.fn().mockResolvedValue({ ok: true })
    vi.stubGlobal('fetch', fetchMock)
    submit()
    await waitFor(() => expect(logout).toHaveBeenCalledOnce())
    const [path, request] = fetchMock.mock.calls[0]
    expect(path).toBe('/api/user/password')
    expect(request.headers.Authorization).toBe('Bearer real-auth-token')
    expect(JSON.parse(request.body)).toEqual({
      current_password: 'old-password',
      new_password: 'new-password',
    })
  })
  it('does not sign out or claim success when the current password is rejected', async () => {
    vi.stubGlobal(
      'fetch',
      vi
        .fn()
        .mockResolvedValue({
          ok: false,
          json: async () => ({ error: 'Current password incorrect' }),
        })
    )
    submit()
    await waitFor(() =>
      expect(error).toHaveBeenCalledWith('Current password incorrect')
    )
    expect(logout).not.toHaveBeenCalled()
    expect(success).not.toHaveBeenCalled()
  })
})
