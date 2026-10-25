import * as React from 'react'
import { cn } from '../../lib/cn'
import { Change } from './pnl'

export interface StatProps {
  label: React.ReactNode
  value: React.ReactNode
  /** 涨跌副值(百分比); 自动着色 */
  delta?: number | null
  deltaSuffix?: string
  /** 数值本身着色 */
  tone?: 'default' | 'up' | 'down'
  size?: 'md' | 'lg'
  hint?: React.ReactNode
  className?: string
}

const TONE = { default: 'text-fg', up: 'text-up', down: 'text-down' } as const

export function Stat({
  label,
  value,
  delta,
  deltaSuffix = '%',
  tone = 'default',
  size = 'md',
  hint,
  className,
}: StatProps) {
  return (
    <div className={cn('min-w-0', className)}>
      <div className="truncate text-xs text-fg-3">{label}</div>
      <div className="mt-0.5 flex items-baseline gap-2">
        <span
          className={cn(
            'num truncate font-semibold',
            size === 'lg' ? 'text-2xl' : 'text-xl',
            TONE[tone]
          )}
        >
          {value}
        </span>
        {delta !== undefined && (
          <Change value={delta} suffix={deltaSuffix} className="text-xs" />
        )}
      </div>
      {hint != null && <div className="mt-0.5 text-xs text-fg-3">{hint}</div>}
    </div>
  )
}
