import { Globe } from 'lucide-react'
import { useLanguage } from '../../contexts/LanguageContext'
import type { Language } from '../../i18n/translations'

const languages: { code: Language; label: string }[] = [
  { code: 'zh', label: '中文' },
  { code: 'en', label: 'EN' },
  { code: 'id', label: 'ID' },
]

export function LanguageSwitcher() {
  const { language, setLanguage } = useLanguage()

  return (
    <div className="absolute right-4 top-4 z-50 flex h-8 items-center gap-0.5 rounded-md border border-line bg-surface p-0.5">
      <Globe size={14} className="ml-1.5 mr-0.5 text-fg-3" />
      {languages.map(({ code, label }) => (
        <button
          key={code}
          type="button"
          onClick={() => setLanguage(code)}
          className={`rounded px-2 py-1 text-xs font-semibold transition-colors ${
            language === code
              ? 'bg-brand-soft text-brand'
              : 'text-fg-3 hover:bg-surface-hover hover:text-fg'
          }`}
        >
          {label}
        </button>
      ))}
    </div>
  )
}
