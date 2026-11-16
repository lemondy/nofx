# NOFX Architecture Documentation

**Language:** **English** | [中文](README.zh-CN.md)

> Last updated: 2026-11-15 (based on the full-stack deep review of that day, baseline `feat/factor-quant-regime-tilt` @ `abb9b47a`). Kept in sync with the Chinese version.

Technical documentation for developers who want to understand NOFX internals.

---

## Overview

NOFX is a **self-hosted, single-user** full-stack AI trading platform covering crypto perpetual futures and US stocks (Binance bStock tokens + paper ledgers). The system has three main lines:

1. **LLM trading path** (in this repo, "quant" without qualifier defaults to this line): the kernel decision engine pre-computes market data, indicators and scanner evidence into structured signals injected into prompts; an LLM produces open/close decisions; the execution layer validates and places orders programmatically.
2. **factor_quant programmatic path**: factor mining → frozen backtests (S1) → paper forward validation (S2) → purely programmatic rebalancing. No LLM involved; paper-only is enforced at code level.
3. **Research methodology**: preregistration, evolution ledger, K-accumulated multiple-testing correction, and the three-stage incremental evaluation (S1/S2/S3).

**Tech stack:**

- **Backend:** Go 1.25, Gin, GORM (production driver `gorm.io/driver/sqlite` → mattn/go-sqlite3 CGO; Postgres is a first-class option)
- **Frontend:** React 18 + TypeScript 5.8 + Vite 6, TailwindCSS, SWR, axios, zustand
- **AI providers:** DeepSeek, OpenAI, Claude, Qwen, Gemini, Grok, Kimi, MiniMax, GLM + custom endpoints
- **Exchanges:** Binance (futures, primary), Bybit, OKX, Bitget, Gate, KuCoin, Hyperliquid, Aster, Lighter, Indodax (spot skeleton), Binance bStock (US stocks), paper trading

---

## System Overview

```
                         ┌──────────────────────────────────────────┐
                         │              web/ (React SPA)            │
                         │   nginx serves static + /api reverse proxy│
                         │        (Go serves no static files)       │
                         └───────────────────┬──────────────────────┘
                                             │ /api (JWT)
┌────────────────────────────────────────────▼───────────────────────────────────────────┐
│  Go backend (main.go)                                                                  │
│                                                                                        │
│  manager/trader_manager ── builds/starts/recovers AutoTraders (all users × traders)    │
│        │                                                                               │
│        ▼                                                                               │
│  trader.AutoTrader ── four strategy shapes: ai_trading / grid_trading / us_stock /     │
│    │            │                      factor_quant                                    │
│    │            ▼                                                                      │
│    │      kernel (LLM decision engine) ── mcp (9+1 AI provider clients)                │
│    │            ▲                                                                      │
│    ▼            │                                                                       │
│  trader/ execution layer (entry / exit / protective orders / reconciliation / risk)    │
│    │                                                                                   │
│    ▼                                                                                   │
│  exchange adapters (binance/bybit/okx/.../paper)                                       │
│                                                                                        │
│  Support: market (klines/indicators/scanners)  provider (external data)  store         │
│           (SQLite/Postgres)  security (SSRF)  netmeter (traffic metering)              │
│           fundsledger  telegram  cmd/* tools                                           │
└────────────────────────────────────────────────────────────────────────────────────────┘
                                             │
        External: Binance fapi / CoinAnk / vergex (headless relay) / Hyperliquid /
                  Yahoo·bStock / Alpaca / TwelveData / OpenBB sidecar / sentiment feeds
```

---

## Project Structure

```
nofx/
├── main.go                    # Process entry: assembly, startup, graceful shutdown
├── manager/                   # Multi-trader manager (load/start/recover/RunVersion reload)
├── api/                       # Gin routes + middleware + all handlers (routes-as-docs)
├── kernel/                    # LLM decision engine: signal layer/prompt/validation/derivation
│   └── stockengine/           #   US-stock engine (Kell cycle phases, long-only spot contract)
├── mcp/                       # AI client abstraction + 9 providers + streaming/limits/retry
├── trader/                    # Trading execution layer (largest package, 142 source files)
│   ├── types/                 #   Trader/GridTrader interfaces, TradeRecord
│   ├── binance/  bybit/  okx/  bitget/  gate/  kucoin/          # Exchange adapters
│   ├── hyperliquid/  aster/  lighter/  indodax/                 #
│   ├── binance_bstock/  binance_stocks/                         # US-stock executors
│   └── paper/                 #   Crypto paper broker (matching + funding)
├── market/                    # Market data: klines/indicators/scanner family
│   ├── breakout/              #   Breakout/short scanners, online tuner, BTC regime
│   ├── pubcache/              #   Public HTTP cache + single-flight
│   ├── bnweight/              #   Binance fapi global rate limiting (weight buckets)
│   └── usstock/               #   bStock/Yahoo data, NYSE calendar
├── provider/                  # External data sources: coinank/vergex/hyperliquid/alpaca/
│                              #   twelvedata/openbb/nofxos (deprecated, types reused only)
├── store/                     # GORM persistence: 40+ tables, migrations, busy retry
├── fundsledger/               # Funds ledger pure-function aggregation (/api/funds/overview)
├── security/                  # SSRF protection: ValidateURL + SafeHTTPClient
├── crypto/                    # RSA-OAEP + AES-GCM envelope encryption (exchange keys)
├── auth/                      # JWT(HS256) + bcrypt + in-memory blacklist
├── config/                    # .env process-level config (no hot reload)
├── hook/                      # Global string-key hook registry (extension point, unused)
├── logger/                    # logrus, daily rotation data/nofx_YYYY-MM-DD.log
├── netmeter/                  # RoundTripper byte metering → /api/net-traffic
├── telegram/                  # Notifications + LLM proxy bot (agent/) + sessions (session/)
├── telemetry/  wallet/        # Anonymous telemetry; Base-chain USDC balance check
├── cmd/                       # Ops tools: placetp/protectposition/cancelorphan/
│                              #   listalgos/llmreplay/cycleic/repair20261007
├── web/                       # React frontend (see Frontend Architecture)
├── scripts/                   # Research infra: backtest/factor_mining/autoresearch/promotion
├── docs/                      # architecture / research (33 prereg docs) / api / design
├── data/                      # Runtime data: data.db, evolution/ledger.jsonl,
│                              #   factor_mining/, quant_targets.json (not committed)
├── docker/  nginx/  railway/  # Deployment shapes
└── AGENTS.md                  # AI collaboration & research process conventions
```

---

## Backend Architecture

### Process Startup & Lifecycle

`main.go:32-204` in order:

1. `godotenv.Load()` reads CWD `.env` → `logger.Init` → `config.Init`
2. `crypto` init (Fatal on failure) → `store` init (GORM AutoMigrate on all tables)
3. `openbb.EnsureSidecar` (fail-open, optional local data plane)
4. `manager.LoadTradersFromStore`: iterates all users; a trader loads only with strategy + AI model + enabled exchange; those with `IsRunning=1` are restored in place via `StartIfDesired(RunVersion)`
5. `api.NewServer` (includes a 72h auto-review loop) → telegram supervisor goroutine → `notify.Init` → breakout scan engine → vergex headless relay

Shutdown: `server.Shutdown`(5s) → second Ctrl+C force-exits → `StopAll` (`SignalStop` + parallel `StopForRestart`, us_stock keeps orders). **Single-user system**: registration permanently closes after the first user (`api/handler_user.go:51-63`).

### Multi-Trader Management (manager)

- In-memory registry + lock order `loadMu → tm.mu`; `StartTrader` does lookup+start in one critical section to prevent orphan loops
- Reloads use **RunVersion**: `StopForRestart` (keeps orders) is distinct from a real stop (cancels orders)
- **Authorization never goes through manager**: data handlers resolve via `getTraderFromQuery` → `GetForUser` (WHERE user_id); public endpoints gate on `ShowInCompetition`

### HTTP API & Middleware

Request chain: nginx → `gin.Logger/Recovery` → `SetTrustedProxies(nil)` → `requestLimits` (body ≤2MB; fixed-window rate limits: 600/min default, 120/min market data, 10/min login) → CORS `*` → routes → protected routes pass `authMiddleware` (Bearer JWT HS256 + DB token revocation + TokenVersion, `api/server.go:718-764`).

Route groups (full list in `api/server.go:88-487` and `docs/api/API_REFERENCE.md`): auth & system surface, trader CRUD/start/stop/prompt/sync-balance, strategy CRUD/preview-prompt/test-run, market data (public klines/symbols/trending/breakout), trading data (status/account/positions/decisions/statistics/usstock), funds (funds/overview, exchanges), models & AI cost, research & review (review/factor-mining/promotion/code-reviews/system-quality), system (net-traffic/telegram).

**Convention: routes are the docs** — the schema in `api/route_registry.go` is also injected into the telegram agent's prompt, so docs and implementation share one source.

### Four Strategy Shapes

`trader/auto_trader.go:666-810` forks `run()` by strategy type:

| Shape | Loop | Notes |
|---|---|---|
| **ai_trading** (LLM trading) | `runCycle`, aligned to 5-minute boundaries | kernel decisions + programmatic execution gates, see next sections |
| **grid_trading** | `RunGridCycle` | Grid strategy; shares the futures monitor with ai_trading |
| **us_stock** | `runStockLoop` | ET preset decision schedule + 60s protection tick; stockengine five-phase cycle (reconcile → data + weekly drawdown breaker → regime scaling → LLM → in-lock review & execute); bStock paper ledger |
| **factor_quant** | `runFactorQuantLoop`, independent 5-min tick | Purely programmatic rebalancing, **paper enforced at code level** (`factorQuantGuard` rejects non-paper), see below |

Scheduling alignment: `nextAlignedWait` aligns scans to the clock grid (5m → :00/:05/…); skipped boundaries are not replayed; the first cycle runs immediately at startup.

### LLM Decision Engine (kernel + mcp)

**kernel/ grouped by theme:**

| Theme | Key files | Responsibility |
|---|---|---|
| Entry/orchestration | `engine_analysis.go` | `GetFullDecisionWithStrategy` main flow, regime-skip, JSON repair parsing, intra-batch conflict resolution |
| Types/Schema | `engine.go` | `Context`/`Decision`, wait_state/stage derivation, candidate sources (static/ai500/oi_top/oi_low/piggy_dash/short_scan/hyper_*/mixed) |
| Validation/snapping | `engine_position.go` | `validateDecision`, anchor/SL/TP snapping (0.05% tolerance), annotation backfill |
| Signal layer | `signal_layer.go` (3655 lines) | `ComputeSymbolSignals` per-coin signal block, `computeHardEntryGate` bidirectional gate, `classifyTrend` four quadrants (up/pullback/down/rally), `methodStopPlan`, RR scan, TP menu |
| Gate helpers | `regime_line.go` `volatile_gate.go` `market_regime.go` `bb_ride.go` | EMA50 regime-line guard, thin-volume/vol-spike/OI-surge absolute bans, closed 1h/4h regime, Bollinger ride evidence |
| Prompt | `engine_prompt.go` + `schema.go` | System/user prompt sections, bilingual data dictionary, hard constraints rendered from live config |
| US stocks | `stockengine/` | Kell cycle phase classification, long-only spot contract (model never outputs quantity), order rejected when confidence<80 |

**mcp/**: interface `AIClient` (`mcp/interface.go:14`); a `Client` template base + 9 provider subpackages registered via `RegisterProvider`; streaming SSE (90s idle watchdog, tiered hard caps 300s first try / 600s retry); per-host+key concurrency limits; context guard never drops system messages.

**One decision lifecycle** (crypto cycle):

1. **Cycle prep** (`trader/auto_trader_loop.go:27`, holding the account execution lock): settle pending limit orders, vol-target/trailing stops, protective-order watchdog, orphaned position reconciliation, auth breaker check, config drift check
2. **Build context**: account/position snapshot (incl. exchange SL/TP live query), loss-streak ban map, last 5 closed trades + rolling stats; equity snapshot persisted, intraday breaker baseline anchored
3. **Data fetch**: multi-timeframe klines for the execution venue (1h/4h forced), candidates below the OI floor dropped
4. **Prompt assembly**: system prompt kept byte-stable (provider prefix cache hits); each candidate gets a `ComputeSymbolSignals` JSON block driven by ~30 config switches, with `GateStates` captured (the source of truth for later snapping/derivation); over-budget strips ohlcv first, then fails the cycle
5. **Regime-skip**: if all candidates are hard-blocked and all AI positions are close-locked ⇒ no LLM call; the program synthesizes a wait (`ALL_CANDIDATES_HARD_BLOCKED`), zero tokens
6. **Model call** → parsing (CoT extraction, JSON repair/truncation salvage)
7. **Programmatic snapping & validation**: `correctLimitAnchors` (limit opens with suppressed anchors downgrade to wait) → `correctStopLossToPlan`/`correctTakeProfitToPlan` (snap back to precomputed stop_plan / TP menu) → `validateDecision` (action enum/required fields/leverage clamp/position cap) → **program-derived annotations override model claims** (wait_state/stage/blocking_factors) → intra-batch conflict resolution (opposing opens → wait)
8. **trader execution**: close-first ordering, STATE_CHANGED quantity-drift rejection, shadow-block recording, preTrade rules, `applyHardRiskGates`, per-decision execution (margin reservation/release)
9. **Persistence**: `decision_records` (full prompts/CoT/decision JSON, system prompt hash-deduped), `entry_assessments` (one row per decision, quality dataset), `gate_shadow_blocks` (counterfactuals for hard-blocked entries with a complete triple; adjudicated on 15m klines at 8h/48h as tp_first/sl_first/timeout to calibrate thresholds), `ai_charges`

### Execution Layer (trader/)

**Entry paths**: limit is the default (`limit_entry_enabled`); anchors = snapshot price ± clamp(0.5×ATR(1h)); crossed anchors fall back to market; two market-order exceptions (breakout six conditions, bb_ride) re-verified by `marketExceptionGate` (`trader/auto_trader_orders.go:150`); spread gate, OI floor, margin-budget hard gate, max_positions slot accounting enforced on all three paths.

**Exit paths**: full protective-order verification chain (`placeProtectiveOrders` → `verifyProtectiveLegs` double re-check → Telegram alert on missing legs; protection failure = entry failure, contract F01); 30s protection watchdog re-arms/re-hangs/cancels orphan legs; 1R profit lock (`initial_stop_loss` write-once anchor; at 1R trim 50% + SL to entry); TP ladder (tp_trim/tp_full); TP menu exit_mode (trend/range/quick); adjust_stop_loss tighten-only and breakeven-or-better; early-close gate (<4h requires ≥2 counter-trend 1h closes).

**Risk subsystems**: account-level breakers (daily loss / drawdown), loss-streak ban, drawdown-protect, margin budget, funding-crowding annualized gate; the account execution lock is shared per exchangeID across traders (`account_execution.go:225`).

**Reconciliation & recovery**: startup reconciliation + `lim-<hash4>-` client-ID ownership tags re-claimed after restart; `reconcileOrphanedPositionRows` settles DB/exchange drift every cycle (only cancels this trader's tagged orders; manual orders are hands-off and never cancelled); pending shadow rows recover after restart; config drift self-check (DB vs in-memory config hash).

**Exchange adapter status:**

| Adapter | Maturity | Notes |
|---|---|---|
| binance | Full (primary) | order_sync/user_stream/rules cache/thin-OI threshold |
| paper | Full | Crypto paper broker; the factor_quant broker |
| binance_bstock / binance_stocks | Full | us_stock executors; binance_stocks has no stop-loss API |
| bybit/okx/bitget/hyperliquid/aster/lighter | Full | Trader+order_sync+entry band |
| gate/kucoin | Usable | No PlaceLimitOrder (limit entries fail-closed) |
| indodax | Skeleton | Spot mapping; OpenShort/SetStopLoss error out (see Known Issues P1-2) |

### factor_quant Programmatic Trader

`trader/auto_trader_factor_quant*.go`: independent 5-minute tick → portfolio-level risk (tiered drawdown scaling 10%→0.70x / 15%→0.40x / 20%→0.25x, ahead of the 25% breaker) → reads `data/quant_targets.json` (produced by the factor_mining pipeline) → rebalance gate (horizon/as_of/stale) → phased rebalance (close → trim → add → open) → stop re-hang / Chandelier trailing stop → equity snapshot. Added on the current branch: bull/bear regime tilt (benchmark daily EMA20/50; 20 long / 10 short asymmetric in bull), rolling alpha-decay exit, 2.5σ adaptive stops, 1/σ² short sizing. All four switches default on (`store/strategy_factor_quant.go:66-81`).

The full chain (mining → S1 → S2 → live) is under "Research & Backtesting Infrastructure".

### Data Layer

**K-line three-tier chain** (`market/data_klines.go`): CoinAnk (primary) → direct Binance fapi fallback (same exchange only) → OpenBB/yfinance tier-3 (majors only). Multi-timeframe: 15m aggregated locally from 3m (zero extra vendor calls); forming candles backfilled with live price (±80% sanity band); indicators all hand-written (EMA/Wilder RSI/ATR/MACD/BOLL, `market/data_indicators.go`), **closed bars only**; staleness detection MaxStaleBars=3.

**HTTP base (all outbound traffic)**: `netmeter.Wrap` byte metering → `pubcache` (public GET cache + single-flight, exact-path whitelist) → `bnweight` (Binance fapi weight buckets: IP 2400/min + funding 500/5min + data 1000/5min, Critical/Normal/Low priority, global 429/418 cooldown) → `SafeHTTPClient` (SSRF: URL validation + dial-time private-IP recheck + redirect re-validation + DNS fail-closed, `security/url_validator.go`). Proxying uniformly via the `HTTPS_PROXY` environment variable.

**Scanner family** (`market/breakout/`): 5-minute main loop; primary universe = top-20 volume + secondary universes (hot/gainer/loser boards, top 20 each); short universe = 24h top-50 gainers + hist_gainer history pool + near_high slow toppers (<5% below the 90-day high + 4h bearish divergence); nine-dimension scoring with grade lines 70/55/40 → strong/medium/weak; the **online tuner** evaluates cross-sectional rank IC (Spearman) per next-day UTC block, requires ≥30 blocks and |t|>3, and only emits research proposals that never publish directly (`shorttuner.go:75-97`, double switch).

**provider/ status**: vergex (go-rod headless through Cloudflare; the **sole source without fallback** for AI500/OI/NetFlow), coinank klines (active; its OpenAPI module is dead code), hyperliquid xyz assets (active), alpaca/twelvedata (frontend charts), openbb sidecar (optional), nofxos (deprecated, types reused only).

**store/**: 40+ tables (GORM AutoMigrate, no versioned schema; Postgres uses add-only ensureColumns). Core tables: `traders`/`trader_positions`/`decision_records` (+system prompt dedupe table)/`entry_assessments`/`gate_shadow_blocks`/`trader_equity_snapshots`/`trade_journal`/`ai_charges`/`factor_quant_state`/the US-stock paper family (`stock_paper_*` etc.). SQLite production DSN: `WAL + synchronous=NORMAL + busy_timeout=15000 + _txlock=immediate`, pool 4/4, one 300ms busy retry.

**Key data conventions (read before changing):**

- `funding_rate` is Binance's raw decimal (0.0001 = 0.01% per settlement); annualization uses the symbol's real settlement interval (fundingInfo table), not a fixed ×3/day; crowding uses `funding_annualized_pct`
- `premiumIndex.lastFundingRate` is a drifting forward-looking estimate; rollover detection uses the settled series (`funding_history.go`)
- OI USD delta = base delta × latest price (not notional subtraction); short-window OI build-up uses contract quantity; failure ≠ 0 (`FundingRateOK`/`OpenInterestOK` distinguish)
- US bStock uses the 1d bar as the volatility yardstick; missing 1d ATR fails closed; freshness follows NYSE sessions

---

## Frontend Architecture (web/)

**Skeleton**: `main.tsx` mounts `BrowserRouter` + sonner; but **`App.tsx` hand-rolls routing** (`useState(pathname)` + popstate/hashchange + `history.pushState`, `App.tsx:182-227`); react-router is only used by a few components' `useNavigate`; pages are all lazy-imported. Unmatched paths fall back to LandingPage. Auth: JWT in localStorage; 401s are handled centrally by the axios interceptor — clear token → `unauthorized` event → returnUrl → `/login`.

**Pages & data sources:**

| Page | Role / API |
|---|---|
| TraderDashboardPage | Trader dashboard (defaults to the running trader with max cycles); App-level SWR injects status/account/positions/decisions (15s); RiskStatusPanel 30s |
| StrategyStudioPage | Strategy editor; bare fetch `/api/strategies`; save with 15s AbortController + 409 optimistic lock `base_updated_at` |
| DataPage | Market/scanner tabs (crypto/hl/breakout/shortscan 🩸); trending connects to vergex directly from the browser and relays back into the backend cache |
| FundsPage | Funds overview `/api/funds/overview` (fundsledger); paginated daily ledger |
| ReviewPage | Trade journal / rule engine / AI review `/api/review/*` |
| ResearchHubPage | Hash-driven three tabs: factor-mining / paper-promotion / research-log (separate page files) |
| SettingsPage | Model/exchange config, netmeter traffic card (5s), embedded quality & code-review tabs |
| Others | FactorMiningPage, PaperPromotionPage, ResearchLogPage (pure static), CodeReviewHistoryPage, SystemQualityPage, CompetitionPage, AITradersPage, LandingPage |

**Data-fetching conventions**: SWR-first, **resilient backoff, never a permanent latch** — App-level account/positions enter a 60s heartbeat after 2 consecutive failures and restore 15s on any success (`App.tsx:304-345`); decisions use linear backoff. The axios `httpClient` singleton: injects Bearer on requests; on responses, system errors (401/5xx) are toasted and swallowed, only 4xx business errors are returned to callers (`web/src/lib/httpClient.ts:109-255`). Exceptions: StrategyStudio/some AuthContext calls bare-fetch around the unified handling (see Known Issues).

**Other**: zustand has only 2 stores (config aggregation/modal), everything else SWR+props; lightweight-charts for candlesticks, recharts for curves; i18n in three languages (en/zh/id, `translations.ts` 5347 lines) with a separate dictionary for the strategy editor; design system in `docs/design/TRADING_UI_DESIGN_SYSTEM.md` (CSS variable tokens, `index.css:13-98`); tests with vitest + testing-library (29 files, ~5800 lines); types in `types/` are hand-written mirrors of Go structs (no codegen).

**Build & deploy**: dev vite proxy `/api → localhost:8080` (`web/vite.config.ts:33`); production nginx container serves dist + `/api` reverse proxy (`docker/frontend/Dockerfile.frontend`, `railway/start.sh` generates nginx conf at runtime). **The Go backend serves no static files.**

---

## Research & Backtesting Infrastructure

| Directory | Role | Entry |
|---|---|---|
| `scripts/backtest/` | Unified **NautilusTrader 1.222.0 + QuantStats 0.0.86** backtests (`nt_engine.py`, default `--engine nautilus`, plus `nt-parity`; `--engine backtrader` kept read-only for historical frozen studies): signal extraction / entry comparison / exit backtests / ML labels / unified L+S margin portfolio with funding & borrow carry / LLM replay scoring; **reads only `decision_records` from `data/data.db`, exit params read live from strategies.config** | `.venv/bin/python -m scripts.backtest <sub>`; README.md is the authoritative spec |
| `scripts/factor_mining/` (50 files) | Nightly factor mining: genetic mutation + LLM proposals → frozen evaluation (K threshold t_req=max(2, Φ⁻¹(1−0.025/K))) → `quant_targets.json` trading weights | `run_nightly.sh` (launchd 02:30, `FM_US_MINING` gates the US line) |
| `scripts/promotion/` | Paper promotion: baseline registration, candidate/control pairs, daily report, fq_us S2 forward report (126 trading days, NW lag20, z≥1.65) | `python -m scripts.promotion.report` |
| `scripts/autoresearch/` | Self-evolving scoring research (collect → frozen dataset → K threshold → one-shot holdout) | `run_round.sh` |
| `cmd/llmreplay/` | Offline LLM replay (fixed samples, three arms baseline/noise/variant) | `go run ./cmd/llmreplay` |

**Three-stage incremental evaluation** (`INCREMENTAL_EVAL_DESIGN_2026-11-03.md`) status:

- **S1 offline screening**: tooling complete (dev/holdout split, noise arms, random-drop placebo, shadow gates); ledger S1=26
- **S2 preregistered online pairing**: two coexisting paths (promotion candidate/control pairs; fq_us single-trader forward test); ledger S2=4; **the designed incremental verdicts (harm_stop/extend) are not yet wired into report.py**
- **S3 holdout promotion**: design only — no tooling, no ledger rows

**Ledger**: `data/evolution/ledger.jsonl` append-only; `evolution_ledger.append_event` uses flock to make K computation and the write atomic; K accumulates per track (fq_crypto 9, fq_us 8, crypto_llm 6, us_llm 2).

**Methodology conventions**: preregistration docs at `docs/research/YYYY-MM-DD-<topic>-prereg.md` (33 files) — **commit before computing any returns/IC; parameters frozen after the run**; four-risk checks (lookahead/survivorship/label overlap/multiple testing) each have tooling and recorded lessons (e.g. the same-bar lookahead in `scripts/backtest/README.md:121`, CAGR +73.5%→+41.1%); research records are manually synced in three places: `docs/research/*.md` + `web/src/constants/researchLog.ts` + `docs/research/quant-research-log.html`.

---

## Key Design Principles

1. **Program derives, model doesn't declare**: wait_state/stage/blocking_factors/hard-gate verdicts are all program-derived; model claims are overwritten — datasets can't contradict themselves
2. **precompute-derived-data-for-llm**: gates, RR, TP menu, regime, micro-trend are all precomputed into signal JSON; prompts explicitly forbid recomputing from raw klines
3. **"The planned gate is the executed gate"**: stop_plan/rr_scan/TP menu/executor share one spec; model output is snapped back to program prices within 0.05% tolerance
4. **Fail-closed by default; every fail-open is recorded**: missing evidence ≠ pass (BTC_REGIME_UNKNOWN, VENDOR_DIVERGENCE_UNKNOWN always block); liquidity-class gates pass when data is missing but the behavior is in the vocabulary; every fail-open in the reconciliation/protection chain has comments and alerts
5. **Zero-token synthesis**: deterministic conclusions (all-candidates blocked, close-locked positions, opposing conflicts) are compressed or skip the LLM entirely
6. **One source of truth, two renderings**: GateStates are captured once during prompt building and reused for wait derivation, shadow records, and executor exemptions
7. **Single user + ownership checks**: the manager's in-memory table is never an authorization source; everything goes through `GetForUser`
8. **Backtests match live conventions**: backtests read the production DB, exit params come from live config, limit fills use the conservative 1-tick-cross convention

---

## Deployment Shapes

| Shape | Notes |
|---|---|
| **docker-compose** | Frontend nginx container (static + /api proxy to nofx:8080) + backend container; `.env` mounted + env_file; the DATA_ENCRYPTION_KEY generated by `start.sh` is written back to .env (persistent) |
| **Railway** | All-in-one container: starts nginx reverse-proxying 127.0.0.1:8081; `railway/start.sh` generates keys at runtime (**see Known Issues P1-1**) |
| **Local nohup** (current live) | kill / build / nohup as three separate steps; **must explicitly set `env HTTPS_PROXY=http://127.0.0.1:7890`** (the .env has no proxy vars); verify builds with `go version -m`; query data.db with `mode=ro` |

---

## Core Dependencies

### Backend (Go)

| Package | Purpose |
|---|---|
| `gin-gonic/gin` | HTTP framework |
| `gorm.io/gorm` + `gorm.io/driver/sqlite` (mattn CGO) | ORM and production SQLite driver (Postgres optional) |
| `adshao/go-binance/v2` | Binance futures client |
| `go-hyperliquid` / `bybit.go.api` / `gateapi-go` / `lighter-go` | Exchange SDKs |
| `golang-jwt/jwt/v5` | JWT auth |
| `sirupsen/logrus` (zerolog sits unused in go.mod) | Logging (daily rotation) |
| `joho/godotenv` | .env loading |
| `go-telegram-bot-api/v5` | Telegram |
| `go-rod/rod` | vergex headless relay |
| `ethereum/go-ethereum` | Wallet validation |

### Frontend (React)

| Package | Purpose |
|---|---|
| `react` 18 + `react-router-dom` (partially used) | UI framework |
| `swr` | Data fetching/polling |
| `axios` | HTTP client (httpClient singleton) |
| `zustand` | Light state (2 stores) |
| `lightweight-charts` | Candlestick charts |
| `recharts` | Equity/comparison/factor charts |
| `tailwindcss` + in-house ui primitives | Design system |
| `vitest` + `@testing-library/react` | Testing |

---

## Known Issues & Technical Debt (2026-11-15 deep review)

> Full list in that day's review report; recorded here by priority — update this section after fixing.

### P1 (correctness / security / governance)

1. **Railway keys not persistent** (`railway/start.sh:10-15`): when `RSA_PRIVATE_KEY`/`DATA_ENCRYPTION_KEY` are not set in platform env, they are randomly generated per boot and not persisted → after a restart every encrypted exchange API key fails to decrypt and traders silently fail to load. Contrast the root `start.sh:142-146`, which writes back to .env.
2. **indodax selectable for AI futures strategies** (`trader/auto_trader.go:476`): SetStopLoss/SetTakeProfit/OpenShort hard-error → an "open → unprotected → protection failure → emergency close" loop. Should reject adapters without a stop-loss API at construction time, like `factorQuantGuard`.
3. **The regime-tilt branch bypassed its own research process**: the four new behaviors (tilt/rolling_exit/vol_adaptive_stop/drawdown_scale) default on with no preregistration and no S1 ledger rows; deploying it modifies the rules under test mid-flight for the running S2 (fq-us-momentum-ls-v1). Before merging, either explicitly set all four switches false on S2 traders or run prereg→S1 on the fq_us track.
4. **The launchd nightly job runs an out-of-repo copy** (`~/Library/LaunchAgents/com.nofx.factor-mining.plist` points to `~/workspace/nofx-autoresearch`, no .git): the code producing live paper target weights is not under version control, and repo fixes never reach the nightly job. Point it at the main repo and print a code hash in reports.
5. **Frontend triple-track routing** (`web/src/App.tsx:182-227`): a mounted-but-unused BrowserRouter + hand-rolled routing + local `useNavigate`, with the route map duplicated in 4 places; `ChartWithOrders*.tsx` (836 lines) and `PageNotFound.tsx` are unreferenced dead code.

### P2 (design flaws, selected)

- Rate limiting collapses behind nginx: official deployments put the backend behind nginx, and `SetTrustedProxies(nil)` makes the whole site share one bucket (`api/server.go:47`)
- Every authenticated data request runs `LoadUserTradersFromStore` (holding loadMu + 3 DB queries); slow reloads serialize data endpoints (`api/server.go:695`)
- `decision_records` has cleanup functions with no callers; the table keeps growing (`store/decision.go:338`)
- Regime-skip synthetic-wait cycles still record a per-call AI charge (`trader/auto_trader_loop.go:230`)
- The prompt blocking-code vocabulary lags the gates' actual output (`DATA_STALE_*`, the NEG_EDGE family etc. undefined, `kernel/schema.go:312`)
- `buildAccountContext` type-asserts position maps directly; one malformed snapshot can panic and stop a trader (`trader/auto_trader_loop.go:669`)
- The limit-entry path lacks the market path's fresh-account re-admission / downsizing recheck (`trader/auto_trader_pending.go:230`)
- JWT in localStorage (XSS-stealable, `web/src/lib/…AuthContext.tsx:109`); three coexisting i18n mechanisms; some pages bare-fetch around the unified client
- S3 (promote/rollback) is design-only; S2 incremental verdicts are not wired into promotion; incremental-eval baseline registration never ran
- The short tuner rewrites its journal file O(N) every 30min (`market/breakout/shorttuner.go:624`)

### P3 (summary)

Assorted dead code (store/driver.go, provider/coinank apikey, nofxos network client, the unregistered hook extension point); `crypto.go`'s claimed replay protection is actually just a 5-minute time window; two coexisting Binance HTTP stacks with inaccurate comments (`market/api_client.go` has no SSRF protection); `docs/architecture/STRATEGY_MODULE.md` is severely outdated (references the nonexistent `decision/` package) — rewrite it or mark it obsolete.

---

## Related Documents

| Document | Content | Status |
|---|---|---|
| [STRATEGY_MODULE.md](STRATEGY_MODULE.md) | Strategy module (old decision-engine description) | **Severely outdated**, historical reference only |
| [INCREMENTAL_EVAL_DESIGN_2026-11-03.md](INCREMENTAL_EVAL_DESIGN_2026-11-03.md) | Three-stage incremental evaluation design | S1 done / S2 partial / S3 design |
| [FACTOR_QUANT_TRADER_DESIGN_2026-11-01.md](FACTOR_QUANT_TRADER_DESIGN_2026-11-01.md) | factor_quant trader design | Mostly implemented |
| [FQ_US_MOMENTUM_S2_DESIGN_2026-11-05.md](FQ_US_MOMENTUM_S2_DESIGN_2026-11-05.md) | fq_us momentum S2 design | Running |
| [LLM_EVOLUTION_LOOP_DESIGN_2026-11-02.md](LLM_EVOLUTION_LOOP_DESIGN_2026-11-02.md) | LLM evolution loop | Design draft |
| [QUANT_SYSTEM_REVIEW_2026-11-14.md](QUANT_SYSTEM_REVIEW_2026-11-14.md) | Previous day's full quant review | Conclusion: no urgent defects |
| [../api/API_REFERENCE.md](../api/API_REFERENCE.md) | HTTP API reference | Maintained alongside route_registry |
| [../design/TRADING_UI_DESIGN_SYSTEM.md](../design/TRADING_UI_DESIGN_SYSTEM.md) | Frontend design system | Current |
| [../research/paper-promotion.md](../research/paper-promotion.md) | Paper/strategy promotion process | Current |
| `scripts/backtest/README.md` | Authoritative backtest spec | Current |

## Quick Links

- [Strategy Module (outdated)](STRATEGY_MODULE.md) / [Incremental Eval](INCREMENTAL_EVAL_DESIGN_2026-11-03.md) / [Getting Started](../getting-started/README.md) / [FAQ](../faq/README.md)

---

[← Back to Documentation](../README.md)
