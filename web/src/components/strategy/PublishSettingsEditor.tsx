import { Badge } from '../ui'
import { Globe, Lock, Eye, EyeOff } from 'lucide-react'
import { publishSettings, ts } from '../../i18n/strategy-translations'

interface PublishSettingsEditorProps {
  isPublic: boolean
  configVisible: boolean
  onIsPublicChange: (value: boolean) => void
  onConfigVisibleChange: (value: boolean) => void
  disabled?: boolean
  language: string
}

export function PublishSettingsEditor({
  isPublic,
  configVisible,
  onIsPublicChange,
  onConfigVisibleChange,
  disabled = false,
  language,
}: PublishSettingsEditorProps) {
  return (
    <div className="space-y-3">
      <button
        type="button"
        role="switch"
        aria-checked={isPublic}
        aria-label={ts(publishSettings.publishToMarket, language)}
        disabled={disabled}
        onClick={() => !disabled && onIsPublicChange(!isPublic)}
        className={`flex w-full items-center justify-between gap-3 rounded-md border p-3 text-left transition-colors focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-brand/50 disabled:cursor-not-allowed disabled:opacity-50 ${isPublic ? 'border-brand bg-brand-soft' : 'border-line bg-surface-2 hover:bg-surface-2'}`}
      >
        <span className="flex min-w-0 items-start gap-2">
          {isPublic ? (
            <Globe className="mt-0.5 h-4 w-4 shrink-0 text-brand" />
          ) : (
            <Lock className="mt-0.5 h-4 w-4 shrink-0 text-fg-3" />
          )}
          <span>
            <span className="block text-[13px] font-medium text-fg-2">
              {ts(publishSettings.publishToMarket, language)}
            </span>
            <span className="mt-1 block text-xs text-fg-3">
              {ts(publishSettings.publishDesc, language)}
            </span>
          </span>
        </span>
        <span className="flex shrink-0 flex-col items-end gap-2 sm:flex-row sm:items-center">
          <Badge variant={isPublic ? 'up' : 'neutral'} size="xs">
            {isPublic
              ? ts(publishSettings.public, language)
              : ts(publishSettings.private, language)}
          </Badge>
          <span
            aria-hidden="true"
            className={`relative h-5 w-9 rounded-full ${isPublic ? 'bg-brand' : 'bg-line-strong'}`}
          >
            <span
              className={`absolute top-0.5 h-4 w-4 rounded-full bg-surface transition-transform ${isPublic ? 'translate-x-[18px]' : 'translate-x-0.5'}`}
            />
          </span>
        </span>
      </button>
      {isPublic && (
        <button
          type="button"
          role="switch"
          aria-checked={configVisible}
          aria-label={ts(publishSettings.showConfig, language)}
          disabled={disabled}
          onClick={() => !disabled && onConfigVisibleChange(!configVisible)}
          className={`flex w-full items-center justify-between gap-3 rounded-md border p-3 text-left transition-colors focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-brand/50 disabled:cursor-not-allowed disabled:opacity-50 ${configVisible ? 'border-brand bg-brand-soft' : 'border-line bg-surface-2 hover:bg-surface-2'}`}
        >
          <span className="flex min-w-0 items-start gap-2">
            {configVisible ? (
              <Eye className="mt-0.5 h-4 w-4 shrink-0 text-brand" />
            ) : (
              <EyeOff className="mt-0.5 h-4 w-4 shrink-0 text-fg-3" />
            )}
            <span>
              <span className="block text-[13px] font-medium text-fg-2">
                {ts(publishSettings.showConfig, language)}
              </span>
              <span className="mt-1 block text-xs text-fg-3">
                {ts(publishSettings.showConfigDesc, language)}
              </span>
            </span>
          </span>
          <span className="flex shrink-0 flex-col items-end gap-2 sm:flex-row sm:items-center">
            <Badge variant={configVisible ? 'brand' : 'neutral'} size="xs">
              {configVisible
                ? ts(publishSettings.visible, language)
                : ts(publishSettings.hidden, language)}
            </Badge>
            <span
              aria-hidden="true"
              className={`relative h-5 w-9 rounded-full ${configVisible ? 'bg-brand' : 'bg-line-strong'}`}
            >
              <span
                className={`absolute top-0.5 h-4 w-4 rounded-full bg-surface transition-transform ${configVisible ? 'translate-x-[18px]' : 'translate-x-0.5'}`}
              />
            </span>
          </span>
        </button>
      )}
    </div>
  )
}

export default PublishSettingsEditor
