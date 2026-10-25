import { cn } from '../../lib/cn'

const MINUS = '−' // 规范: 负号用 −(U+2212)

/** 数值格式化: 带符号、千分位; 四舍五入后为零时不带符号。 */
export function formatSigned(
  value: number,
  opts: {
    decimals?: number
    prefix?: string
    suffix?: string
  } = {}
): string {
  const { decimals = 2, prefix = '', suffix = '' } = opts
  const abs = Math.abs(value).toLocaleString('en-US', {
    minimumFractionDigits: decimals,
    maximumFractionDigits: decimals,
  })
  const isZero = Number(abs.replace(/,/g, '')) === 0
  const sign = isZero ? '' : value > 0 ? '+' : MINUS
  return `${sign}${prefix}${abs}${suffix}`
}

export function signTone(value: number, decimals = 2): 'up' | 'down' | 'zero' {
  const rounded = Number(value.toFixed(decimals))
  if (rounded > 0) return 'up'
  if (rounded < 0) return 'down'
  return 'zero'
}

const TONE_CLASS = {
  up: 'text-up',
  down: 'text-down',
  zero: 'text-fg-2',
} as const

interface SignedProps {
  value: number | null | undefined
  decimals?: number
  className?: string
}

function Signed({
  value,
  decimals = 2,
  className,
  prefix,
  suffix,
}: SignedProps & { prefix?: string; suffix?: string }) {
  if (value == null || !Number.isFinite(value)) {
    return <span className={cn('num text-fg-3', className)}>—</span>
  }
  const tone = signTone(value, decimals)
  return (
    <span className={cn('num', TONE_CLASS[tone], className)} data-tone={tone}>
      {formatSigned(value, { decimals, prefix, suffix })}
    </span>
  )
}

/** 涨跌幅, 默认百分比: +1.23% / −1.23% / 0.00% */
export function Change({
  suffix = '%',
  ...props
}: SignedProps & { suffix?: string }) {
  return <Signed suffix={suffix} {...props} />
}

/** 盈亏金额: +$1,234.50 / −$80.00 / $0.00 */
export function PnL({
  currency = '$',
  ...props
}: SignedProps & { currency?: string }) {
  return <Signed prefix={currency} {...props} />
}
