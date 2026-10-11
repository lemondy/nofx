import { Github, Send, ExternalLink } from 'lucide-react'
import { t, Language } from '../../i18n/translations'
import { OFFICIAL_LINKS } from '../../constants/branding'

interface FooterSectionProps {
  language: Language
}

export default function FooterSection({ language }: FooterSectionProps) {
  const links = {
    social: [
      { name: 'GitHub', href: OFFICIAL_LINKS.github, icon: Github },
      {
        name: 'X (Twitter)',
        href: OFFICIAL_LINKS.twitter,
        icon: () => (
          <svg viewBox="0 0 24 24" className="h-4 w-4" fill="currentColor">
            <path d="M18.244 2.25h3.308l-7.227 8.26 8.502 11.24H16.17l-5.214-6.817L4.99 21.75H1.68l7.73-8.835L1.254 2.25H8.08l4.713 6.231zm-1.161 17.52h1.833L7.084 4.126H5.117z" />
          </svg>
        ),
      },
      { name: 'Telegram', href: OFFICIAL_LINKS.telegram, icon: Send },
    ],
    resources: [
      {
        name: language === 'zh' ? '文档' : 'Documentation',
        href: 'https://github.com/NoFxAiOS/nofx/blob/main/README.md',
      },
      { name: 'Issues', href: 'https://github.com/NoFxAiOS/nofx/issues' },
      { name: 'Pull Requests', href: 'https://github.com/NoFxAiOS/nofx/pulls' },
    ],
    supporters: [
      { name: 'Binance', href: 'https://www.binance.com/join?ref=NOFXENG' },
      { name: 'Bybit', href: 'https://partner.bybit.com/b/83856' },
      { name: 'OKX', href: 'https://www.okx.com/join/1865360' },
      {
        name: 'Bitget',
        href: 'https://www.bitget.com/referral/register?from=referral&clacCode=c8a43172',
      },
      { name: 'Gate.io', href: 'https://www.gatenode.xyz/share/VQBGUAxY' },
      { name: 'KuCoin', href: 'https://www.kucoin.com/r/broker/CXEV7XKK' },
      {
        name: 'Hyperliquid',
        href: 'https://app.hyperliquid.xyz/join/AITRADING',
      },
      {
        name: 'Aster DEX',
        href: 'https://www.asterdex.com/en/referral/fdfc0e',
      },
      { name: 'Lighter', href: 'https://app.lighter.xyz/?referral=68151432' },
    ],
  }

  return (
    <footer className="border-t border-line bg-surface-2">
      <div className="mx-auto max-w-6xl px-4 py-10 md:py-12">
        {/* Top Section */}
        <div className="mb-10 grid grid-cols-1 gap-8 md:grid-cols-4 md:gap-10">
          {/* Brand */}
          <div className="md:col-span-1">
            <div className="mb-4 flex items-center gap-2.5">
              <img src="/icons/nofx.svg" alt="NOFX Logo" className="h-7 w-7" />
              <span className="text-lg font-semibold text-fg">NOFX</span>
            </div>
            <p className="mb-5 text-[13px] text-fg-3">
              {t('futureStandardAI', language)}
            </p>
            {/* Social Icons */}
            <div className="flex items-center gap-2">
              {links.social.map((link) => (
                <a
                  key={link.name}
                  href={link.href}
                  target="_blank"
                  rel="noopener noreferrer"
                  className="flex h-8 w-8 items-center justify-center rounded-md bg-surface text-fg-3 transition-colors hover:bg-surface-hover hover:text-fg"
                  title={link.name}
                >
                  <link.icon className="h-4 w-4" />
                </a>
              ))}
            </div>
          </div>

          {/* Links */}
          <div>
            <h4 className="mb-4 text-sm font-semibold text-fg">
              {t('links', language)}
            </h4>
            <ul className="space-y-2.5">
              {links.social.map((link) => (
                <li key={link.name}>
                  <a
                    href={link.href}
                    target="_blank"
                    rel="noopener noreferrer"
                    className="text-[13px] text-fg-3 transition-colors hover:text-brand"
                  >
                    {link.name}
                  </a>
                </li>
              ))}
            </ul>
          </div>

          {/* Resources */}
          <div>
            <h4 className="mb-4 text-sm font-semibold text-fg">
              {t('resources', language)}
            </h4>
            <ul className="space-y-2.5">
              {links.resources.map((link) => (
                <li key={link.name}>
                  <a
                    href={link.href}
                    target="_blank"
                    rel="noopener noreferrer"
                    className="inline-flex items-center gap-1 text-[13px] text-fg-3 transition-colors hover:text-brand"
                  >
                    {link.name}
                    <ExternalLink className="h-3 w-3 opacity-50" />
                  </a>
                </li>
              ))}
            </ul>
          </div>

          {/* Supporters */}
          <div>
            <h4 className="mb-4 text-sm font-semibold text-fg">
              {t('supporters', language)}
            </h4>
            <div className="flex flex-wrap gap-2">
              {links.supporters.map((link) => (
                <a
                  key={link.name}
                  href={link.href}
                  target="_blank"
                  rel="noopener noreferrer"
                  className="rounded border border-line bg-surface px-2.5 py-1.5 text-xs text-fg-3 transition-colors hover:border-brand hover:text-brand"
                >
                  {link.name}
                </a>
              ))}
            </div>
          </div>
        </div>

        {/* Bottom Section */}
        <div className="border-t border-line pt-6 text-center text-xs text-fg-3">
          <p className="mb-2 text-fg-2">{t('footerTitle', language)}</p>
          <p>{t('footerWarning', language)}</p>
        </div>
      </div>
    </footer>
  )
}
