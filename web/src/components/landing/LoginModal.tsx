import { motion } from 'framer-motion'
import { X } from 'lucide-react'
import { Button } from '../ui'
import { t, Language } from '../../i18n/translations'

interface LoginModalProps {
  onClose: () => void
  language: Language
}

export default function LoginModal({ onClose, language }: LoginModalProps) {
  const goLogin = () => {
    window.history.pushState({}, '', '/login')
    window.dispatchEvent(new PopStateEvent('popstate'))
    onClose()
  }

  return (
    <motion.div
      className="fixed inset-0 z-50 flex items-center justify-center bg-fg/45 p-4"
      initial={{ opacity: 0 }}
      animate={{ opacity: 1 }}
      exit={{ opacity: 0 }}
      onClick={onClose}
    >
      <motion.div
        className="relative w-full max-w-md rounded-lg border border-line bg-surface p-6 shadow-pop"
        initial={{ scale: 0.96, y: 8 }}
        animate={{ scale: 1, y: 0 }}
        exit={{ scale: 0.96, y: 8 }}
        transition={{ duration: 0.15 }}
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
          {t('accessNofxPlatform', language)}
        </h2>
        <p className="mt-2 text-[13px] leading-relaxed text-fg-3">
          {t('loginRegisterPrompt', language)}
        </p>
        <Button
          variant="primary"
          size="lg"
          className="mt-5 w-full"
          onClick={goLogin}
        >
          {t('signIn', language)}
        </Button>
      </motion.div>
    </motion.div>
  )
}
