import { lazy, Suspense, useEffect, useState } from 'react'
import { Tabs } from '../components/ui'

const FactorMiningPage = lazy(() => import('./FactorMiningPage'))
const PaperPromotionPage = lazy(() => import('./PaperPromotionPage'))
const ResearchLogPage = lazy(() => import('./ResearchLogPage'))

export type ResearchTab = 'factor-mining' | 'paper-promotion' | 'research-log'

export const RESEARCH_TABS: ResearchTab[] = [
  'factor-mining',
  'paper-promotion',
  'research-log',
]

const DEFAULT_TAB: ResearchTab = 'factor-mining'

export function researchTabFromHash(hash: string): ResearchTab {
  const h = hash.replace(/^#/, '')
  return (RESEARCH_TABS as string[]).includes(h)
    ? (h as ResearchTab)
    : DEFAULT_TAB
}

export function researchTabLabel(tab: ResearchTab, language: string) {
  const zh = language === 'zh'
  switch (tab) {
    case 'factor-mining':
      return zh ? '因子挖掘' : 'Factor Mining'
    case 'paper-promotion':
      return zh ? '晋升报告' : 'Paper Promotion'
    default:
      return zh ? '研究记录' : 'Research Log'
  }
}

/** 研究中心：因子挖掘 / 晋升报告 / 研究记录，顶部导航悬停菜单直达各项，默认因子挖掘。 */
export default function ResearchHubPage({ language }: { language: string }) {
  const [tab, setTab] = useState<ResearchTab>(() =>
    researchTabFromHash(window.location.hash)
  )

  useEffect(() => {
    const sync = () => setTab(researchTabFromHash(window.location.hash))
    window.addEventListener('popstate', sync)
    window.addEventListener('hashchange', sync)
    return () => {
      window.removeEventListener('popstate', sync)
      window.removeEventListener('hashchange', sync)
    }
  }, [])

  return (
    <div className="mx-auto max-w-[1440px] px-4 pb-10 pt-6 sm:px-6">
      <Tabs
        className="mb-2"
        items={RESEARCH_TABS.map((k) => ({
          key: k,
          label: researchTabLabel(k, language),
        }))}
        value={tab}
        onChange={(k) => {
          setTab(k)
          window.history.replaceState(null, '', `/research#${k}`)
        }}
      />
      <Suspense
        fallback={<div className="p-6 text-sm text-fg-3">Loading…</div>}
      >
        {tab === 'factor-mining' && <FactorMiningPage language={language} />}
        {tab === 'paper-promotion' && (
          <PaperPromotionPage language={language} />
        )}
        {tab === 'research-log' && <ResearchLogPage language={language} />}
      </Suspense>
    </div>
  )
}
