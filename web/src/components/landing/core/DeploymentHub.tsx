import { useState } from 'react'
import { Check, Command, Copy, Server, Shield, Terminal } from 'lucide-react'
import { Button, Card, CardBody, CardHeader } from '../../ui'
import { t, type Language } from '../../../i18n/translations'
import { useLanguage } from '../../../contexts/LanguageContext'

export default function DeploymentHub() {
  const [copied, setCopied] = useState(false)
  const { language } = useLanguage()
  const lang = language as Language
  const installCmd =
    'curl -fsSL https://raw.githubusercontent.com/NoFxAiOS/nofx/main/install.sh | bash'

  const handleCopy = () => {
    navigator.clipboard.writeText(installCmd)
    setCopied(true)
    setTimeout(() => setCopied(false), 2000)
  }

  const highlights = [
    {
      icon: Command,
      label: 'One-Line Install',
      desc: 'No configuration needed',
    },
    {
      icon: Shield,
      label: 'Secure Core',
      desc: 'Sandboxed execution env',
    },
  ]

  return (
    <section
      id="install"
      className="scroll-mt-[68px] border-t border-line bg-surface-2"
    >
      <div className="mx-auto grid w-full max-w-[1200px] grid-cols-1 items-center gap-10 px-4 py-16 sm:px-6 lg:grid-cols-2">
        {/* Left Column: Context */}
        <div className="space-y-5">
          <div className="flex items-center gap-2 text-xs font-medium uppercase tracking-wider text-fg-3">
            <Server size={14} /> System Deployment
          </div>

          <h2 className="text-2xl font-semibold leading-tight text-fg md:text-3xl">
            Deploy instantly, self-hosted
          </h2>

          <p className="max-w-xl text-[15px] leading-relaxed text-fg-2">
            Initialize your own trading node in seconds. The installer handles
            all dependencies and brings your autonomous agents online with a
            single command.
          </p>

          <div className="grid grid-cols-1 gap-3 pt-2 sm:grid-cols-2">
            {highlights.map((item) => (
              <div
                key={item.label}
                className="flex items-start gap-3 rounded-lg border border-line bg-surface p-3"
              >
                <div className="flex h-8 w-8 shrink-0 items-center justify-center rounded-md bg-surface-2 text-brand">
                  <item.icon size={16} />
                </div>
                <div>
                  <h4 className="text-[13px] font-semibold text-fg">
                    {item.label}
                  </h4>
                  <p className="mt-0.5 text-xs text-fg-3">{item.desc}</p>
                </div>
              </div>
            ))}
          </div>
        </div>

        {/* Right Column: Install Command */}
        <Card>
          <CardHeader
            title="install.sh"
            subtitle="bash"
            actions={<Terminal size={14} className="text-fg-3" />}
          />
          <CardBody>
            <div className="flex items-start gap-3 rounded-md border border-line bg-surface-2 p-3">
              <span className="num select-none pt-px text-[13px] text-fg-3">
                $
              </span>
              <code className="num flex-1 break-all text-[13px] text-fg">
                {installCmd}
              </code>
              <Button
                variant={copied ? 'up' : 'secondary'}
                size="sm"
                onClick={handleCopy}
                className="shrink-0"
                aria-label={t('copy', lang)}
              >
                {copied ? <Check size={14} /> : <Copy size={14} />}
                {copied ? 'OK' : t('copy', lang)}
              </Button>
            </div>
          </CardBody>
        </Card>
      </div>
    </section>
  )
}
