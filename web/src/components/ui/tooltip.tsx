import * as React from 'react'
import { cn } from '../../lib/cn'

export interface TooltipProps {
  content: React.ReactNode
  children: React.ReactNode
  side?: 'top' | 'bottom'
  className?: string
}

/** 纯 CSS 提示: 悬停 / 键盘聚焦时显示, 不依赖 JS 定位。 */
export function Tooltip({
  content,
  children,
  side = 'top',
  className,
}: TooltipProps) {
  return (
    <span className={cn('group relative inline-flex', className)}>
      {children}
      <span
        role="tooltip"
        className={cn(
          'pointer-events-none absolute left-1/2 z-50 w-max max-w-xs -translate-x-1/2 rounded-md border border-line bg-surface-2 px-2 py-1 text-xs font-normal text-fg opacity-0 shadow-pop transition-opacity',
          'group-hover:opacity-100 group-focus-within:opacity-100',
          side === 'top' ? 'bottom-full mb-1.5' : 'top-full mt-1.5'
        )}
      >
        {content}
      </span>
    </span>
  )
}
