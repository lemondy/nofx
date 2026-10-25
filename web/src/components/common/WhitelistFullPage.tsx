import { ShieldAlert, ArrowLeft, Twitter, Send } from 'lucide-react'
import { OFFICIAL_LINKS } from '../../constants/branding'
import { Button, Card, CardBody } from '../ui'

interface WhitelistFullPageProps {
  onBack?: () => void
}

export function WhitelistFullPage({ onBack }: WhitelistFullPageProps) {
  const handleBackToLogin = () => {
    if (onBack) {
      onBack()
    } else {
      window.location.href = '/login'
    }
  }

  return (
    <div className="flex min-h-screen items-center justify-center bg-bg px-4 text-fg">
      <Card className="w-full max-w-[400px]">
        <CardBody className="p-6 text-center">
          {/* Icon */}
          <div className="mx-auto mb-4 flex h-12 w-12 items-center justify-center rounded-full bg-down-soft">
            <ShieldAlert className="h-6 w-6 text-down" />
          </div>

          {/* Title */}
          <h1 className="text-lg font-semibold text-fg">Access restricted</h1>

          {/* Description */}
          <p className="mt-2 text-[13px] leading-relaxed text-fg-3">
            Your identifier is not on the active whitelist. Platform capacity
            limits have been reached for the current beta phase. Prioritized
            access is currently reserved for authorized operators only.
          </p>

          {/* Info Box */}
          <div className="mt-5 rounded-md border border-down/20 bg-down-soft p-3 text-left">
            <div className="flex items-start gap-2.5">
              <ShieldAlert className="mt-0.5 h-4 w-4 shrink-0 text-down" />
              <div>
                <h3 className="text-xs font-semibold text-down">
                  Batched access
                </h3>
                <p className="mt-0.5 text-xs leading-relaxed text-fg-3">
                  Access is rolled out in batches. If you believe this is an
                  error, please verify your credentials or contact system
                  administrators.
                </p>
              </div>
            </div>
          </div>

          {/* Action Buttons */}
          <div className="mt-5 space-y-2">
            <Button
              variant="secondary"
              className="w-full"
              onClick={handleBackToLogin}
            >
              <ArrowLeft className="h-3.5 w-3.5" />
              Back to login
            </Button>

            <div className="grid grid-cols-2 gap-2">
              <a
                href={OFFICIAL_LINKS.twitter}
                target="_blank"
                rel="noopener noreferrer"
                className="inline-flex h-7 items-center justify-center gap-1.5 rounded-md text-xs text-fg-3 transition-colors hover:bg-surface-hover hover:text-fg"
              >
                <Twitter className="h-3.5 w-3.5" />
                Updates
              </a>
              <a
                href={OFFICIAL_LINKS.telegram}
                target="_blank"
                rel="noopener noreferrer"
                className="inline-flex h-7 items-center justify-center gap-1.5 rounded-md text-xs text-fg-3 transition-colors hover:bg-surface-hover hover:text-fg"
              >
                <Send className="h-3.5 w-3.5" />
                Support
              </a>
            </div>
          </div>
        </CardBody>
      </Card>
    </div>
  )
}
