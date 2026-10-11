import { Check } from 'lucide-react'
import type { AIModel } from '../../types'
import { getModelIcon } from '../common/ModelIcons'
import { getShortName } from './model-constants'

interface ModelCardProps {
  model: AIModel
  selected: boolean
  onClick: () => void
  configured?: boolean
}

export function ModelCard({
  model,
  selected,
  onClick,
  configured,
}: ModelCardProps) {
  return (
    <button
      type="button"
      onClick={onClick}
      className="flex flex-col items-center gap-2 p-4 rounded-xl transition-all hover:scale-105"
      style={{
        background: selected ? 'var(--ai-soft)' : 'var(--surface-2)',
        border: selected ? '2px solid var(--ai)' : '2px solid var(--line)',
      }}
    >
      <div className="relative">
        <div className="w-12 h-12 rounded-xl flex items-center justify-center bg-surface-2 border border-nofx-line">
          {getModelIcon(model.provider || model.id, {
            width: 32,
            height: 32,
          }) || (
            <span className="text-lg font-bold" style={{ color: 'var(--ai)' }}>
              {model.name[0]}
            </span>
          )}
        </div>
        {selected && (
          <div
            className="absolute -top-1 -right-1 w-5 h-5 rounded-full flex items-center justify-center"
            style={{ background: 'var(--up)' }}
          >
            <Check className="w-3 h-3 text-brand-fg" />
          </div>
        )}
        {configured && !selected && (
          <div
            className="absolute -top-1 -right-1 w-4 h-4 rounded-full flex items-center justify-center"
            style={{ background: 'var(--brand)' }}
          >
            <Check className="w-2.5 h-2.5 text-brand-fg" />
          </div>
        )}
      </div>
      <span className="text-sm font-semibold" style={{ color: 'var(--fg)' }}>
        {getShortName(model.name)}
      </span>
      <span
        className="text-[10px] px-2 py-0.5 rounded-full uppercase tracking-wide"
        style={{ background: 'var(--ai-soft)', color: 'var(--ai)' }}
      >
        {model.provider}
      </span>
    </button>
  )
}
