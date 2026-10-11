import type { TraderConfigData } from '../../types'
import { t } from '../../i18n/translations'
import { useLanguage } from '../../contexts/LanguageContext'
import { PunkAvatar, getTraderAvatar } from '../common/PunkAvatar'

// Extract the name part after the last underscore
function getShortName(fullName: string): string {
  const parts = fullName.split('_')
  return parts.length > 1 ? parts[parts.length - 1] : fullName
}

interface TraderConfigViewModalProps {
  isOpen: boolean
  onClose: () => void
  traderData?: TraderConfigData | null
}

export function TraderConfigViewModal({
  isOpen,
  onClose,
  traderData,
}: TraderConfigViewModalProps) {
  const { language } = useLanguage()
  if (!isOpen || !traderData) return null

  const InfoRow = ({
    label,
    value,
  }: {
    label: string
    value: string | number | boolean
  }) => (
    <div className="flex justify-between items-start py-2 border-b border-line last:border-b-0">
      <span className="text-sm text-fg-3 font-medium">{label}</span>
      <span className="text-sm text-fg font-mono text-right">
        {typeof value === 'boolean'
          ? value
            ? t('traderConfigView.yes', language)
            : t('traderConfigView.no', language)
          : value}
      </span>
    </div>
  )

  return (
    <div className="fixed inset-0 z-50 flex items-center justify-center bg-surface-2 bg-opacity-50 ">
      <div
        className="bg-surface-hover border border-line rounded-xl shadow-2xl max-w-2xl w-full mx-4 max-h-[90vh] overflow-y-auto"
        onClick={(e) => e.stopPropagation()}
      >
        {/* Header */}
        <div className="flex items-center justify-between p-6 border-b border-line bg-gradient-to-r from-surface-hover to-line">
          <div className="flex items-center gap-3">
            <PunkAvatar
              seed={getTraderAvatar(
                traderData.trader_id || '',
                traderData.trader_name
              )}
              size={48}
              className="rounded-lg"
            />
            <div>
              <h2 className="text-xl font-bold text-fg">
                {t('traderConfigView.traderConfig', language)}
              </h2>
              <p className="text-sm text-fg-3 mt-1">
                {t('traderConfigView.configInfo', language, {
                  name: traderData.trader_name,
                })}
              </p>
            </div>
          </div>
          <div className="flex items-center gap-2">
            {/* Running Status */}
            <div
              className="px-3 py-1 rounded-full text-xs font-bold flex items-center gap-1"
              style={
                traderData.is_running
                  ? { background: 'var(--up-soft)', color: 'var(--up)' }
                  : { background: 'var(--down-soft)', color: 'var(--down)' }
              }
            >
              <span>{traderData.is_running ? '●' : '○'}</span>
              {traderData.is_running
                ? t('traderConfigView.running', language)
                : t('traderConfigView.stopped', language)}
            </div>
            <button
              onClick={onClose}
              className="w-8 h-8 rounded-lg text-fg-3 hover:text-fg hover:bg-line transition-colors flex items-center justify-center"
            >
              ✕
            </button>
          </div>
        </div>

        {/* Content */}
        <div className="p-6 space-y-6">
          {/* Basic Info */}
          <div className="bg-surface-2 border border-line rounded-lg p-5">
            <h3 className="text-lg font-semibold text-fg mb-4 flex items-center gap-2">
              {'🤖 ' + t('traderConfigView.basicInfo', language)}
            </h3>
            <div className="space-y-3">
              <InfoRow
                label={t('traderConfigView.traderName', language)}
                value={traderData.trader_name}
              />
              <InfoRow
                label={t('traderConfigView.aiModel', language)}
                value={getShortName(traderData.ai_model).toUpperCase()}
              />
              <InfoRow
                label={t('traderConfigView.exchange', language)}
                value={getShortName(traderData.exchange_id).toUpperCase()}
              />
              <InfoRow
                label={t('traderConfigView.initialBalance', language)}
                value={`$${traderData.initial_balance.toLocaleString()}`}
              />
              <InfoRow
                label={t('traderConfigView.marginMode', language)}
                value={
                  traderData.is_cross_margin
                    ? t('traderConfigView.crossMargin', language)
                    : t('traderConfigView.isolatedMargin', language)
                }
              />
              <InfoRow
                label={t('traderConfigView.scanIntervalLabel', language)}
                value={t('traderConfigView.scanInterval', language, {
                  minutes: traderData.scan_interval_minutes || 3,
                })}
              />
            </div>
          </div>

          {/* Strategy Info - only show if strategy is bound */}
          {traderData.strategy_id && (
            <div className="bg-surface-2 border border-line rounded-lg p-5">
              <h3 className="text-lg font-semibold text-fg mb-4 flex items-center gap-2">
                {'📋 ' + t('traderConfigView.strategyUsed', language)}
              </h3>
              <div className="space-y-3">
                <InfoRow
                  label={t('traderConfigView.strategyName', language)}
                  value={traderData.strategy_name || traderData.strategy_id}
                />
              </div>
            </div>
          )}
        </div>

        {/* Footer */}
        <div className="flex justify-end p-6 border-t border-line bg-gradient-to-r from-surface-hover to-line">
          <button
            onClick={onClose}
            className="px-6 py-3 bg-line text-fg rounded-lg hover:bg-line transition-all duration-200 border border-line-strong"
          >
            {t('traderConfigView.close', language)}
          </button>
        </div>
      </div>
    </div>
  )
}
