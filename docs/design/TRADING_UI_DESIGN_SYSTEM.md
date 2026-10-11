# NOFX 交易平台 UI 设计规范 · 2026-10-11

目标：把现有"米色纸质"风格改造成现代交易平台风格（参考主流交易所与 TradingView 的通用做法，不复制任何品牌的标识或专有配色组合）。默认深色主题，保留浅色主题可切换。

## 1. 设计原则

1. **数据优先**：数字是主角。价格、数量、盈亏一律等宽数字（tabular-nums）、右对齐，涨跌色高对比。
2. **高密度但不拥挤**：表格行高 32–36px，卡片内边距 12–16px，减少大面积留白。
3. **一套变量管所有颜色**：组件里不再出现写死的色值，全部引用设计变量；主题切换只改变量。
4. **控件统一**：按钮、标签页、分段选择、徽标、卡片、表格使用同一套公共组件，状态（hover/active/disabled/focus）一致。
5. **克制的强调色**：品牌金只用于主操作和当前选中态；紫色仅用于 AI 相关标识；不使用霓虹、扫描线、故障抖动等装饰特效。

## 2. 设计变量（CSS 变量，定义在 `web/src/index.css`）

主题挂在 `<html data-theme="dark|light">`，默认 `dark`，用户选择存 localStorage（读写包 try/catch）。

| 变量 | 用途 | 深色 | 浅色 |
| --- | --- | --- | --- |
| `--bg` | 页面底色 | `#0B0E11` | `#F5F6F8` |
| `--surface` | 卡片/面板 | `#14181D` | `#FFFFFF` |
| `--surface-2` | 卡片内控件、表头、输入框 | `#1C2127` | `#F3F4F6` |
| `--surface-hover` | 行/控件悬停 | `#232931` | `#EBEDF0` |
| `--line` | 常规边框/分隔线 | `#262C34` | `#E3E6EA` |
| `--line-strong` | 强调边框、焦点前 | `#363D47` | `#CDD2D9` |
| `--fg` | 主文字 | `#EAECEF` | `#1E2329` |
| `--fg-2` | 次文字 | `#B7BDC6` | `#474D57` |
| `--fg-3` | 弱文字/标签 | `#848E9C` | `#707A8A` |
| `--fg-disabled` | 禁用 | `#5E6673` | `#B0B6BF` |
| `--brand` | 品牌金：主按钮、选中态 | `#E0B03A` | `#B8891A` |
| `--brand-fg` | 品牌色上的文字 | `#0B0E11` | `#FFFFFF` |
| `--brand-soft` | 品牌色浅底（选中标签、徽标） | `rgba(224,176,58,.14)` | `rgba(184,137,26,.12)` |
| `--up` | 上涨/盈利/做多 | `#0ECB81` | `#03A66D` |
| `--up-soft` | 上涨浅底 | `rgba(14,203,129,.12)` | `rgba(3,166,109,.10)` |
| `--down` | 下跌/亏损/做空 | `#F6465D` | `#CF304A` |
| `--down-soft` | 下跌浅底 | `rgba(246,70,93,.12)` | `rgba(207,48,74,.10)` |
| `--warn` | 警告 | `#F0A020` | `#C77700` |
| `--warn-soft` | 警告浅底 | `rgba(240,160,32,.14)` | `rgba(199,119,0,.10)` |
| `--info` | 信息/链接 | `#3B8BEB` | `#1F6FD1` |
| `--info-soft` | 信息浅底 | `rgba(59,139,235,.14)` | `rgba(31,111,209,.10)` |
| `--ai` | AI 相关标识（模型、思维链） | `#9B7BF7` | `#6D4FD8` |
| `--ai-soft` | AI 浅底 | `rgba(155,123,247,.14)` | `rgba(109,79,216,.10)` |
| `--shadow` | 浮层阴影 | `0 8px 24px rgba(0,0,0,.45)` | `0 8px 24px rgba(16,24,40,.10)` |

Tailwind 映射（`tailwind.config.js`）：`bg-bg / bg-surface / bg-surface-2 / bg-surface-hover`、`border-line / border-line-strong`、`text-fg / text-fg-2 / text-fg-3`、`text-brand / bg-brand / bg-brand-soft`、`text-up / bg-up-soft`、`text-down / bg-down-soft`、`text-warn`、`text-info`、`text-ai` 等，颜色值写 `var(--x)`。

旧的 `nofx-*` 颜色与旧 CSS 变量（`--panel-bg`、`--binance-green` 等）**保留名称、改指向新变量**，保证未改造的页面也随主题切换。

## 3. 字体与排版

- 界面文字：`Inter, "PingFang SC", "Microsoft YaHei", system-ui, sans-serif`（不再用等宽字体做正文）。
- 数字：`JetBrains Mono, "SF Mono", Menlo, monospace` + `font-variant-numeric: tabular-nums`；提供工具类 `.num`（等宽数字）。价格、数量、百分比、时间戳一律 `.num`。
- 字号：页面标题 20/600，卡片标题 14/600，正文 13–14，表格 12–13，辅助标签 11–12。行高 1.45。
- 数字格式：涨跌带符号（`+1.23%` / `−1.23%`），盈亏用 `--up/--down` 着色，零值用 `--fg-2`。

## 4. 间距、圆角、层级

- 间距基于 4px：4 / 8 / 12 / 16 / 24。
- 圆角：控件 6px，卡片 8px，徽标 4px，胶囊标签 999px。
- 层级：页面 `--bg` → 卡片 `--surface`（1px `--line` 边框，无阴影）→ 卡片内控件 `--surface-2`；只有弹窗、下拉、提示浮层用 `--shadow`。

## 5. 公共组件（`web/src/components/ui/`）

| 组件 | 要点 |
| --- | --- |
| `Button` | 变体 primary（品牌金）/ secondary（surface-2+边框）/ ghost / danger / up / down；尺寸 sm(28)/md(32)/lg(40)；loading 与 disabled 态 |
| `Card` | `Card`, `CardHeader`(标题+右侧操作区), `CardBody`；可选 `dense` |
| `Tabs` | 下划线式（页面级）与分段式 `Segmented`（时间周期、视图切换）；选中态品牌色 |
| `Badge` | 变体 neutral/brand/up/down/warn/info/ai；尺寸 xs/sm |
| `Stat` | 指标块：标签（fg-3, 12px）+ 数值（.num, 20–24px）+ 涨跌副值 |
| `DataTable` | 表头 sticky、surface-2 底、12px fg-3；行高 34，悬停 surface-hover；数字列右对齐 .num；空态/加载态 |
| `PnL` / `Change` | 统一的带符号着色数字展示 |
| `Select` / `Input` | 已有组件改用变量，高度 32，焦点环 `--brand` |
| `Tooltip`、`EmptyState`、`Skeleton` | 统一样式 |

## 6. 图表

lightweight-charts / recharts 的背景、网格线、坐标文字、十字线、K 线涨跌色从 CSS 变量读取（提供 `getChartTheme()`），主题切换时更新图表选项。K 线：上涨 `--up` 实心，下跌 `--down` 实心；网格线用 `--line` 降透明度。

## 7. 页面布局

- 顶部导航：高 52px，`--surface` 底，左 Logo + 主导航（当前项品牌色下划线），右侧主题切换、语言、账户。
- 交易员面板：顶部指标条（总权益、可用、总盈亏、持仓数，Stat 组件横排）→ 风控状态条 → 主区左侧图表、右侧最近决策（宽屏两栏，窄屏单栏）→ 底部持仓/委托/历史表格（Tabs + DataTable）。
- 决策卡片：标题行（周期号、时间、状态徽标）→ 决策动作行（币种、方向徽标 up/down、价格 .num）→ 候选列表（每行：币种、状态徽标、拦截原因 chips）→ 可折叠的提示词/思维链区。
- 首页：去掉扫描线、故障抖动、"SYSTEM_DIAGNOSTICS"等装饰，改为简洁的产品首屏 + 能力卡片。

## 8. 实施顺序

1. 基础：变量、主题切换、字体、旧变量重定向、公共组件、全仓库写死色值 → 变量的机械替换。
2. 交易员面板 + 决策记录（最常用）。
3. 策略、复盘、数据中心、设置等其余页面；图表主题。
4. 首页与导航。

每阶段完成后在浏览器中逐页截图核对深色/浅色两套主题。
