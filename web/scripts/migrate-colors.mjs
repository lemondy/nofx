#!/usr/bin/env node
/* global process, console */
/**
 * 颜色迁移脚本: 把 web/src 下写死的 hex / rgba 颜色改写为设计变量 var(--x)。
 * 规范: docs/design/TRADING_UI_DESIGN_SYSTEM.md
 *
 * 用法:
 *   node scripts/migrate-colors.mjs            # 直接改写文件并打印映射统计
 *   node scripts/migrate-colors.mjs --dry      # 只统计, 不写文件
 *
 * 设计要点
 *  - 映射表是显式的(HEX / RGB 两张表), 对"同一个值在不同角色下含义不同"的中性色
 *    (文字 / 背景 / 边框)按 CSS 属性名或 Tailwind 前缀分角色映射。
 *  - Tailwind 任意值类 bg-[#xxx] / text-[#xxx] / border-[#xxx] / from-[#xxx] ... 改写为语义类
 *    (bg-surface-2 / text-fg-3 / border-line, 保留 /透明度 后缀)。
 *  - rgba(r,g,b,a): 调色板色 -> 对应变量; 背景角色且 a∈[0.08,0.2] 用 -soft 变量,
 *    其余用 color-mix(in srgb, var(--x) N%, transparent)。
 *  - ALLOW_VALUES / ALLOW_FILES 是有意保留的第三方品牌色、数据色板、头像素材等。
 *  - 画布类图表(lightweight-charts)的选项区(组件 return 之前)不转换 —— canvas 不认识 var(),
 *    由 src/lib/chartTheme.ts 在运行时读取变量。见 CANVAS_ZONE。
 */
import fs from 'node:fs'
import path from 'node:path'
import { fileURLToPath } from 'node:url'

const ROOT = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '../src')
const DRY = process.argv.includes('--dry')

// ---------------------------------------------------------------- 允许保留
// 整个文件保留(头像素材 / 第三方品牌图标 / 数据系列调色板)
const ALLOW_FILES = new Set([
  'components/common/PunkAvatar.tsx',
  'components/common/ModelIcons.tsx',
  'utils/traderColors.ts',
  'lib/chartTheme.ts', // 运行时读取变量时的回退色值
])
// 按值保留(品牌色 / 数据色板 / 品类强调色)
const ALLOW_VALUES = {
  // 社交 / 交易所 / 链 品牌色
  '#2aabee': 'Telegram',
  '#0088cc': 'Telegram',
  '#1da1f2': 'Twitter/X',
  '#50e3c2': 'Hyperliquid',
  '#7fe7cc': 'Hyperliquid',
  '#f3ba2f': 'Binance',
  '#f7931a': 'BTC',
  '#627eea': 'ETH',
  // 奖牌
  '#c0c0c0': 'medal silver',
  '#e8e8e8': 'medal silver',
  '#cd7f32': 'medal bronze',
  // macOS 窗口三色点(装饰性终端)
  '#ff5f56': 'window dot',
  '#ffbd2e': 'window dot',
  '#27c93f': 'window dot',
  '#ff5f57': 'window dot',
  '#febc2e': 'window dot',
  '#28c840': 'window dot',
  // 指标/图例数据色板(需在两种主题下区分系列, 不是语义色)
  '#ff6b6b': 'series palette',
  '#4ecdc4': 'series palette',
  '#ffd93d': 'series palette',
  '#95e1d3': 'series palette',
  '#a8e6cf': 'series palette',
  '#ffd3b6': 'series palette',
  '#ffe66d': 'series palette',
  '#14b8a6': 'series palette',
  '#f97316': 'series palette',
  '#84cc16': 'series palette',
  '#06b6d4': 'series palette',
  '#9b59b6': 'series palette',
  // 品类强调色(粉)
  '#ec4899': 'category accent pink',
  '#f472b6': 'category accent pink',
  '#c0c4cc': 'neutral series',
  // Telegram 深色消息预览
  '#1a3a52': 'Telegram preview',
  '#2b5278': 'Telegram preview',
  '#c9d1d9': 'Telegram preview',
  '#8b949e': 'Telegram preview',
  // 文档里的 issue 号 "#123" 不是颜色
  '#123': 'not a color',
}
const ALLOW_RGB = new Set([
  '42,171,238', // Telegram
  '29,161,242', // Twitter
  '80,227,194', // Hyperliquid
  '127,231,204', // Hyperliquid
  '243,186,47', // Binance
  '236,72,153', // pink category accent
])
// 画布图表: 这些文件在 `return (` JSX 之前的行不转换(canvas 不支持 var())
const CANVAS_ZONE = {
  'components/charts/AdvancedChart.tsx': /^ {2}return \($/,
  'components/charts/ChartWithOrders.tsx': /^ {2}return \($/,
}

// ---------------------------------------------------------------- 映射表
// 角色: t=文字 b=背景 bd=边框 sv=svg fill/stroke s=阴影 d=默认
const HEX = {
  // --- 暖灰中性色(旧纸质主题)
  '#1e1e1a': { t: 'fg', b: 'fg', bd: 'fg', d: 'fg' },
  '#2a2a24': { d: 'fg-2' },
  '#3a3a32': { d: 'fg-2' },
  '#6e6e60': { t: 'fg-3', b: 'fg-3', bd: 'line-strong', d: 'fg-3' },
  '#7a7a6c': { d: 'fg-3' },
  '#8a8a7c': { d: 'fg-3' },
  '#6b7280': { d: 'fg-3' },
  '#3c4249': { d: 'fg-3' },
  '#c0b9a2': { t: 'fg-3', b: 'line', bd: 'line', sv: 'line', d: 'line' },
  '#b3ab92': { d: 'line-strong' },
  '#a69e86': { d: 'line-strong' },
  '#a9997b': { d: 'line-strong' },
  '#474d57': { d: 'line-strong' },
  '#f2efe6': { t: 'bg', b: 'surface-2', pg: 'bg', bd: 'bg', sv: 'surface', d: 'surface-2' },
  '#efece1': { d: 'surface-2' },
  '#ece8db': { d: 'surface' },
  '#e9e4d6': { b: 'surface-hover', bd: 'line', d: 'surface-hover' },
  '#e4e0d0': { d: 'surface-hover' },
  '#ebe7da': { d: 'surface-hover' },
  '#ede9dc': { d: 'surface-hover' },
  '#f4f1e8': { d: 'surface-hover' },
  // --- 品牌金
  '#b8912a': { d: 'brand' },
  '#c9a227': { d: 'brand' },
  '#b8860b': { d: 'brand' },
  '#e1a706': { d: 'brand' },
  '#d4951e': { d: 'brand' },
  '#8a6d1f': { d: 'brand' },
  '#e3d6a3': { d: 'brand' },
  // --- 涨 / 跌
  '#2e7d4f': { d: 'up' },
  '#256944': { d: 'up' },
  '#22c55e': { d: 'up' },
  '#4ade80': { d: 'up' },
  '#34d399': { d: 'up' },
  '#10b981': { d: 'up' },
  '#c0392b': { d: 'down' },
  '#ef4444': { d: 'down' },
  '#fb7185': { d: 'down' },
  '#fbeae7': { d: 'down-soft' },
  // --- 警告
  '#f59e0b': { d: 'warn' },
  '#fbbf24': { d: 'warn' },
  '#e8a64c': { d: 'warn' },
  '#fb923c': { d: 'warn' },
  '#ffc107': { d: 'warn' },
  // --- 信息 / 链接 / 强调(旧 sage 绿 #5e7a5e / #6b7f5e 是"选中强调色")
  '#5e7a5e': { d: 'info' },
  '#6b7f5e': { d: 'info' },
  '#3b82f6': { d: 'info' },
  '#60a5fa': { d: 'info' },
  '#58a6ff': { d: 'info' },
  '#4b9eff': { d: 'info' },
  '#4a90e2': { d: 'info' },
  '#3d7ea6': { d: 'info' },
  '#2c5f80': { d: 'info' },
  '#7a9bb5': { d: 'info' },
  '#38bdf8': { d: 'info' },
  // --- AI 紫
  '#a78bfa': { d: 'ai' },
  '#a855f7': { d: 'ai' },
  '#c084fc': { d: 'ai' },
  '#8b5cf6': { d: 'ai' },
  '#6366f1': { d: 'ai' },
  // --- 黑白(多为品牌色按钮上的文字)
  '#000': { t: 'brand-fg', d: 'brand-fg' },
  '#000000': { t: 'brand-fg', d: 'brand-fg' },
  '#111': { t: 'brand-fg', d: 'brand-fg' },
  '#fff': { t: 'brand-fg', d: 'brand-fg' },
  '#ffffff': { t: 'brand-fg', d: 'brand-fg' },
}
// rgba 调色板 -> 变量 (alpha 另算)
const RGB = {
  '184,145,42': 'brand',
  '201,162,39': 'brand',
  '138,109,31': 'brand',
  '255,215,0': 'brand',
  '234,179,8': 'brand',
  '252,213,53': 'brand',
  '46,125,79': 'up',
  '34,197,94': 'up',
  '16,185,129': 'up',
  '74,222,128': 'up',
  '52,211,153': 'up',
  '192,57,43': 'down',
  '239,68,68': 'down',
  '245,158,11': 'warn',
  '255,193,7': 'warn',
  '107,127,94': 'info',
  '59,130,246': 'info',
  '96,165,250': 'info',
  '75,158,255': 'info',
  '34,211,238': 'info',
  '167,139,250': 'ai',
  '168,85,247': 'ai',
  '139,92,246': 'ai',
  '192,132,252': 'ai',
  '99,102,241': 'ai',
  '132,142,156': 'fg-3',
  '192,185,162': 'line',
  '236,232,219': 'surface',
  '239,236,225': 'surface',
  '244,241,232': 'surface',
  '30,30,26': 'fg',
  '255,255,255': 'fg', // 白色叠加层 -> 随主题的前景色
}
// 固定"黑色阴影/遮罩"
const BLACK = '0,0,0'

// ---------------------------------------------------------------- 工具
const SOFT_NAMES = new Set(['brand', 'up', 'down', 'warn', 'info', 'ai'])
const stats = new Map() // key `${orig} -> ${out}` => count
const kept = new Map() // value => {reason,count}
const unmapped = new Map()
function bump(m, k) {
  m.set(k, (m.get(k) || 0) + 1)
}
function norm(v) {
  v = v.toLowerCase().replace(/\s+/g, '')
  return v
}
function hexParts(v) {
  // 返回 {base:'#rrggbb'|'#rgb', alpha:1}
  if (/^#[0-9a-f]{8}$/.test(v)) return { base: v.slice(0, 7), alpha: parseInt(v.slice(7), 16) / 255 }
  return { base: v, alpha: 1 }
}
function pct(a) {
  return Math.round(a * 100)
}
function cssVar(name, alpha, role) {
  if (alpha >= 0.995) return `var(--${name})`
  if (role === 'b' && SOFT_NAMES.has(name) && alpha >= 0.08 && alpha <= 0.2) return `var(--${name}-soft)`
  return `color-mix(in srgb, var(--${name}) ${pct(alpha)}%, transparent)`
}
function pickRole(entry, role) {
  return entry[role] || entry.d
}

// style 属性名 -> 角色
const KEY_ROLE = [
  [/^(color|caretColor|accentColor|textDecorationColor|WebkitTextFillColor|textColor)$/, 't'],
  [/^(background|backgroundColor|backgroundImage|bg)$/, 'b'],
  [/^(border\w*|outline\w*|rowBorder|borderColor)$/, 'bd'],
  [/^(fill|stroke|stopColor)$/, 'sv'],
  [/^(boxShadow|textShadow|filter)$/, 's'],
]
function roleFromBefore(before) {
  // 同一表达式内(最多 260 字符、不跨过 `}` `;` 之外的语句边界)找最近的 style 键
  const win = before.slice(-260)
  // CSS 字符串 "background:#fff;color:#000" 形式
  const css = win.match(/(color|background|border[\w-]*|fill|stroke)\s*:\s*[^;'"`,{}\n]*$/i)
  if (css) {
    const k = css[1].toLowerCase()
    if (k === 'color') return 't'
    if (k === 'background') return 'b'
    if (k.startsWith('border')) return 'bd'
    return 'sv'
  }
  // 最近的 "key:" / "key=": 已知样式键 -> 取其角色; 别的键(如 muted:) -> 无角色(用默认映射);
  // 若值前面仍有三元 "?" 说明在同一表达式里, 继续向前找。
  const re = /\b([A-Za-z]+)\s*[:=]\s*/g
  const found = []
  let m
  while ((m = re.exec(win))) found.push({ name: m[1], end: m.index + m[0].length })
  for (let i = found.length - 1; i >= 0; i--) {
    const f = found[i]
    if (f.name === 'bg') return 'pg'
    for (const [rx, role] of KEY_ROLE) if (rx.test(f.name)) return role
    if (win.slice(f.end).includes('?')) continue
    return undefined
  }
  return undefined
}

// ---------------------------------------------------------------- Tailwind 任意值类
const TW_PREFIX = {
  bg: 'b',
  text: 't',
  placeholder: 't',
  caret: 't',
  accent: 't',
  decoration: 't',
  border: 'bd',
  'border-t': 'bd',
  'border-b': 'bd',
  'border-l': 'bd',
  'border-r': 'bd',
  'border-x': 'bd',
  'border-y': 'bd',
  ring: 'bd',
  'ring-offset': 'bd',
  outline: 'bd',
  divide: 'bd',
  from: 'b',
  via: 'b',
  to: 'b',
  fill: 'sv',
  stroke: 'sv',
}
const TW_RE =
  /(?<![\w-])((?:[a-z0-9]+:)*)(bg|text|placeholder|caret|accent|decoration|border-[trblxy]|border|ring-offset|ring|outline|divide|from|via|to|fill|stroke)-\[(#[0-9a-fA-F]{3,8}|rgba?\([^\]]*?\))\](\/\d+)?/g

// 页面根容器(整屏高度)上的背景用 --bg, 其余 #f2efe6 是卡片内控件 -> --surface-2
const PAGE_RE = /min-h-screen|h-screen|100vh/

function resolveName(value, role, ctx) {
  // 返回 {name, alpha} 或 null(保留/未映射)
  let v = norm(value)
  if (v.startsWith('#')) {
    const { base, alpha } = hexParts(v)
    if (ALLOW_VALUES[base]) return { keep: ALLOW_VALUES[base] }
    const e = HEX[base]
    if (!e) return { unmapped: true }
    let name = pickRole(e, role)
    if (base === '#f2efe6' && role === 'b' && ctx.pageBg) name = 'bg'
    // 品牌色按钮上的 #fff/#000 文字: 若同一语句里有非品牌的强调背景(蓝/紫/红)则保留
    return { name, alpha }
  }
  const m = v.match(/^rgba?\((\d+),(\d+),(\d+)(?:,([\d.]+))?\)$/)
  if (!m) return { unmapped: true }
  const key = `${m[1]},${m[2]},${m[3]}`
  const alpha = m[4] === undefined ? 1 : parseFloat(m[4])
  if (ALLOW_RGB.has(key)) return { keep: 'brand rgba ' + key }
  if (key === BLACK) {
    if (role === 'b' && alpha >= 0.4) return { name: 'overlay', alpha: 1, special: true }
    return { keep: 'black shadow/overlay' }
  }
  const name = RGB[key]
  if (!name) return { unmapped: true }
  // 暖黑色的阴影在深色主题下应仍是黑色阴影
  if (role === 's' && key === '30,30,26') return { keep: 'dark shadow' }
  // 阴影/边框里的 line 半透明 -> 直接 var(--line)
  if (name === 'line' && (role === 'bd' || role === undefined) && alpha >= 0.4) return { name: 'line', alpha: 1 }
  return { name, alpha }
}

function migrateFile(rel, src) {
  if (ALLOW_FILES.has(rel)) {
    return src
  }
  const lines = src.split('\n')
  let canvasUntil = -1
  if (CANVAS_ZONE[rel]) {
    for (let i = lines.length - 1; i >= 0; i--) {
      if (CANVAS_ZONE[rel].test(lines[i])) {
        canvasUntil = i
        break
      }
    }
  }
  const offsets = [0]
  for (const l of lines) offsets.push(offsets[offsets.length - 1] + l.length + 1)
  const lineOf = (idx) => {
    let lo = 0
    let hi = offsets.length - 1
    while (lo < hi - 1) {
      const mid = (lo + hi) >> 1
      if (offsets[mid] <= idx) lo = mid
      else hi = mid
    }
    return lo
  }
  const inCanvas = (idx) => canvasUntil >= 0 && lineOf(idx) < canvasUntil

  // ---- Pass 1: Tailwind 任意值类
  let out = src.replace(TW_RE, (full, variants, prefix, val, op, offset) => {
    if (inCanvas(offset)) return full
    const role = TW_PREFIX[prefix]
    const ctx = { pageBg: PAGE_RE.test(lines.slice(Math.max(0, lineOf(offset) - 2), lineOf(offset) + 1).join('\n')) }
    const r = resolveName(val, role, ctx)
    if (r.keep) {
      bump(kept, `${norm(val)} (${r.keep})`)
      return full
    }
    if (r.unmapped) {
      bump(unmapped, `${norm(val)} [tw ${prefix}]`)
      return full
    }
    let name = r.name
    let suffix = op || ''
    if (r.alpha < 0.995 && !op) suffix = `/${pct(r.alpha)}`
    else if (r.alpha < 0.995 && op) suffix = `/${Math.round((pct(r.alpha) * parseInt(op.slice(1))) / 100)}`
    if (name === 'overlay') {
      name = 'fg'
      suffix = suffix || '/60'
    }
    bump(stats, `${norm(val)} [tw ${prefix}] -> ${prefix}-${name}${suffix}`)
    return `${variants}${prefix}-${name}${suffix}`
  })

  // ---- Pass 1b: shadow-[...rgba(...)...] 任意值阴影
  out = out.replace(/(?<![\w-])((?:[a-z0-9]+:)*)(shadow|drop-shadow)-\[([^\]]*rgba?\([^\]]*\)[^\]]*)\]/g, (full, variants, p, body) => {
    const rebuilt = body.replace(/rgba?\([^)]*\)/g, (c) => {
      const r = resolveName(c.replace(/_/g, ''), 's', {})
      if (r.keep || r.unmapped || !r.name) return c
      bump(stats, `${norm(c)} [tw shadow] -> ${r.name} ${pct(r.alpha)}%`)
      return `color-mix(in_srgb,var(--${r.name})_${pct(r.alpha)}%,transparent)`
    })
    return `${variants}${p}-[${rebuilt}]`
  })

  // ---- Pass 2: 其余字面量(style 对象 / JS 字符串 / SVG 属性 / 8 位 hex)
  const LIT =
    /#[0-9a-fA-F]{8}\b|#[0-9a-fA-F]{6}\b|#[0-9a-fA-F]{3}\b|rgba?\(\s*\d+\s*,\s*\d+\s*,\s*\d+\s*(?:,\s*[\d.]+\s*)?\)/g
  const outLines = out.split('\n')
  void outLines
  const o2offsets = [0]
  const lines2 = out.split('\n')
  for (const l of lines2) o2offsets.push(o2offsets[o2offsets.length - 1] + l.length + 1)
  // canvasUntil 在第一遍后行数不变(只改行内)
  const lineOf2 = (idx) => {
    let lo = 0
    let hi = o2offsets.length - 1
    while (lo < hi - 1) {
      const mid = (lo + hi) >> 1
      if (o2offsets[mid] <= idx) lo = mid
      else hi = mid
    }
    return lo
  }
  out = out.replace(LIT, (val, offset, whole) => {
    const li = lineOf2(offset)
    if (canvasUntil >= 0 && li < canvasUntil) return val
    // 跳过 Tailwind 已转换后的 "color-mix(... var(--x) ...)" 不会匹配 LIT, 无需处理
    const before = whole.slice(Math.max(0, offset - 260), offset)
    // #123 之类 issue 号: 前一个字符是字母/中文/空格后的 "Closes #123"
    if (/Closes\s*$/.test(before) || /^#123$/.test(val)) return val
    // 前一个字符是单词字符/斜杠(URL 片段、标识符)则不是颜色
    if (val.startsWith('#') && /[A-Za-z0-9_/&]$/.test(before)) return val
    const role = roleFromBefore(before)
    const lineText = lines2[li]
    const ctx = { pageBg: PAGE_RE.test(lines2.slice(Math.max(0, li - 2), li + 1).join('\n')) }
    const r = resolveName(val, role, ctx)
    if (r.keep) {
      bump(kept, `${norm(val)} (${r.keep})`)
      return val
    }
    if (r.unmapped) {
      bump(unmapped, `${norm(val)} [${role || 'd'}]`)
      return val
    }
    let name = r.name
    // 品牌色按钮上的黑/白文字: 同一行里还有非品牌的强调背景(蓝/紫/青/红 toast) -> 保留原值
    if (name === 'brand-fg' && /(?:2AABEE|7FE7CC|8B5CF6|C0392B)/i.test(lineText)) {
      bump(kept, `${norm(val)} (text on third-party/accent bg)`)
      return val
    }
    // 8 位 hex(带 alpha)
    const { alpha } = val.startsWith('#') ? hexParts(norm(val)) : { alpha: r.alpha }
    const finalAlpha = r.alpha !== undefined ? r.alpha : alpha
    let res
    if (name === 'overlay') res = 'var(--overlay)'
    else if (name.endsWith('-soft')) res = finalAlpha >= 0.995 ? `var(--${name})` : `var(--${name})`
    else res = cssVar(name, finalAlpha, role)
    bump(stats, `${norm(val)} [${role || 'd'}] -> ${res}`)
    return res
  })

  return out
}

// ---------------------------------------------------------------- 主流程
function walk(dir, acc = []) {
  for (const f of fs.readdirSync(dir)) {
    const p = path.join(dir, f)
    if (fs.statSync(p).isDirectory()) walk(p, acc)
    else if (/\.(ts|tsx|css)$/.test(f)) acc.push(p)
  }
  return acc
}

let changed = 0
for (const file of walk(ROOT)) {
  const rel = path.relative(ROOT, file)
  if (rel === 'index.css') continue // 变量定义本身
  if (/\.(test|spec)\.(ts|tsx)$/.test(rel)) continue
  const src = fs.readFileSync(file, 'utf8')
  const out = migrateFile(rel, src)
  if (out !== src) {
    changed++
    if (!DRY) fs.writeFileSync(file, out)
  }
}

const sorted = (m) => [...m.entries()].sort((a, b) => b[1] - a[1])
console.log(`# files changed: ${changed}${DRY ? ' (dry run)' : ''}`)
console.log('\n## mapping (original [role] -> output) : count')
for (const [k, n] of sorted(stats)) console.log(`${String(n).padStart(4)}  ${k}`)
console.log('\n## kept (allowlist) : count')
for (const [k, n] of sorted(kept)) console.log(`${String(n).padStart(4)}  ${k}`)
console.log('\n## UNMAPPED (needs a table entry) : count')
for (const [k, n] of sorted(unmapped)) console.log(`${String(n).padStart(4)}  ${k}`)
