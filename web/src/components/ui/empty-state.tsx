import * as React from 'react'
import { Inbox } from 'lucide-react'
import { cn } from '../../lib/cn'

export interface EmptyStateProps {
  icon?: React.ReactNode
  title?: React.ReactNode
  description?: React.ReactNode
  action?: React.ReactNode
  className?: string
}

export function EmptyState({
  icon,
  title,
  description,
  action,
  className,
}: EmptyStateProps) {
  return (
    <div
      className={cn(
        'flex flex-col items-center justify-center gap-2 px-4 py-10 text-center',
        className
      )}
    >
      <div className="text-fg-disabled">
        {icon ?? <Inbox className="h-8 w-8" />}
      </div>
      {title != null && (
        <div className="text-sm font-semibold text-fg-2">{title}</div>
      )}
      {description != null && (
        <div className="max-w-sm text-xs text-fg-3">{description}</div>
      )}
      {action != null && <div className="mt-2">{action}</div>}
    </div>
  )
}
