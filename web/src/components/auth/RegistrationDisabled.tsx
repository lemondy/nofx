import { useLanguage } from '../../contexts/LanguageContext'
import { t } from '../../i18n/translations'
import { Button } from '../ui'

export function RegistrationDisabled() {
  const { language } = useLanguage()

  const handleBackToLogin = () => {
    window.history.pushState({}, '', '/login')
    window.dispatchEvent(new PopStateEvent('popstate'))
  }

  return (
    <div className="flex min-h-screen items-center justify-center bg-bg px-6 text-fg">
      <div className="max-w-md text-center">
        <img
          src="/icons/nofx.svg"
          alt="NoFx Logo"
          className="mx-auto mb-4 h-12 w-12"
        />
        <h1 className="mb-3 text-xl font-semibold">
          {t('registrationClosed', language)}
        </h1>
        <p className="text-[13px] leading-relaxed text-fg-3">
          {t('registrationClosedMessage', language)}
        </p>
        <Button variant="primary" className="mt-6" onClick={handleBackToLogin}>
          {t('backToLogin', language)}
        </Button>
      </div>
    </div>
  )
}
