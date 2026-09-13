import { Globe, Lock, Eye, EyeOff } from 'lucide-react'
import { publishSettings, ts } from '../../i18n/strategy-translations'

interface PublishSettingsEditorProps {
 isPublic: boolean
 configVisible: boolean
 onIsPublicChange: (value: boolean) => void
 onConfigVisibleChange: (value: boolean) => void
 disabled?: boolean
 language: string
}

export function PublishSettingsEditor({
 isPublic,
 configVisible,
 onIsPublicChange,
 onConfigVisibleChange,
 disabled = false,
 language,
}: PublishSettingsEditorProps) {
 return (
 <div className="space-y-3">
 {/* Publish toggle */}
 <div
 className={`relative overflow-hidden rounded-lg transition-all duration-300 ${disabled ? 'opacity-50 cursor-not-allowed' : 'cursor-pointer'}`}
 style={{
 background: isPublic
 ? 'linear-gradient(135deg, rgba(46, 125, 79, 0.15) 0%, rgba(46, 125, 79, 0.05) 100%)'
 : 'linear-gradient(135deg, #E9E4D6 0%, #F2EFE6 100%)',
 border: isPublic ? '1px solid rgba(46, 125, 79, 0.4)' : '1px solid #C0B9A2',
 boxShadow: isPublic ? '0 0 20px rgba(46, 125, 79, 0.1)' : 'none',
 }}
 onClick={() => !disabled && onIsPublicChange(!isPublic)}
 >
 {/* Top glow line */}
 <div
 className="absolute top-0 left-0 w-full h-[1px] transition-opacity duration-300"
 style={{
 background: isPublic
 ? 'linear-gradient(90deg, transparent, #2E7D4F, transparent)'
 : 'linear-gradient(90deg, transparent, #C0B9A2, transparent)',
 opacity: isPublic ? 1 : 0.5
 }}
 />

 <div className="p-4 flex items-center justify-between">
 <div className="flex items-center gap-3">
 <div
 className="p-2.5 rounded-lg transition-all duration-300"
 style={{
 background: isPublic ? 'rgba(46, 125, 79, 0.2)' : '#F2EFE6',
 border: isPublic ? '1px solid rgba(46, 125, 79, 0.3)' : '1px solid #C0B9A2'
 }}
 >
 {isPublic ? (
 <Globe className="w-5 h-5" style={{ color: '#2E7D4F' }} />
 ) : (
 <Lock className="w-5 h-5" style={{ color: '#6E6E60' }} />
 )}
 </div>
 <div>
 <div className="text-sm font-medium" style={{ color: '#1E1E1A' }}>
 {ts(publishSettings.publishToMarket, language)}
 </div>
 <div className="text-xs mt-0.5" style={{ color: '#6E6E60' }}>
 {ts(publishSettings.publishDesc, language)}
 </div>
 </div>
 </div>

 {/* Toggle with status */}
 <div className="flex items-center gap-3">
 <span
 className="text-[10px] font-mono font-bold tracking-wider"
 style={{ color: isPublic ? '#2E7D4F' : '#6E6E60' }}
 >
 {isPublic ? ts(publishSettings.public, language) : ts(publishSettings.private, language)}
 </span>
 <div
 className="relative w-12 h-6 rounded-full transition-all duration-300"
 style={{
 background: isPublic
 ? 'linear-gradient(90deg, #2E7D4F, #4ade80)'
 : '#C0B9A2',
 boxShadow: isPublic ? '0 0 10px rgba(46, 125, 79, 0.4)' : 'none'
 }}
 >
 <div
 className="absolute top-1 w-4 h-4 rounded-full transition-all duration-300"
 style={{
 background: '#1E1E1A',
 left: isPublic ? '28px' : '4px',
 boxShadow: '0 2px 4px rgba(0,0,0,0.3)'
 }}
 />
 </div>
 </div>
 </div>
 </div>

 {/* Config visibility toggle - only shown when public */}
 {isPublic && (
 <div
 className={`relative overflow-hidden rounded-lg transition-all duration-300 ${disabled ? 'opacity-50 cursor-not-allowed' : 'cursor-pointer'}`}
 style={{
 background: configVisible
 ? 'linear-gradient(135deg, rgba(168, 85, 247, 0.15) 0%, rgba(168, 85, 247, 0.05) 100%)'
 : 'linear-gradient(135deg, #E9E4D6 0%, #F2EFE6 100%)',
 border: configVisible ? '1px solid rgba(168, 85, 247, 0.4)' : '1px solid #C0B9A2',
 boxShadow: configVisible ? '0 0 20px rgba(168, 85, 247, 0.1)' : 'none',
 }}
 onClick={() => !disabled && onConfigVisibleChange(!configVisible)}
 >
 {/* Top glow line */}
 <div
 className="absolute top-0 left-0 w-full h-[1px] transition-opacity duration-300"
 style={{
 background: configVisible
 ? 'linear-gradient(90deg, transparent, #a855f7, transparent)'
 : 'linear-gradient(90deg, transparent, #C0B9A2, transparent)',
 opacity: configVisible ? 1 : 0.5
 }}
 />

 <div className="p-4 flex items-center justify-between">
 <div className="flex items-center gap-3">
 <div
 className="p-2.5 rounded-lg transition-all duration-300"
 style={{
 background: configVisible ? 'rgba(168, 85, 247, 0.2)' : '#F2EFE6',
 border: configVisible ? '1px solid rgba(168, 85, 247, 0.3)' : '1px solid #C0B9A2'
 }}
 >
 {configVisible ? (
 <Eye className="w-5 h-5" style={{ color: '#a855f7' }} />
 ) : (
 <EyeOff className="w-5 h-5" style={{ color: '#6E6E60' }} />
 )}
 </div>
 <div>
 <div className="text-sm font-medium" style={{ color: '#1E1E1A' }}>
 {ts(publishSettings.showConfig, language)}
 </div>
 <div className="text-xs mt-0.5" style={{ color: '#6E6E60' }}>
 {ts(publishSettings.showConfigDesc, language)}
 </div>
 </div>
 </div>

 {/* Toggle with status */}
 <div className="flex items-center gap-3">
 <span
 className="text-[10px] font-mono font-bold tracking-wider"
 style={{ color: configVisible ? '#a855f7' : '#6E6E60' }}
 >
 {configVisible ? ts(publishSettings.visible, language) : ts(publishSettings.hidden, language)}
 </span>
 <div
 className="relative w-12 h-6 rounded-full transition-all duration-300"
 style={{
 background: configVisible
 ? 'linear-gradient(90deg, #a855f7, #c084fc)'
 : '#C0B9A2',
 boxShadow: configVisible ? '0 0 10px rgba(168, 85, 247, 0.4)' : 'none'
 }}
 >
 <div
 className="absolute top-1 w-4 h-4 rounded-full transition-all duration-300"
 style={{
 background: '#1E1E1A',
 left: configVisible ? '28px' : '4px',
 boxShadow: '0 2px 4px rgba(0,0,0,0.3)'
 }}
 />
 </div>
 </div>
 </div>
 </div>
 )}
 </div>
 )
}

export default PublishSettingsEditor
