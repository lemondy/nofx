import { motion, AnimatePresence } from 'framer-motion'
import { Check, LogIn, UserPlus, X } from 'lucide-react'
import { buttonVariants } from '../ui'
import { useLanguage } from '../../contexts/LanguageContext'
import { t } from '../../i18n/translations'

interface LoginRequiredOverlayProps {
  isOpen: boolean
  onClose: () => void
  featureName?: string
}

export function LoginRequiredOverlay({
  isOpen,
  onClose,
  featureName,
}: LoginRequiredOverlayProps) {
  const { language } = useLanguage()

  const tr = (key: string, params?: Record<string, string | number>) =>
    t(`loginRequired.${key}`, language, params)

  const subtitle = featureName
    ? tr('subtitleWithFeature', { featureName })
    : tr('subtitleDefault')

  const benefits = [tr('benefit1'), tr('benefit2'), tr('benefit4')]

  return (
    <AnimatePresence>
      {isOpen && (
        <motion.div
          initial={{ opacity: 0 }}
          animate={{ opacity: 1 }}
          exit={{ opacity: 0 }}
          transition={{ duration: 0.15 }}
          className="fixed inset-0 z-50 flex items-center justify-center bg-fg/45 p-4"
          onClick={onClose}
        >
          <motion.div
            initial={{ opacity: 0, scale: 0.96, y: 8 }}
            animate={{ opacity: 1, scale: 1, y: 0 }}
            exit={{ opacity: 0, scale: 0.96, y: 8 }}
            transition={{ duration: 0.15 }}
            className="relative w-full max-w-sm rounded-lg border border-line bg-surface p-6 shadow-pop"
            onClick={(e) => e.stopPropagation()}
          >
            <button
              type="button"
              onClick={onClose}
              aria-label="Close"
              className="absolute right-3 top-3 flex h-8 w-8 items-center justify-center rounded-md text-fg-3 transition-colors hover:bg-surface-hover hover:text-fg"
            >
              <X size={16} />
            </button>

            <h2 className="pr-8 text-lg font-semibold text-fg">
              {tr('title')}
            </h2>
            <p className="mt-1.5 text-[13px] text-fg-3">{subtitle}</p>
            <p className="mt-3 text-[13px] leading-relaxed text-fg-2">
              {tr('description')}
            </p>

            <ul className="mt-4 space-y-2 border-t border-line pt-4">
              {benefits.map((benefit, i) => (
                <li
                  key={i}
                  className="flex items-start gap-2 text-[13px] text-fg-2"
                >
                  <Check size={14} className="mt-0.5 shrink-0 text-up" />
                  {benefit}
                </li>
              ))}
            </ul>

            <div className="mt-5 flex flex-col gap-2 sm:flex-row">
              <a
                href="/login"
                className={`${buttonVariants({
                  variant: 'primary',
                  size: 'md',
                })} flex-1`}
              >
                <LogIn size={14} />
                {tr('loginButton')}
              </a>
              <a
                href="/register"
                className={`${buttonVariants({
                  variant: 'secondary',
                  size: 'md',
                })} flex-1`}
              >
                <UserPlus size={14} />
                {tr('registerButton')}
              </a>
            </div>

            <div className="mt-4 text-center">
              <button
                type="button"
                onClick={onClose}
                className="text-xs text-fg-3 transition-colors hover:text-fg-2 hover:underline"
              >
                {tr('abort')}
              </button>
            </div>
          </motion.div>
        </motion.div>
      )}
    </AnimatePresence>
  )
}
