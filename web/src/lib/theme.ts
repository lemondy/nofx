// 主题切换: <html data-theme="dark|light">, 偏好存 localStorage(读写均 try/catch), 默认 dark。
import { useSyncExternalStore } from 'react'

export type Theme = 'dark' | 'light'

export const THEME_STORAGE_KEY = 'nofx-theme'
export const THEME_CHANGE_EVENT = 'nofx-theme-change'
export const DEFAULT_THEME: Theme = 'dark'

function normalize(v: unknown): Theme {
  return v === 'light' ? 'light' : 'dark'
}

export function getTheme(): Theme {
  try {
    const stored = localStorage.getItem(THEME_STORAGE_KEY)
    if (stored === 'light' || stored === 'dark') return stored
  } catch {
    /* storage 不可用时回落到默认 */
  }
  return DEFAULT_THEME
}

export function applyTheme(theme: Theme): void {
  if (typeof document === 'undefined') return
  document.documentElement.setAttribute('data-theme', theme)
}

/** 以 <html data-theme> 为准(首屏脚本已写入), 缺失时读存储。 */
export function currentTheme(): Theme {
  if (typeof document !== 'undefined') {
    const attr = document.documentElement.getAttribute('data-theme')
    if (attr === 'light' || attr === 'dark') return attr
  }
  return getTheme()
}

export function setTheme(theme: Theme): void {
  const next = normalize(theme)
  try {
    localStorage.setItem(THEME_STORAGE_KEY, next)
  } catch {
    /* ignore */
  }
  applyTheme(next)
  if (typeof window !== 'undefined') {
    window.dispatchEvent(new CustomEvent(THEME_CHANGE_EVENT, { detail: next }))
  }
}

export function toggleTheme(): Theme {
  const next: Theme = currentTheme() === 'dark' ? 'light' : 'dark'
  setTheme(next)
  return next
}

/** 首屏前调用(main.tsx 顶部); index.html 的内联脚本做同样的事以避免闪烁。 */
export function initTheme(): Theme {
  const theme = getTheme()
  applyTheme(theme)
  return theme
}

function subscribe(cb: () => void) {
  window.addEventListener(THEME_CHANGE_EVENT, cb)
  window.addEventListener('storage', cb)
  return () => {
    window.removeEventListener(THEME_CHANGE_EVENT, cb)
    window.removeEventListener('storage', cb)
  }
}

export function useTheme(): Theme {
  return useSyncExternalStore(subscribe, currentTheme, () => DEFAULT_THEME)
}
