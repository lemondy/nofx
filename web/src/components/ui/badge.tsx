import * as React from 'react'
import { cva, type VariantProps } from 'class-variance-authority'
import { cn } from '../../lib/cn'

export const badgeVariants = cva(
  'inline-flex items-center gap-1 whitespace-nowrap rounded font-medium',
  {
    variants: {
      variant: {
        neutral: 'bg-surface-2 text-fg-2',
        brand: 'bg-brand-soft text-brand',
        up: 'bg-up-soft text-up',
        down: 'bg-down-soft text-down',
        warn: 'bg-warn-soft text-warn',
        info: 'bg-info-soft text-info',
        ai: 'bg-ai-soft text-ai',
      },
      size: {
        xs: 'h-[18px] px-1.5 text-[11px]',
        sm: 'h-5 px-2 text-xs',
      },
    },
    defaultVariants: { variant: 'neutral', size: 'sm' },
  }
)

export interface BadgeProps
  extends
    React.HTMLAttributes<HTMLSpanElement>,
    VariantProps<typeof badgeVariants> {}

export function Badge({ className, variant, size, ...props }: BadgeProps) {
  return (
    <span
      className={cn(badgeVariants({ variant, size }), className)}
      {...props}
    />
  )
}
