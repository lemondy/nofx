import React, { useState, useEffect } from 'react'
import { Eye, EyeOff } from 'lucide-react'
import { useAuth } from '../../contexts/AuthContext'
import { invalidateSystemConfig } from '../../lib/config'
import { OnboardingModeSelector } from '../auth/OnboardingModeSelector'
import type { UserMode } from '../../lib/onboarding'
import { useLanguage } from '../../contexts/LanguageContext'
import { LanguageSwitcher } from '../common/LanguageSwitcher'
import { Button, Card, Input } from '../ui'

const labels = {
  zh: {
    welcome: '欢迎使用 NOFX',
    subtitle: '创建账号开始使用',
    email: '邮箱',
    emailPlaceholder: 'you@example.com',
    password: '密码',
    passwordPlaceholder: '至少 8 个字符',
    passwordError: '密码至少需要 8 个字符',
    submit: '开始使用',
    submitting: '创建中...',
    setupFailed: '创建失败，请重试',
    singleUser: '单用户系统 — 这是唯一的账号',
  },
  en: {
    welcome: 'Welcome to NOFX',
    subtitle: 'Create your account to get started',
    email: 'Email',
    emailPlaceholder: 'you@example.com',
    password: 'Password',
    passwordPlaceholder: 'At least 8 characters',
    passwordError: 'Password must be at least 8 characters',
    submit: 'Get Started',
    submitting: 'Creating account...',
    setupFailed: 'Setup failed, please try again',
    singleUser: 'Single-user system — this is the only account',
  },
  id: {
    welcome: 'Selamat Datang di NOFX',
    subtitle: 'Buat akun untuk memulai',
    email: 'Email',
    emailPlaceholder: 'you@example.com',
    password: 'Kata Sandi',
    passwordPlaceholder: 'Minimal 8 karakter',
    passwordError: 'Kata sandi minimal 8 karakter',
    submit: 'Mulai',
    submitting: 'Membuat akun...',
    setupFailed: 'Gagal membuat akun, coba lagi',
    singleUser: 'Sistem pengguna tunggal — ini satu-satunya akun',
  },
} as const

export function SetupPage() {
  const { language } = useLanguage()
  const { register } = useAuth()
  const [email, setEmail] = useState('')
  const [password, setPassword] = useState('')
  const [showPassword, setShowPassword] = useState(false)
  const [error, setError] = useState('')
  const [loading, setLoading] = useState(false)
  const [mode, setMode] = useState<UserMode>('beginner')

  // Clean up any stale auth/onboarding state on setup page load
  useEffect(() => {
    localStorage.removeItem('auth_token')
    localStorage.removeItem('auth_user')
    localStorage.removeItem('user_id')
    localStorage.removeItem('nofx_beginner_onboarding_completed')
    localStorage.removeItem('nofx_beginner_wallet_address')
  }, [])

  const l = labels[language as keyof typeof labels] || labels.en

  const handleSubmit = async (e: React.FormEvent) => {
    e.preventDefault()
    setError('')
    if (password.length < 8) {
      setError(l.passwordError)
      return
    }
    setLoading(true)
    const result = await register(email, password, undefined, mode)
    setLoading(false)
    if (result.success) {
      invalidateSystemConfig()
    } else {
      setError(result.message || l.setupFailed)
    }
  }

  return (
    <div className="relative min-h-screen w-full bg-bg">
      <LanguageSwitcher />

      <div className="relative z-10 flex min-h-screen items-center justify-center px-4 py-16">
        <div className="w-full max-w-sm">
          {/* Logo + Title */}
          <div className="mb-6 text-center">
            <div className="mb-3 flex justify-center">
              <img src="/icons/nofx.svg" alt="NOFX" className="h-12 w-12" />
            </div>
            <h1 className="mb-1 text-xl font-semibold text-fg">{l.welcome}</h1>
            <p className="text-[13px] text-fg-3">{l.subtitle}</p>
          </div>

          {/* Card */}
          <Card className="p-5">
            <form onSubmit={handleSubmit} className="space-y-4">
              {/* Email */}
              <div>
                <label className="mb-1 block text-xs font-medium text-fg-3">
                  {l.email}
                </label>
                <Input
                  type="email"
                  value={email}
                  onChange={(e) => setEmail(e.target.value)}
                  placeholder={l.emailPlaceholder}
                  required
                  autoFocus
                />
              </div>

              {/* Password */}
              <div>
                <label className="mb-1 block text-xs font-medium text-fg-3">
                  {l.password}
                </label>
                <div className="relative">
                  <Input
                    type={showPassword ? 'text' : 'password'}
                    value={password}
                    onChange={(e) => setPassword(e.target.value)}
                    className="pr-9"
                    placeholder={l.passwordPlaceholder}
                    required
                  />
                  <button
                    type="button"
                    onClick={() => setShowPassword(!showPassword)}
                    className="absolute right-3 top-1/2 -translate-y-1/2 text-fg-3 transition-colors hover:text-fg-2"
                  >
                    {showPassword ? <EyeOff size={16} /> : <Eye size={16} />}
                  </button>
                </div>
              </div>

              <OnboardingModeSelector
                language={language}
                mode={mode}
                onChange={setMode}
              />

              {/* Error */}
              {error && (
                <p className="rounded-md border border-down/30 bg-down-soft px-3 py-2 text-xs text-down">
                  {error}
                </p>
              )}

              {/* Submit */}
              <Button
                type="submit"
                variant="primary"
                size="lg"
                disabled={loading}
                loading={loading}
                className="w-full"
              >
                {loading ? l.submitting : l.submit}
              </Button>
            </form>
          </Card>

          <p className="mt-4 text-center text-xs text-fg-3">{l.singleUser}</p>
        </div>
      </div>
    </div>
  )
}
