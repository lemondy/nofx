import { useEffect, useMemo, useRef, useState } from 'react'
import { createPortal } from 'react-dom'
import { t, type Language } from '../../i18n/translations'
import { toast } from 'sonner'
import { Lock } from 'lucide-react'
import { Button } from '../ui'
import { WebCryptoEnvironmentCheck } from '../common/WebCryptoEnvironmentCheck'

const DEFAULT_LENGTH = 64

function generateObfuscation(): string {
  const bytes = new Uint8Array(32)
  crypto.getRandomValues(bytes)
  return Array.from(bytes, (byte) => byte.toString(16).padStart(2, '0')).join(
    ''
  )
}

function validatePrivateKeyFormat(
  value: string,
  expectedLength: number
): boolean {
  const normalized = value.startsWith('0x') ? value.slice(2) : value
  if (normalized.length !== expectedLength) {
    return false
  }
  return /^[0-9a-fA-F]+$/.test(normalized)
}

export interface TwoStageKeyModalResult {
  value: string
  obfuscationLog: string[]
}

interface TwoStageKeyModalProps {
  isOpen: boolean
  language: Language
  onCancel: () => void
  onComplete: (result: TwoStageKeyModalResult) => void
  expectedLength?: number
  contextLabel?: string
}

export function TwoStageKeyModal({
  isOpen,
  language,
  onCancel,
  onComplete,
  expectedLength = DEFAULT_LENGTH,
  contextLabel,
}: TwoStageKeyModalProps) {
  const [stage, setStage] = useState<1 | 2>(1)
  const [part1, setPart1] = useState('')
  const [part2, setPart2] = useState('')
  const [error, setError] = useState<string | null>(null)
  const [clipboardStatus, setClipboardStatus] = useState<
    'idle' | 'copied' | 'failed'
  >('idle')
  const [obfuscationLog, setObfuscationLog] = useState<string[]>([])
  const [processing, setProcessing] = useState(false)
  const [manualObfuscationValue, setManualObfuscationValue] = useState<
    string | null
  >(null)

  const stage1Ref = useRef<HTMLInputElement>(null)
  const stage2Ref = useRef<HTMLInputElement>(null)

  // UX improvement: Use 58 + 6 split (most of the key + last 6 chars)
  // Advantage: Second stage only requires entering 6 characters, much easier to count
  const expectedPart1Length = expectedLength - 6 // 64 - 6 = 58
  const expectedPart2Length = 6 // Last 6 characters

  useEffect(() => {
    if (isOpen && stage === 1 && stage1Ref.current) {
      stage1Ref.current.focus()
    } else if (isOpen && stage === 2 && stage2Ref.current) {
      stage2Ref.current.focus()
    }
  }, [isOpen, stage])

  const handleStage1Next = async () => {
    // ✅ Normalize input (remove possible 0x prefix) before validating length
    const normalized1 = part1.startsWith('0x') ? part1.slice(2) : part1
    if (normalized1.length < expectedPart1Length) {
      setError(
        t('errors.privatekeyIncomplete', language, {
          expected: expectedPart1Length,
        })
      )
      return
    }

    setError(null)
    setProcessing(true)

    try {
      // 生成混淆字符串
      const obfuscation = generateObfuscation()
      setManualObfuscationValue(obfuscation)

      // 尝试复制到剪贴板
      if (navigator.clipboard) {
        try {
          await navigator.clipboard.writeText(obfuscation)
          setClipboardStatus('copied')
          setObfuscationLog([
            ...obfuscationLog,
            `Stage 1: ${new Date().toISOString()} - Auto copied obfuscation`,
          ])
          toast.success('已复制混淆字符串到剪贴板')
        } catch {
          setClipboardStatus('failed')
          setObfuscationLog([
            ...obfuscationLog,
            `Stage 1: ${new Date().toISOString()} - Auto copy failed, manual required`,
          ])
          toast.error('复制失败，请手动复制混淆字符串')
        }
      } else {
        setClipboardStatus('failed')
        setObfuscationLog([
          ...obfuscationLog,
          `Stage 1: ${new Date().toISOString()} - Clipboard API not available`,
        ])
        toast('当前浏览器不支持自动复制，请手动复制')
      }

      setTimeout(() => {
        setStage(2)
        setProcessing(false)
      }, 2000)
    } catch (err) {
      setError(t('errors.privatekeyObfuscationFailed', language))
      setProcessing(false)
    }
  }

  const handleStage2Complete = () => {
    // ✅ Normalize input (remove possible 0x prefix) before validating length
    const normalized2 = part2.startsWith('0x') ? part2.slice(2) : part2
    if (normalized2.length < expectedPart2Length) {
      setError(
        t('errors.privatekeyIncomplete', language, {
          expected: expectedPart2Length,
        })
      )
      return
    }

    // ✅ Concatenate after removing 0x prefix from both parts
    const normalized1 = part1.startsWith('0x') ? part1.slice(2) : part1
    const fullKey = normalized1 + normalized2
    if (!validatePrivateKeyFormat(fullKey, expectedLength)) {
      setError(t('errors.privatekeyInvalidFormat', language))
      return
    }

    const finalLog = [
      ...obfuscationLog,
      `Stage 2: ${new Date().toISOString()} - Completed`,
    ]
    onComplete({
      value: fullKey,
      obfuscationLog: finalLog,
    })
  }

  const handleReset = () => {
    setStage(1)
    setPart1('')
    setPart2('')
    setError(null)
    setClipboardStatus('idle')
    setObfuscationLog([])
    setProcessing(false)
    setManualObfuscationValue(null)
  }

  const modalContent = useMemo(() => {
    if (!isOpen) return null

    return (
      <div className="fixed inset-0 z-50 flex items-center justify-center bg-[var(--overlay)]">
        <div className="mx-4 w-full max-w-lg rounded-lg border border-line bg-surface p-5 shadow-[var(--shadow)]">
          <div className="mb-4 text-center">
            <h2 className="mb-1 flex items-center justify-center gap-2 text-base font-semibold text-fg">
              <Lock size={16} className="text-brand" />
              {t('twoStageKey.title', language)}
              {contextLabel && (
                <span className="ml-1 text-[13px] font-normal text-fg-3">
                  ({contextLabel})
                </span>
              )}
            </h2>
            <p className="text-[13px] text-fg-3">
              {stage === 1
                ? t('twoStageKey.stage1Description', language, {
                    length: expectedPart1Length,
                  })
                : t('twoStageKey.stage2Description', language, {
                    length: expectedPart2Length,
                  })}
            </p>
          </div>

          <div className="mb-4">
            <WebCryptoEnvironmentCheck language={language} variant="compact" />
          </div>

          {/* Stage 1 */}
          {stage === 1 && (
            <div className="space-y-3">
              <div>
                <label className="mb-1 block text-xs font-medium text-fg-3">
                  {t('twoStageKey.stage1InputLabel', language)} (
                  {expectedPart1Length} {t('twoStageKey.characters', language)})
                </label>
                <input
                  ref={stage1Ref}
                  type="password"
                  value={part1}
                  onChange={(e) => setPart1(e.target.value)}
                  placeholder="0x1234..."
                  className="num h-8 w-full rounded-md border border-line bg-surface-2 px-3 text-[13px] text-fg outline-none hover:border-line-strong focus:border-brand focus:ring-1 focus:ring-brand/40"
                  maxLength={expectedPart1Length + 2} // +2 for optional 0x prefix
                  disabled={processing}
                />
              </div>

              {error && <div className="text-[13px] text-down">{error}</div>}

              <div className="flex gap-2">
                <Button
                  variant="primary"
                  onClick={handleStage1Next}
                  disabled={
                    (part1.startsWith('0x') ? part1.slice(2) : part1).length <
                      expectedPart1Length || processing
                  }
                  className="flex-1"
                >
                  {processing
                    ? t('twoStageKey.processing', language)
                    : t('twoStageKey.nextButton', language)}
                </Button>
                <Button onClick={onCancel} disabled={processing}>
                  {t('twoStageKey.cancelButton', language)}
                </Button>
              </div>
            </div>
          )}

          {/* Transition Message */}
          {stage === 2 && clipboardStatus !== 'idle' && (
            <div className="mb-3 rounded-md border border-brand/30 bg-brand-soft p-3">
              {clipboardStatus === 'copied' && (
                <div className="text-fg">
                  <div className="font-medium">
                    {t('twoStageKey.obfuscationCopied', language)}
                  </div>
                  <div className="mt-1 text-[13px]">
                    {t('twoStageKey.obfuscationInstruction', language)}
                  </div>
                </div>
              )}
              {clipboardStatus === 'failed' && manualObfuscationValue && (
                <div className="text-brand">
                  <div className="font-medium">
                    {t('twoStageKey.obfuscationManual', language)}
                  </div>
                  <div className="num mt-2 break-all rounded-md border border-line bg-surface-2 p-2 text-xs">
                    {manualObfuscationValue}
                  </div>
                  <div className="mt-1 text-[13px]">
                    {t('twoStageKey.obfuscationInstruction', language)}
                  </div>
                </div>
              )}
            </div>
          )}

          {/* Stage 2 */}
          {stage === 2 && (
            <div className="space-y-3">
              <div>
                <label className="mb-1 block text-xs font-medium text-fg-3">
                  {t('twoStageKey.stage2InputLabel', language)} (
                  {expectedPart2Length} {t('twoStageKey.characters', language)})
                </label>
                <input
                  ref={stage2Ref}
                  type="password"
                  value={part2}
                  onChange={(e) => setPart2(e.target.value)}
                  placeholder="...5678"
                  className="num h-8 w-full rounded-md border border-line bg-surface-2 px-3 text-[13px] text-fg outline-none hover:border-line-strong focus:border-brand focus:ring-1 focus:ring-brand/40"
                  maxLength={expectedPart2Length + 2}
                />
              </div>

              {error && <div className="text-[13px] text-down">{error}</div>}

              <div className="flex gap-2">
                <Button
                  variant="up"
                  onClick={handleStage2Complete}
                  disabled={
                    (part2.startsWith('0x') ? part2.slice(2) : part2).length <
                    expectedPart2Length
                  }
                  className="flex-1"
                >
                  <Lock size={14} />
                  {t('twoStageKey.encryptButton', language)}
                </Button>
                <Button onClick={handleReset}>
                  {t('twoStageKey.backButton', language)}
                </Button>
              </div>
            </div>
          )}
        </div>
      </div>
    )
  }, [
    isOpen,
    stage,
    part1,
    part2,
    error,
    processing,
    clipboardStatus,
    manualObfuscationValue,
    language,
    expectedPart1Length,
    expectedPart2Length,
    contextLabel,
    obfuscationLog,
    onCancel,
    onComplete,
  ])

  if (!isOpen) return null

  return createPortal(modalContent, document.body)
}
