/**
 * 颜色变量 + Tailwind 透明度修饰符(如 bg-brand/20)支持:
 * 有修饰符时用 color-mix 生成半透明色, 无修饰符直接用 var()。
 */
const v = (name) => ({ opacityValue }) => {
  if (opacityValue === undefined || String(opacityValue).startsWith('var(')) {
    return `var(--${name})`
  }
  return `color-mix(in srgb, var(--${name}) calc(${opacityValue} * 100%), transparent)`
}

/** @type {import('tailwindcss').Config} */
export default {
  content: [
    "./index.html",
    "./src/**/*.{js,ts,jsx,tsx}",
  ],
  theme: {
    extend: {
      colors: {
        // 语义色 — 值来自 index.css 的 CSS 变量(深/浅主题)
        bg: v('bg'),
        surface: {
          DEFAULT: v('surface'),
          2: v('surface-2'),
          hover: v('surface-hover'),
        },
        line: {
          DEFAULT: v('line'),
          strong: v('line-strong'),
        },
        fg: {
          DEFAULT: v('fg'),
          2: v('fg-2'),
          3: v('fg-3'),
          disabled: v('fg-disabled'),
        },
        brand: {
          DEFAULT: v('brand'),
          fg: v('brand-fg'),
          soft: v('brand-soft'),
        },
        up: { DEFAULT: v('up'), soft: v('up-soft') },
        down: { DEFAULT: v('down'), soft: v('down-soft') },
        warn: { DEFAULT: v('warn'), soft: v('warn-soft') },
        info: { DEFAULT: v('info'), soft: v('info-soft') },
        ai: { DEFAULT: v('ai'), soft: v('ai-soft') },

        // 旧 nofx-* 名称保留, 指向新变量
        'nofx-gold': {
          DEFAULT: v('brand'),
          dim: v('brand-soft'),
          glow: v('brand-soft'),
          highlight: v('brand'),
        },
        'nofx-bg': {
          DEFAULT: v('bg'),
          deeper: v('bg'),
          lighter: v('surface'),
        },
        'nofx-card': v('surface'),
        'nofx-inset': v('surface-2'),
        'nofx-line': v('line'),
        'nofx-accent': v('info'),
        'nofx-text': {
          DEFAULT: v('fg'),
          main: v('fg'),
          muted: v('fg-3'),
        },
        'nofx-success': v('up'),
        'nofx-danger': v('down'),
      },
      fontFamily: {
        sans: ['Inter', '"PingFang SC"', '"Microsoft YaHei"', 'system-ui', 'sans-serif'],
        mono: ['"JetBrains Mono"', '"SF Mono"', 'Menlo', 'Monaco', '"Courier New"', 'monospace'],
        num: ['"JetBrains Mono"', '"SF Mono"', 'Menlo', 'Monaco', '"Courier New"', 'monospace'],
      },
      backgroundImage: {
        'gradient-radial': 'radial-gradient(circle at center, var(--tw-gradient-stops))',
        'gradient-conic': 'conic-gradient(from 180deg at 50% 50%, var(--tw-gradient-stops))',
        'scanlines': 'none',
        'grid-pattern': "linear-gradient(to right, var(--line) 1px, transparent 1px), linear-gradient(to bottom, var(--line) 1px, transparent 1px)",
      },
      animation: {
        'pulse-slow': 'pulse 4s cubic-bezier(0.4, 0, 0.6, 1) infinite',
        'shimmer': 'shimmer 2s linear infinite',
      },
      keyframes: {
        shimmer: {
          '0%': { backgroundPosition: '-200% 0' },
          '100%': { backgroundPosition: '200% 0' },
        },
      },
      boxShadow: {
        // 旧霓虹阴影已中和为普通浮层阴影
        'neon': 'var(--shadow)',
        'neon-blue': 'var(--shadow)',
        'pop': 'var(--shadow)',
      },
    },
  },
  plugins: [],
}
