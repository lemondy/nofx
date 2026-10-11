import * as React from 'react'
import { cn } from '../../lib/cn'

export interface TabItem<K extends string = string> {
  key: K
  label: React.ReactNode
  /** 右侧小计数 */
  count?: number | string
  icon?: React.ReactNode
  disabled?: boolean
  /** Accessible name, required for icon-only items */
  ariaLabel?: string
}

interface TabsBaseProps<K extends string> {
  items: TabItem<K>[]
  value: K
  onChange: (key: K) => void
  className?: string
}

/** 下划线式标签页(页面级 / 卡片级导航) */
export function Tabs<K extends string = string>({
  items,
  value,
  onChange,
  className,
}: TabsBaseProps<K>) {
  return (
    <div
      role="tablist"
      className={cn('flex items-center gap-4 border-b border-line', className)}
    >
      {items.map((it) => {
        const active = it.key === value
        return (
          <button
            key={it.key}
            role="tab"
            type="button"
            aria-selected={active}
            disabled={it.disabled}
            aria-label={it.ariaLabel}
            onClick={() => onChange(it.key)}
            className={cn(
              '-mb-px inline-flex h-9 items-center gap-1.5 whitespace-nowrap border-b-2 px-0.5 text-[13px] font-semibold transition-colors',
              active
                ? 'border-brand text-fg'
                : 'border-transparent text-fg-3 hover:text-fg'
            )}
          >
            {it.icon}
            {it.label}
            {it.count != null && (
              <span className="num rounded bg-surface-2 px-1 text-[11px] text-fg-3">
                {it.count}
              </span>
            )}
          </button>
        )
      })}
    </div>
  )
}

export interface SegmentedProps<
  K extends string = string,
> extends TabsBaseProps<K> {
  size?: 'sm' | 'md'
}

/** 分段选择(时间周期 / 视图切换) */
export function Segmented<K extends string = string>({
  items,
  value,
  onChange,
  className,
  size = 'md',
}: SegmentedProps<K>) {
  return (
    <div
      role="group"
      className={cn(
        'inline-flex items-center gap-0.5 rounded-md border border-line bg-surface-2 p-0.5',
        className
      )}
    >
      {items.map((it) => {
        const active = it.key === value
        return (
          <button
            key={it.key}
            type="button"
            aria-pressed={active}
            disabled={it.disabled}
            aria-label={it.ariaLabel}
            onClick={() => onChange(it.key)}
            className={cn(
              'inline-flex items-center gap-1 whitespace-nowrap rounded font-semibold transition-colors',
              size === 'sm' ? 'h-6 px-2 text-xs' : 'h-7 px-2.5 text-[13px]',
              active
                ? 'bg-brand-soft text-brand'
                : 'text-fg-3 hover:bg-surface-hover hover:text-fg'
            )}
          >
            {it.icon}
            {it.label}
          </button>
        )
      })}
    </div>
  )
}
