import type { UserMode } from '../../lib/onboarding'
import { Badge } from '../ui'

interface OnboardingModeSelectorProps {
  language: string
  mode: UserMode
  onChange: (mode: UserMode) => void
}

export function OnboardingModeSelector({
  language,
  mode,
  onChange,
}: OnboardingModeSelectorProps) {
  const isZh = language === 'zh'

  const options: Array<{
    id: UserMode
    title: string
    badge?: string
    description: string
  }> = [
    {
      id: 'beginner',
      title: isZh ? '新手模式' : 'Beginner Mode',
      badge: isZh ? '推荐' : 'Recommended',
      description: isZh
        ? '引导你一步步完成模型、交易所和策略配置，最快完成首次启动。'
        : 'Guided step-by-step setup for models, exchanges, and strategy — fastest first run.',
    },
    {
      id: 'advanced',
      title: isZh ? '老手模式' : 'Advanced Mode',
      description: isZh
        ? '保持现在的完整配置流程，你自己决定模型、钱包和交易所。'
        : 'Keep the full manual flow and configure models, wallets, and exchanges yourself.',
    },
  ]

  return (
    <div className="space-y-2">
      <div className="text-xs text-fg-3">
        {isZh ? '使用模式' : 'Experience'}
      </div>
      <div className="grid grid-cols-1 gap-2">
        {options.map((option) => {
          const selected = option.id === mode
          return (
            <button
              key={option.id}
              type="button"
              onClick={() => onChange(option.id)}
              className={`w-full rounded-lg border px-3 py-2.5 text-left transition-colors ${
                selected
                  ? 'border-brand bg-brand-soft'
                  : 'border-line bg-surface-2 hover:border-line-strong'
              }`}
            >
              <div className="flex items-center gap-2 text-[13px] font-semibold text-fg">
                <span>{option.title}</span>
                {option.badge ? (
                  <Badge variant="brand" size="xs">
                    {option.badge}
                  </Badge>
                ) : null}
              </div>
              <p className="mt-1 text-xs leading-5 text-fg-3">
                {option.description}
              </p>
            </button>
          )
        })}
      </div>
    </div>
  )
}
