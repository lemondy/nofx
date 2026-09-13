/** @type {import('tailwindcss').Config} */
export default {
  content: [
    "./index.html",
    "./src/**/*.{js,ts,jsx,tsx}",
  ],
  theme: {
    extend: {
      colors: {
        'nofx-gold': {
          DEFAULT: '#B8912A',
          dim: 'rgba(184, 145, 42, 0.1)',
          glow: 'rgba(184, 145, 42, 0.4)',
          highlight: '#C9A227',
        },
        'nofx-bg': {
          DEFAULT: '#F2EFE6', // Paper — page bg + inset controls
          deeper: '#EFECE1',
          lighter: '#ECE8DB', // Card surface
        },
        'nofx-card': '#ECE8DB', // 卡片面板(比页面更深,让内嵌控件浮起)
        'nofx-inset': '#F2EFE6', // 卡片内控件(与卡片形成对比)
        'nofx-line': '#C0B9A2', // 边框线
        'nofx-accent': '#6B7F5E', // Sage
        'nofx-text': {
          DEFAULT: '#1E1E1A',
          main: '#1E1E1A',
          muted: '#6E6E60',
        },
        'nofx-success': '#2E7D4F',
        'nofx-danger': '#C0392B',
      },
      fontFamily: {
        sans: ['Inter', 'ui-sans-serif', 'system-ui'],
        mono: ['JetBrains Mono', 'Menlo', 'Monaco', 'Courier New', 'monospace'],
      },
      backgroundImage: {
        'gradient-radial': 'radial-gradient(circle at center, var(--tw-gradient-stops))',
        'gradient-conic': 'conic-gradient(from 180deg at 50% 50%, var(--tw-gradient-stops))',
        'scanlines': "url(\"data:image/svg+xml,%3Csvg width='4' height='4' viewBox='0 0 4 4' fill='none' xmlns='http://www.w3.org/2000/svg'%3E%3Cpath d='M0 0H4V2H0V0Z' fill='rgba(0,0,0,0.4)'/%3E%3C/svg%3E\")",
        'grid-pattern': "linear-gradient(to right, #1f2937 1px, transparent 1px), linear-gradient(to bottom, #1f2937 1px, transparent 1px)",
      },
      animation: {
        'pulse-slow': 'pulse 4s cubic-bezier(0.4, 0, 0.6, 1) infinite',
        'scan': 'scan 8s linear infinite',
        'scan-fast': 'scan 2s linear infinite',
        'float': 'float 6s ease-in-out infinite',
        'glitch': 'glitch 0.3s cubic-bezier(.25, .46, .45, .94) both infinite',
        'shimmer': 'shimmer 2s linear infinite',
      },
      keyframes: {
        scan: {
          '0%': { backgroundPosition: '0 0' },
          '100%': { backgroundPosition: '0 100%' },
        },
        float: {
          '0%, 100%': { transform: 'translateY(0)' },
          '50%': { transform: 'translateY(-10px)' },
        },
        glitch: {
          '0%': { transform: 'translate(0)' },
          '20%': { transform: 'translate(-2px, 2px)' },
          '40%': { transform: 'translate(-2px, -2px)' },
          '60%': { transform: 'translate(2px, 2px)' },
          '80%': { transform: 'translate(2px, -2px)' },
          '100%': { transform: 'translate(0)' },
        },
        shimmer: {
          '0%': { backgroundPosition: '-200% 0' },
          '100%': { backgroundPosition: '200% 0' },
        },
      },
      boxShadow: {
        'neon': '0 0 5px theme("colors.nofx-gold.DEFAULT"), 0 0 20px theme("colors.nofx-gold.dim")',
        'neon-blue': '0 0 5px theme("colors.nofx-accent"), 0 0 20px rgba(0, 240, 255, 0.2)',
      },
    },
  },
  plugins: [],
}
