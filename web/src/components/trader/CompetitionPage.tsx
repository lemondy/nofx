import { useState } from 'react'
import { Trophy } from 'lucide-react'
import useSWR from 'swr'
import { api } from '../../lib/api'
import type { CompetitionData } from '../../types'
import { ComparisonChart } from '../charts/ComparisonChart'
import { TraderConfigViewModal } from './TraderConfigViewModal'
import { getTraderColor } from '../../utils/traderColors'
import { useTheme } from '../../lib/theme'
import { useLanguage } from '../../contexts/LanguageContext'
import { t } from '../../i18n/translations'
import { PunkAvatar, getTraderAvatar } from '../common/PunkAvatar'
import { DeepVoidBackground } from '../common/DeepVoidBackground'

export function CompetitionPage() {
  const { language } = useLanguage()
  useTheme() // 主题切换时重渲染, 使 getTraderColor 取到对应调色板
  const [selectedTrader, setSelectedTrader] = useState<any>(null)
  const [isModalOpen, setIsModalOpen] = useState(false)

  const { data: competition } = useSWR<CompetitionData>(
    'competition',
    api.getCompetition,
    {
      refreshInterval: 15000, // 15秒刷新（竞赛数据不需要太频繁更新）
      revalidateOnFocus: false,
      dedupingInterval: 10000,
    }
  )

  const handleTraderClick = async (traderId: string) => {
    try {
      const traderConfig = await api.getTraderConfig(traderId)
      setSelectedTrader(traderConfig)
      setIsModalOpen(true)
    } catch (error) {
      console.error('Failed to fetch trader config:', error)
      // 对于未登录用户，不显示详细配置，这是正常行为
      // 竞赛页面主要用于查看排行榜和基本信息
    }
  }

  const closeModal = () => {
    setIsModalOpen(false)
    setSelectedTrader(null)
  }

  if (!competition) {
    return (
      <DeepVoidBackground className="py-8" disableAnimation>
        <div className="container mx-auto max-w-7xl px-4 md:px-8">
          <div className="space-y-6">
            <div className="animate-pulse bg-surface border border-nofx-line rounded-xl p-8 ">
              <div className="flex items-center justify-between mb-6">
                <div className="space-y-3 flex-1">
                  <div className="h-8 w-64 bg-fg/5 rounded"></div>
                  <div className="h-4 w-48 bg-fg/5 rounded"></div>
                </div>
                <div className="h-12 w-32 bg-fg/5 rounded"></div>
              </div>
            </div>
            <div className="bg-surface border border-nofx-line rounded-xl p-6 ">
              <div className="h-6 w-40 mb-4 bg-fg/5 rounded"></div>
              <div className="space-y-3">
                <div className="h-20 w-full bg-fg/5 rounded"></div>
                <div className="h-20 w-full bg-fg/5 rounded"></div>
              </div>
            </div>
          </div>
        </div>
      </DeepVoidBackground>
    )
  }

  // 如果有数据返回但没有交易员，显示空状态
  if (!competition.traders || competition.traders.length === 0) {
    return (
      <DeepVoidBackground className="py-8" disableAnimation>
        <div className="container mx-auto max-w-7xl px-4 md:px-8 space-y-8 animate-fade-in">
          {/* Competition Header - 精简版 */}
          <div className="flex flex-col md:flex-row items-start md:items-center justify-between gap-3 md:gap-0">
            <div className="flex items-center gap-3 md:gap-4">
              <div className="w-10 h-10 md:w-12 md:h-12 rounded-xl flex items-center justify-center bg-surface-hover border border-nofx-gold/30 shadow-[0_0_15px_color-mix(in_srgb,var(--brand)_20%,transparent)]">
                <Trophy className="w-6 h-6 md:w-7 md:h-7 text-nofx-gold" />
              </div>
              <div>
                <h1 className="text-xl md:text-2xl font-bold flex items-center gap-2 text-fg">
                  {t('aiCompetition', language)}
                  <span className="text-xs font-normal px-2 py-1 rounded bg-nofx-gold/10 text-nofx-gold border border-nofx-gold/20">
                    0 {t('traders', language)}
                  </span>
                </h1>
                <p className="text-xs text-fg-3">{t('liveBattle', language)}</p>
              </div>
            </div>
          </div>

          {/* Empty State */}
          <div className="bg-surface border border-nofx-line rounded-xl p-16 text-center ">
            <Trophy className="w-16 h-16 mx-auto mb-4 text-fg-3" />
            <h3 className="text-lg font-bold mb-2 text-fg">
              {t('noTraders', language)}
            </h3>
            <p className="text-sm text-fg-3">
              {t('createFirstTrader', language)}
            </p>
          </div>
        </div>
      </DeepVoidBackground>
    )
  }

  // 按收益率排序
  const sortedTraders = [...competition.traders].sort(
    (a, b) => b.total_pnl_pct - a.total_pnl_pct
  )

  // 找出领先者
  const leader = sortedTraders[0]

  return (
    <DeepVoidBackground className="py-8" disableAnimation>
      <div className="w-full px-4 md:px-8 space-y-8 animate-fade-in">
        {/* Competition Header - 精简版 */}
        <div className="flex flex-col md:flex-row items-start md:items-center justify-between gap-3 md:gap-0">
          <div className="flex items-center gap-3 md:gap-4">
            <div className="w-10 h-10 md:w-12 md:h-12 rounded-xl flex items-center justify-center bg-surface-hover border border-nofx-gold/30 shadow-[0_0_15px_color-mix(in_srgb,var(--brand)_20%,transparent)]">
              <Trophy className="w-6 h-6 md:w-7 md:h-7 text-nofx-gold" />
            </div>
            <div>
              <h1 className="text-xl md:text-2xl font-bold flex items-center gap-2 text-fg">
                {t('aiCompetition', language)}
                <span className="text-xs font-normal px-2 py-1 rounded bg-nofx-gold/10 text-nofx-gold border border-nofx-gold/20">
                  {competition.count} {t('traders', language)}
                </span>
              </h1>
              <p className="text-xs text-fg-3">{t('liveBattle', language)}</p>
            </div>
          </div>
          <div className="text-left md:text-right w-full md:w-auto">
            <div className="text-xs mb-1 text-fg-3">
              {t('leader', language)}
            </div>
            <div className="text-base md:text-lg font-bold text-nofx-gold">
              {leader?.trader_name}
            </div>
            <div
              className="text-sm font-semibold"
              style={{
                color:
                  (leader?.total_pnl ?? 0) >= 0 ? 'var(--up)' : 'var(--down)',
              }}
            >
              {(leader?.total_pnl ?? 0) >= 0 ? '+' : ''}
              {leader?.total_pnl_pct?.toFixed(2) || '0.00'}%
            </div>
          </div>
        </div>

        {/* Left/Right Split: Performance Chart + Leaderboard */}
        <div className="grid grid-cols-1 lg:grid-cols-2 gap-6">
          {/* Left: Performance Comparison Chart */}
          <div
            className="bg-surface border border-nofx-line rounded-xl p-4 md:p-6 animate-slide-in hover:border-line-strong transition-colors"
            style={{ animationDelay: '0.1s' }}
          >
            <div className="flex items-center justify-between mb-6">
              <h2 className="text-lg font-bold flex items-center gap-2 text-fg">
                {t('performanceComparison', language)}
              </h2>
              <div className="text-xs text-fg-3">
                {t('realTimePnL', language)}
              </div>
            </div>
            <ComparisonChart traders={sortedTraders.slice(0, 10)} />
          </div>

          {/* Right: Leaderboard */}
          <div
            className="bg-surface border border-nofx-line rounded-xl p-4 md:p-6 animate-slide-in hover:border-line-strong transition-colors"
            style={{ animationDelay: '0.1s' }}
          >
            <div className="flex items-center justify-between mb-6">
              <h2 className="text-lg font-bold flex items-center gap-2 text-fg">
                {t('leaderboard', language)}
              </h2>
              <div className="text-xs px-2 py-1 rounded bg-nofx-gold/10 text-nofx-gold border border-nofx-gold/20 shadow-[0_0_8px_color-mix(in_srgb,var(--brand)_10%,transparent)]">
                {t('live', language)}
              </div>
            </div>
            <div className="space-y-2">
              {sortedTraders.map((trader, index) => {
                const isLeader = index === 0
                const traderColor = getTraderColor(
                  sortedTraders,
                  trader.trader_id
                )

                return (
                  <div
                    key={trader.trader_id}
                    onClick={() => handleTraderClick(trader.trader_id)}
                    className="rounded p-3 transition-all duration-300 hover:translate-y-[-1px] cursor-pointer hover:shadow-lg"
                    style={{
                      background: isLeader
                        ? 'linear-gradient(135deg, var(--brand-soft) 0%, var(--surface-2) 100%)'
                        : 'var(--surface-2)',
                      border: `1px solid ${isLeader ? 'color-mix(in srgb, var(--brand) 40%, transparent)' : 'var(--line)'}`,
                      boxShadow: isLeader
                        ? '0 3px 15px color-mix(in srgb, var(--brand) 12%, transparent), 0 0 0 1px color-mix(in srgb, var(--brand) 15%, transparent)'
                        : '0 1px 4px rgba(30, 30, 26, 0.12)',
                    }}
                  >
                    <div className="flex flex-wrap items-center justify-between gap-2">
                      {/* Rank & Avatar & Name */}
                      <div className="flex items-center gap-3">
                        {/* Rank Badge */}
                        <div
                          className="w-6 h-6 rounded-full flex items-center justify-center text-xs font-bold"
                          style={{
                            background:
                              index === 0
                                ? 'linear-gradient(135deg, #D4A62A 0%, #F0CC5B 100%)'
                                : index === 1
                                  ? 'linear-gradient(135deg, #C0C0C0 0%, #E8E8E8 100%)'
                                  : index === 2
                                    ? 'linear-gradient(135deg, #CD7F32 0%, #E8A64C 100%)'
                                    : 'var(--surface-hover)',
                            color: index < 3 ? '#1A1405' : 'var(--fg)',
                          }}
                        >
                          {index + 1}
                        </div>
                        {/* Punk Avatar */}
                        <PunkAvatar
                          seed={getTraderAvatar(
                            trader.trader_id,
                            trader.trader_name
                          )}
                          size={36}
                          className="rounded-lg"
                        />
                        <div>
                          <div
                            className="font-bold text-sm"
                            style={{ color: 'var(--fg)' }}
                          >
                            {trader.trader_name}
                          </div>
                          <div
                            className="text-xs mono font-semibold"
                            style={{ color: traderColor }}
                          >
                            {trader.ai_model.toUpperCase()} +{' '}
                            {trader.exchange.toUpperCase()}
                          </div>
                        </div>
                      </div>

                      {/* Stats */}
                      <div className="flex flex-wrap items-center gap-3 md:gap-6">
                        {/* Total Equity */}
                        <div className="text-right min-w-[60px] md:min-w-[80px]">
                          <div
                            className="text-[10px] mb-0.5"
                            style={{ color: 'var(--fg-3)' }}
                          >
                            {t('equity', language)}
                          </div>
                          <div
                            className="text-sm md:text-base font-bold mono"
                            style={{ color: 'var(--fg)' }}
                          >
                            {trader.total_equity?.toFixed(2) || '0.00'}
                          </div>
                        </div>

                        {/* P&L */}
                        <div className="text-right min-w-[70px] md:min-w-[90px]">
                          <div
                            className="text-[10px] mb-0.5"
                            style={{ color: 'var(--fg-3)' }}
                          >
                            {t('pnl', language)}
                          </div>
                          <div
                            className="text-sm md:text-base font-bold mono"
                            style={{
                              color:
                                (trader.total_pnl ?? 0) >= 0
                                  ? 'var(--up)'
                                  : 'var(--down)',
                            }}
                          >
                            {(trader.total_pnl ?? 0) >= 0 ? '+' : ''}
                            {trader.total_pnl_pct?.toFixed(2) || '0.00'}%
                          </div>
                          <div
                            className="text-[10px] mono"
                            style={{ color: 'var(--fg-3)' }}
                          >
                            {(trader.total_pnl ?? 0) >= 0 ? '+' : ''}
                            {trader.total_pnl?.toFixed(2) || '0.00'}
                          </div>
                        </div>

                        {/* Positions */}
                        <div className="text-right min-w-[40px] md:min-w-[50px]">
                          <div
                            className="text-[10px] mb-0.5"
                            style={{ color: 'var(--fg-3)' }}
                          >
                            {t('pos', language)}
                          </div>
                          <div
                            className="text-sm md:text-base font-bold mono"
                            style={{ color: 'var(--fg)' }}
                          >
                            {trader.position_count}
                          </div>
                          <div
                            className="text-[10px]"
                            style={{ color: 'var(--fg-3)' }}
                          >
                            {trader.margin_used_pct.toFixed(1)}%
                          </div>
                        </div>

                        {/* Status */}
                        <div>
                          <div
                            className="px-2 py-1 rounded text-xs font-bold"
                            style={
                              trader.is_running
                                ? {
                                    background: 'var(--up-soft)',
                                    color: 'var(--up)',
                                  }
                                : {
                                    background: 'var(--down-soft)',
                                    color: 'var(--down)',
                                  }
                            }
                          >
                            {trader.is_running ? '●' : '○'}
                          </div>
                        </div>
                      </div>
                    </div>
                  </div>
                )
              })}
            </div>
          </div>
        </div>

        {/* Head-to-Head Stats */}
        {competition.traders.length === 2 && (
          <div
            className="bg-surface border border-nofx-line rounded-xl p-4 md:p-6 animate-slide-in"
            style={{ animationDelay: '0.3s' }}
          >
            <h2 className="text-lg font-bold mb-6 flex items-center gap-2 text-fg">
              {t('headToHead', language)}
            </h2>
            <div className="grid grid-cols-2 gap-4">
              {sortedTraders.map((trader, index) => {
                const isWinning = index === 0
                const opponent = sortedTraders[1 - index]

                // Check if both values are valid numbers
                const hasValidData =
                  trader.total_pnl_pct != null &&
                  opponent.total_pnl_pct != null &&
                  !isNaN(trader.total_pnl_pct) &&
                  !isNaN(opponent.total_pnl_pct)

                const gap = hasValidData
                  ? trader.total_pnl_pct - opponent.total_pnl_pct
                  : NaN

                return (
                  <div
                    key={trader.trader_id}
                    className="p-4 rounded transition-all duration-300 hover:scale-[1.02]"
                    style={
                      isWinning
                        ? {
                            background:
                              'linear-gradient(135deg, var(--up-soft) 0%, color-mix(in srgb, var(--up) 2%, transparent) 100%)',
                            border:
                              '2px solid color-mix(in srgb, var(--up) 30%, transparent)',
                            boxShadow:
                              '0 3px 15px color-mix(in srgb, var(--up) 12%, transparent)',
                          }
                        : {
                            background: 'var(--surface-2)',
                            border: '1px solid var(--line)',
                            boxShadow: '0 1px 4px rgba(30, 30, 26, 0.12)',
                          }
                    }
                  >
                    <div className="text-center">
                      {/* Avatar */}
                      <div className="flex justify-center mb-3">
                        <PunkAvatar
                          seed={getTraderAvatar(
                            trader.trader_id,
                            trader.trader_name
                          )}
                          size={56}
                          className="rounded-xl"
                        />
                      </div>
                      <div
                        className="text-sm md:text-base font-bold mb-2"
                        style={{
                          color: getTraderColor(
                            sortedTraders,
                            trader.trader_id
                          ),
                        }}
                      >
                        {trader.trader_name}
                      </div>
                      <div
                        className="text-lg md:text-2xl font-bold mono mb-1"
                        style={{
                          color:
                            (trader.total_pnl ?? 0) >= 0
                              ? 'var(--up)'
                              : 'var(--down)',
                        }}
                      >
                        {trader.total_pnl_pct != null &&
                        !isNaN(trader.total_pnl_pct)
                          ? `${trader.total_pnl_pct >= 0 ? '+' : ''}${trader.total_pnl_pct.toFixed(2)}%`
                          : '—'}
                      </div>
                      {hasValidData && isWinning && gap > 0 && (
                        <div
                          className="text-xs font-semibold"
                          style={{ color: 'var(--up)' }}
                        >
                          {t('leadingBy', language, { gap: gap.toFixed(2) })}
                        </div>
                      )}
                      {hasValidData && !isWinning && gap < 0 && (
                        <div
                          className="text-xs font-semibold"
                          style={{ color: 'var(--down)' }}
                        >
                          {t('behindBy', language, {
                            gap: Math.abs(gap).toFixed(2),
                          })}
                        </div>
                      )}
                      {!hasValidData && (
                        <div
                          className="text-xs font-semibold"
                          style={{ color: 'var(--fg-3)' }}
                        >
                          —
                        </div>
                      )}
                    </div>
                  </div>
                )
              })}
            </div>
          </div>
        )}

        {/* Trader Config View Modal */}
        <TraderConfigViewModal
          isOpen={isModalOpen}
          onClose={closeModal}
          traderData={selectedTrader}
        />
      </div>
    </DeepVoidBackground>
  )
}
