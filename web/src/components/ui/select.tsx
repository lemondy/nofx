import { useRef, useState, useLayoutEffect, useCallback } from 'react'
import { createPortal } from 'react-dom'
import { ChevronDown } from 'lucide-react'
import { cn } from '../../lib/cn'

// Minimum distance (px) between the open menu and the viewport edges.
const VIEWPORT_GAP = 8

export interface SelectOption {
  value: string | number
  label: string
}

interface NofxSelectProps {
  value: string | number
  onChange: (value: string) => void
  options: SelectOption[]
  disabled?: boolean
  className?: string
  style?: React.CSSProperties
  /** Accessible name / hover hint for the trigger */
  'aria-label'?: string
  title?: string
}

export function NofxSelect({
  value,
  onChange,
  options,
  disabled,
  className,
  style,
  'aria-label': ariaLabel,
  title,
}: NofxSelectProps) {
  const [open, setOpen] = useState(false)
  const triggerRef = useRef<HTMLDivElement>(null)
  const dropdownRef = useRef<HTMLDivElement>(null)
  const [pos, setPos] = useState({ top: 0, left: 0, width: 0 })
  const selected = options.find((o) => String(o.value) === String(value))

  const updatePos = useCallback(() => {
    if (!triggerRef.current) return
    const rect = triggerRef.current.getBoundingClientRect()
    // A trigger near the right edge would push a wider menu off-screen:
    // right-align the menu with the trigger instead, clamped to the viewport.
    const viewport = document.documentElement.clientWidth
    const menuWidth = dropdownRef.current?.offsetWidth ?? rect.width
    let left = rect.left
    if (left + menuWidth > viewport - VIEWPORT_GAP) {
      left = Math.max(
        VIEWPORT_GAP,
        Math.min(rect.right, viewport - VIEWPORT_GAP) - menuWidth
      )
    }
    setPos({ top: rect.bottom + 4, left, width: rect.width })
  }, [])

  useLayoutEffect(() => {
    if (!open) return
    updatePos()
    const handleClose = (e: MouseEvent) => {
      const target = e.target as Node
      if (triggerRef.current?.contains(target)) return
      if (dropdownRef.current?.contains(target)) return
      setOpen(false)
    }
    const handleScroll = (e: Event) => {
      if (dropdownRef.current?.contains(e.target as Node)) return
      setOpen(false)
    }
    document.addEventListener('mousedown', handleClose)
    window.addEventListener('scroll', handleScroll, true)
    return () => {
      document.removeEventListener('mousedown', handleClose)
      window.removeEventListener('scroll', handleScroll, true)
    }
  }, [open, updatePos])

  return (
    <div ref={triggerRef} className={cn('relative', className)} style={style}>
      <div
        role="combobox"
        aria-expanded={open}
        aria-label={ariaLabel}
        title={title}
        className={cn(
          'flex items-center justify-between gap-1.5 w-full h-full cursor-pointer',
          disabled && 'opacity-50 cursor-not-allowed'
        )}
        onClick={(e) => {
          e.stopPropagation()
          if (!disabled) setOpen(!open)
        }}
      >
        <span className="truncate">{selected?.label ?? String(value)}</span>
        <ChevronDown
          className={cn(
            'w-3 h-3 shrink-0 opacity-50 transition-transform',
            open && 'rotate-180'
          )}
        />
      </div>
      {open &&
        createPortal(
          <div
            ref={dropdownRef}
            className="fixed z-[9999] rounded border border-line bg-surface-2 shadow-pop max-h-60 overflow-y-auto"
            style={{
              top: pos.top,
              left: pos.left,
              minWidth: pos.width,
              maxWidth: `calc(100vw - ${VIEWPORT_GAP * 2}px)`,
            }}
          >
            {options.map((opt) => (
              <div
                key={opt.value}
                title={opt.label}
                className={cn(
                  'px-3 py-1.5 text-sm cursor-pointer transition-colors whitespace-nowrap truncate',
                  String(opt.value) === String(value)
                    ? 'bg-brand-soft text-brand'
                    : 'text-fg hover:bg-surface-hover'
                )}
                onClick={(e) => {
                  e.stopPropagation()
                  onChange(String(opt.value))
                  setOpen(false)
                }}
              >
                {opt.label}
              </div>
            ))}
          </div>,
          document.body
        )}
    </div>
  )
}
