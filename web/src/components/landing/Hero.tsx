import { ArrowRight, Code2, Github, ShieldCheck, Swords } from 'lucide-react'
import { Badge, Button, Card, CardBody, buttonVariants } from '../ui'
import { t, type Language } from '../../i18n/translations'
import { useAuth } from '../../contexts/AuthContext'
import { useLanguage } from '../../contexts/LanguageContext'
import { OFFICIAL_LINKS } from '../../constants/branding'

const MARKETS = ['Crypto', 'US Stocks', 'Forex', 'Metals']

export default function Hero() {
  const { user } = useAuth()
  const { language } = useLanguage()
  const lang = language as Language

  const capabilities = [
    {
      icon: Code2,
      title: t('openSourceSelfHosted', lang),
      desc: t('openSourceDesc', lang),
    },
    {
      icon: Swords,
      title: t('multiAgentCompetition', lang),
      desc: t('multiAgentDesc', lang),
    },
    {
      icon: ShieldCheck,
      title: t('secureReliableTrading', lang),
      desc: t('secureDesc', lang),
    },
  ]

  const handleGetStarted = () => {
    window.location.href = user ? '/dashboard' : '/login'
  }

  return (
    <section className="mx-auto w-full max-w-[1200px] px-4 pb-16 pt-14 sm:px-6 md:pt-20">
      <div className="mx-auto max-w-3xl text-center">
        <h1 className="text-4xl font-bold leading-[1.08] tracking-tight text-fg sm:text-5xl md:text-6xl">
          {t('heroTitle1', lang)}
          <br />
          <span className="text-brand">{t('heroTitle2', lang)}</span>
        </h1>
        <p className="mx-auto mt-6 max-w-2xl text-[15px] leading-relaxed text-fg-2">
          {t('heroDescription', lang)}
        </p>

        <div className="mt-8 flex flex-col items-center justify-center gap-3 sm:flex-row">
          <Button variant="primary" size="lg" onClick={handleGetStarted}>
            {t('getStartedNow', lang)}
            <ArrowRight className="h-4 w-4" />
          </Button>
          <a
            href={OFFICIAL_LINKS.github}
            target="_blank"
            rel="noopener noreferrer"
            className={buttonVariants({ variant: 'secondary', size: 'lg' })}
          >
            <Github className="h-4 w-4" />
            {t('viewSourceCode', lang)}
          </a>
        </div>

        <div className="mt-8 flex flex-wrap items-center justify-center gap-2">
          {MARKETS.map((market) => (
            <Badge key={market} variant="neutral">
              {market}
            </Badge>
          ))}
        </div>
      </div>

      {/* Capability cards */}
      <div id="features" className="mt-16 scroll-mt-[68px]">
        <div className="mb-4 flex items-baseline justify-between">
          <h2 className="text-xl font-semibold text-fg">
            {t('coreFeatures', lang)}
          </h2>
          <span className="text-[13px] text-fg-3">
            {t('whyChooseNofx', lang)}
          </span>
        </div>
        <div className="grid gap-4 md:grid-cols-3">
          {capabilities.map((cap) => {
            const Icon = cap.icon
            return (
              <Card key={cap.title}>
                <CardBody className="space-y-3">
                  <div className="flex h-9 w-9 items-center justify-center rounded-md border border-line bg-surface-2 text-brand">
                    <Icon size={18} />
                  </div>
                  <div>
                    <h3 className="text-sm font-semibold text-fg">
                      {cap.title}
                    </h3>
                    <p className="mt-1.5 text-[13px] leading-relaxed text-fg-3">
                      {cap.desc}
                    </p>
                  </div>
                </CardBody>
              </Card>
            )
          })}
        </div>
      </div>
    </section>
  )
}
