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

export function ModelCard({ model, selected, onClick, configured }: ModelCardProps) {
 return (
 <button
 type="button"
 onClick={onClick}
 className="flex flex-col items-center gap-2 p-4 rounded-xl transition-all hover:scale-105"
 style={{
 background: selected ? 'rgba(139, 92, 246, 0.15)' : '#F2EFE6',
 border: selected ? '2px solid #8B5CF6' : '2px solid #C0B9A2',
 }}
 >
 <div className="relative">
 <div className="w-12 h-12 rounded-xl flex items-center justify-center bg-[#F2EFE6] border border-nofx-line">
 {getModelIcon(model.provider || model.id, { width: 32, height: 32 }) || (
 <span className="text-lg font-bold" style={{ color: '#A78BFA' }}>{model.name[0]}</span>
 )}
 </div>
 {selected && (
 <div
 className="absolute -top-1 -right-1 w-5 h-5 rounded-full flex items-center justify-center"
 style={{ background: '#2E7D4F' }}
 >
 <Check className="w-3 h-3 text-black" />
 </div>
 )}
 {configured && !selected && (
 <div
 className="absolute -top-1 -right-1 w-4 h-4 rounded-full flex items-center justify-center"
 style={{ background: '#B8912A' }}
 >
 <Check className="w-2.5 h-2.5 text-black" />
 </div>
 )}
 </div>
 <span className="text-sm font-semibold" style={{ color: '#1E1E1A' }}>
 {getShortName(model.name)}
 </span>
 <span
 className="text-[10px] px-2 py-0.5 rounded-full uppercase tracking-wide"
 style={{ background: 'rgba(139, 92, 246, 0.2)', color: '#A78BFA' }}
 >
 {model.provider}
 </span>
 </button>
 )
}
