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
      {/* Publish toggle */}
      <div
        className={`relative overflow-hidden rounded-lg transition-all duration-300 ${disabled ? 'opacity-50 cursor-not-allowed' : 'cursor-pointer'}`}
        style={{
          background: isPublic
            ? 'linear-gradient(135deg, var(--up-soft) 0%, color-mix(in srgb, var(--up) 5%, transparent) 100%)'
            : 'linear-gradient(135deg, var(--surface-hover) 0%, var(--surface-2) 100%)',
          border: isPublic
            ? '1px solid color-mix(in srgb, var(--up) 40%, transparent)'
            : '1px solid var(--line)',
          boxShadow: isPublic
            ? '0 0 20px color-mix(in srgb, var(--up) 10%, transparent)'
            : 'none',
        }}
        onClick={() => !disabled && onIsPublicChange(!isPublic)}
      >
        {/* Top glow line */}
        <div
          className="absolute top-0 left-0 w-full h-[1px] transition-opacity duration-300"
          style={{
            background: isPublic
              ? 'linear-gradient(90deg, transparent, var(--up), transparent)'
              : 'linear-gradient(90deg, transparent, var(--line), transparent)',
            opacity: isPublic ? 1 : 0.5,
          }}
        />

        <div className="p-4 flex items-center justify-between">
          <div className="flex items-center gap-3">
            <div
              className="p-2.5 rounded-lg transition-all duration-300"
              style={{
                background: isPublic ? 'var(--up-soft)' : 'var(--surface-2)',
                border: isPublic
                  ? '1px solid color-mix(in srgb, var(--up) 30%, transparent)'
                  : '1px solid var(--line)',
              }}
            >
              {isPublic ? (
                <Globe className="w-5 h-5" style={{ color: 'var(--up)' }} />
              ) : (
                <Lock className="w-5 h-5" style={{ color: 'var(--fg-3)' }} />
              )}
            </div>
            <div>
              <div
                className="text-sm font-medium"
                style={{ color: 'var(--fg)' }}
              >
                {ts(publishSettings.publishToMarket, language)}
              </div>
              <div className="text-xs mt-0.5" style={{ color: 'var(--fg-3)' }}>
                {ts(publishSettings.publishDesc, language)}
              </div>
            </div>
          </div>

          {/* Toggle with status */}
          <div className="flex items-center gap-3">
            <span
              className="text-[10px] font-mono font-bold tracking-wider"
              style={{ color: isPublic ? 'var(--up)' : 'var(--fg-3)' }}
            >
              {isPublic
                ? ts(publishSettings.public, language)
                : ts(publishSettings.private, language)}
            </span>
            <div
              className="relative w-12 h-6 rounded-full transition-all duration-300"
              style={{
                background: isPublic
                  ? 'linear-gradient(90deg, var(--up), var(--up))'
                  : 'var(--line)',
                boxShadow: isPublic
                  ? '0 0 10px color-mix(in srgb, var(--up) 40%, transparent)'
                  : 'none',
              }}
            >
              <div
                className="absolute top-1 w-4 h-4 rounded-full transition-all duration-300"
                style={{
                  background: 'var(--fg)',
                  left: isPublic ? '28px' : '4px',
                  boxShadow: '0 2px 4px rgba(0,0,0,0.3)',
                }}
              />
            </div>
          </div>
        </div>
      </div>

      {/* Config visibility toggle - only shown when public */}
      {isPublic && (
        <div
          className={`relative overflow-hidden rounded-lg transition-all duration-300 ${disabled ? 'opacity-50 cursor-not-allowed' : 'cursor-pointer'}`}
          style={{
            background: configVisible
              ? 'linear-gradient(135deg, var(--ai-soft) 0%, color-mix(in srgb, var(--ai) 5%, transparent) 100%)'
              : 'linear-gradient(135deg, var(--surface-hover) 0%, var(--surface-2) 100%)',
            border: configVisible
              ? '1px solid color-mix(in srgb, var(--ai) 40%, transparent)'
              : '1px solid var(--line)',
            boxShadow: configVisible
              ? '0 0 20px color-mix(in srgb, var(--ai) 10%, transparent)'
              : 'none',
          }}
          onClick={() => !disabled && onConfigVisibleChange(!configVisible)}
        >
          {/* Top glow line */}
          <div
            className="absolute top-0 left-0 w-full h-[1px] transition-opacity duration-300"
            style={{
              background: configVisible
                ? 'linear-gradient(90deg, transparent, var(--ai), transparent)'
                : 'linear-gradient(90deg, transparent, var(--line), transparent)',
              opacity: configVisible ? 1 : 0.5,
            }}
          />

          <div className="p-4 flex items-center justify-between">
            <div className="flex items-center gap-3">
              <div
                className="p-2.5 rounded-lg transition-all duration-300"
                style={{
                  background: configVisible
                    ? 'var(--ai-soft)'
                    : 'var(--surface-2)',
                  border: configVisible
                    ? '1px solid color-mix(in srgb, var(--ai) 30%, transparent)'
                    : '1px solid var(--line)',
                }}
              >
                {configVisible ? (
                  <Eye className="w-5 h-5" style={{ color: 'var(--ai)' }} />
                ) : (
                  <EyeOff
                    className="w-5 h-5"
                    style={{ color: 'var(--fg-3)' }}
                  />
                )}
              </div>
              <div>
                <div
                  className="text-sm font-medium"
                  style={{ color: 'var(--fg)' }}
                >
                  {ts(publishSettings.showConfig, language)}
                </div>
                <div
                  className="text-xs mt-0.5"
                  style={{ color: 'var(--fg-3)' }}
                >
                  {ts(publishSettings.showConfigDesc, language)}
                </div>
              </div>
            </div>

            {/* Toggle with status */}
            <div className="flex items-center gap-3">
              <span
                className="text-[10px] font-mono font-bold tracking-wider"
                style={{ color: configVisible ? 'var(--ai)' : 'var(--fg-3)' }}
              >
                {configVisible
                  ? ts(publishSettings.visible, language)
                  : ts(publishSettings.hidden, language)}
              </span>
              <div
                className="relative w-12 h-6 rounded-full transition-all duration-300"
                style={{
                  background: configVisible
                    ? 'linear-gradient(90deg, var(--ai), var(--ai))'
                    : 'var(--line)',
                  boxShadow: configVisible
                    ? '0 0 10px color-mix(in srgb, var(--ai) 40%, transparent)'
                    : 'none',
                }}
              >
                <div
                  className="absolute top-1 w-4 h-4 rounded-full transition-all duration-300"
                  style={{
                    background: 'var(--fg)',
                    left: configVisible ? '28px' : '4px',
                    boxShadow: '0 2px 4px rgba(0,0,0,0.3)',
                  }}
                />
              </div>
            </div>
          </div>
        </div>
      )}
    </div>
  )
}

export default PublishSettingsEditor
