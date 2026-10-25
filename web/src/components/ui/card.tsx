import * as React from 'react'
import { cn } from '../../lib/cn'

const CardDensity = React.createContext(false)

export interface CardProps extends React.HTMLAttributes<HTMLDivElement> {
  /** 紧凑模式: 内边距 12px(默认 16px), 头部更矮 */
  dense?: boolean
}

export const Card = React.forwardRef<HTMLDivElement, CardProps>(
  ({ className, dense = false, ...props }, ref) => (
    <CardDensity.Provider value={dense}>
      <div
        ref={ref}
        className={cn(
          'rounded-lg border border-line bg-surface text-fg',
          className
        )}
        {...props}
      />
    </CardDensity.Provider>
  )
)
Card.displayName = 'Card'

export interface CardHeaderProps extends Omit<
  React.HTMLAttributes<HTMLDivElement>,
  'title'
> {
  title?: React.ReactNode
  subtitle?: React.ReactNode
  /** 右侧操作区(按钮 / Segmented / 链接等) */
  actions?: React.ReactNode
}

export function CardHeader({
  title,
  subtitle,
  actions,
  className,
  children,
  ...props
}: CardHeaderProps) {
  const dense = React.useContext(CardDensity)
  return (
    <div
      className={cn(
        'flex items-center justify-between gap-3 border-b border-line',
        dense ? 'min-h-9 px-3 py-1.5' : 'min-h-11 px-4 py-2',
        className
      )}
      {...props}
    >
      <div className="min-w-0">
        {title != null && (
          <div className="truncate text-sm font-semibold text-fg">{title}</div>
        )}
        {subtitle != null && (
          <div className="truncate text-xs text-fg-3">{subtitle}</div>
        )}
        {children}
      </div>
      {actions != null && (
        <div className="flex shrink-0 items-center gap-2">{actions}</div>
      )}
    </div>
  )
}

export function CardBody({
  className,
  ...props
}: React.HTMLAttributes<HTMLDivElement>) {
  const dense = React.useContext(CardDensity)
  return <div className={cn(dense ? 'p-3' : 'p-4', className)} {...props} />
}
