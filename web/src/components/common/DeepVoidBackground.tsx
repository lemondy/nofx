import React from 'react'

interface DeepVoidBackgroundProps extends React.HTMLAttributes<HTMLDivElement> {
 children?: React.ReactNode
 className?: string
 disableAnimation?: boolean
}

// 平涂米色背景:页面基色 + 顶部一层极淡金色晕染,不再叠加 CRT/网格/噪点
export function DeepVoidBackground({ children, className = '', disableAnimation: _disableAnimation = false, ...props }: DeepVoidBackgroundProps) {
 return (
 <div className={`relative w-full min-h-screen bg-nofx-bg text-nofx-text flex flex-col ${className}`} {...props}>
 <div className="absolute inset-0 pointer-events-none z-0 bg-[radial-gradient(circle_at_top,rgba(184,145,42,0.05),transparent_45%)]"></div>

 {/* Content Layer */}
 <div className="relative z-10 flex-1 flex flex-col h-full w-full">
 {children}
 </div>
 </div>
 )
}
