import React, { useState, useEffect } from 'react'
import { Eye, EyeOff } from 'lucide-react'
import { toast } from 'sonner'
import { useAuth } from '../../contexts/AuthContext'
import { useLanguage } from '../../contexts/LanguageContext'
import { t } from '../../i18n/translations'
import { DeepVoidBackground } from '../common/DeepVoidBackground'
import { LanguageSwitcher } from '../common/LanguageSwitcher'
import { Button, Card, Input } from '../ui'
import { OnboardingModeSelector } from './OnboardingModeSelector'
import type { UserMode } from '../../lib/onboarding'

export function LoginPage() {
  const { language } = useLanguage()
  const { login } = useAuth()
  const [email, setEmail] = useState('')
  const [password, setPassword] = useState('')
  const [showPassword, setShowPassword] = useState(false)
  const [error, setError] = useState('')
  const [loading, setLoading] = useState(false)
  const [expiredToastId, setExpiredToastId] = useState<string | number | null>(
    null
  )
  const [mode, setMode] = useState<UserMode>('beginner')

  // Clean up stale auth state once on mount
  useEffect(() => {
    localStorage.removeItem('auth_token')
    localStorage.removeItem('auth_user')
    localStorage.removeItem('user_id')
  }, [])

  // Show session-expired toast (re-runs on language change to update text)
  useEffect(() => {
    if (sessionStorage.getItem('from401') === 'true') {
      const id = toast.warning(t('sessionExpired', language), {
        duration: Infinity,
      })
      setExpiredToastId(id)
      sessionStorage.removeItem('from401')
    }
  }, [language])

  const handleLogin = async (e: React.FormEvent) => {
    e.preventDefault()
    setError('')
    setLoading(true)
    const result = await login(email, password, mode)
    setLoading(false)
    if (result.success) {
      if (expiredToastId) toast.dismiss(expiredToastId)
    } else {
      const msg = result.message || t('loginFailed', language)
      setError(msg)
      toast.error(msg)
    }
  }

  return (
    <DeepVoidBackground disableAnimation>
      <LanguageSwitcher />

      <div className="flex flex-1 items-center justify-center px-4 py-16">
        <div className="w-full max-w-[400px]">
          {/* Logo + Title */}
          <div className="mb-6 text-center">
            <img
              src="/icons/nofx.svg"
              alt="NOFX"
              className="mx-auto mb-4 h-10 w-10"
            />
            <h1 className="text-xl font-semibold text-fg">
              {t('signIn', language)}
            </h1>
            <p className="mt-1 text-[13px] text-fg-3">NOFX</p>
          </div>

          <Card className="p-6">
            <form onSubmit={handleLogin} className="space-y-4">
              {/* Email */}
              <div>
                <label className="mb-1.5 block text-xs text-fg-3">
                  {t('email', language)}
                </label>
                <Input
                  type="email"
                  value={email}
                  onChange={(e) => setEmail(e.target.value)}
                  placeholder="you@example.com"
                  required
                  autoFocus
                />
              </div>

              {/* Password */}
              <div>
                <div className="mb-1.5 flex items-center justify-between">
                  <label className="text-xs text-fg-3">
                    {t('password', language)}
                  </label>
                  <button
                    type="button"
                    onClick={() => (window.location.href = '/reset-password')}
                    className="text-xs text-fg-3 transition-colors hover:text-brand"
                  >
                    {t('forgotPassword', language)}
                  </button>
                </div>
                <div className="relative">
                  <Input
                    type={showPassword ? 'text' : 'password'}
                    value={password}
                    onChange={(e) => setPassword(e.target.value)}
                    placeholder="••••••••"
                    required
                    className="pr-10"
                  />
                  <button
                    type="button"
                    onClick={() => setShowPassword(!showPassword)}
                    className="absolute right-3 top-1/2 -translate-y-1/2 text-fg-3 transition-colors hover:text-fg-2"
                    tabIndex={-1}
                    aria-label={showPassword ? 'Hide' : 'Show'}
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
                loading={loading}
                className="w-full"
              >
                {t('signIn', language)}
              </Button>
            </form>
          </Card>
        </div>
      </div>
    </DeepVoidBackground>
  )
}
