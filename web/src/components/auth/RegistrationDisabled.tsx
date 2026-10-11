import { useLanguage } from '../../contexts/LanguageContext'
import { t } from '../../i18n/translations'

export function RegistrationDisabled() {
  const { language } = useLanguage()

  const handleBackToLogin = () => {
    window.history.pushState({}, '', '/login')
    window.dispatchEvent(new PopStateEvent('popstate'))
  }

  return (
    <div
      className="min-h-screen flex items-center justify-center"
      style={{ background: 'var(--bg)', color: 'var(--fg)' }}
    >
      <div className="text-center max-w-md px-6">
        <img
          src="/icons/nofx.svg"
          alt="NoFx Logo"
          className="w-16 h-16 mx-auto mb-4"
        />
        <h1 className="text-2xl font-semibold mb-3">
          {t('registrationClosed', language)}
        </h1>
        <p className="text-sm text-fg-3">
          {t('registrationClosedMessage', language)}
        </p>
        <button
          className="mt-6 px-4 py-2 rounded text-sm font-semibold transition-colors hover:opacity-90"
          style={{ background: 'var(--brand)', color: 'var(--brand-fg)' }}
          onClick={handleBackToLogin}
        >
          {t('backToLogin', language)}
        </button>
      </div>
    </div>
  )
}
