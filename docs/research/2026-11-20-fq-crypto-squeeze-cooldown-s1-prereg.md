# Pre-Registration: `fq_crypto` Short-Squeeze Defense & Stop-Loss Cooldown (S1, K=16)

- **Date**: 2026-11-20
- **Track**: `fq_crypto`
- **Planned Ledger K**: `K = 16` (1 candidate arm; prior `fq_crypto` ledger has `K = 1..15`)
- **Status**: FROZEN before computing any candidate return, Sharpe, drawdown, or placebo statistic.
- **Engine**: `scripts/backtest/nt_engine.py` (**NautilusTrader 1.222.0** + **QuantStats 0.0.86**), native `PortfolioRiskOverlay` (`STOP_MARKET`, cooldown, drawdown scale, rolling exit) + `CarryFundingModule` (8h perpetual funding).

---

## 1. Motivation & Empirical Problem Statement

Audit of the paper-trading `fq_crypto` trader (`ae795edd...`, `horizon=3`, `leverage=3`, `long_stop_pct=0.20`, `short_stop_pct=0.20`) and prior offline baseline diagnostics (`docs/research/2026-11-18-ltw-crypto-factors-report.md`) revealed three concrete loss mechanisms on the short leg:

1. **Repeat short-squeeze stop-outs**: `11/60` closed paper trades hit `stop_loss` (`-$759.87`, `0%` win rate), wiping out `93.6%` of the `+$811.58` earned by normal rebalances (`49` trades, `57.1%` win rate). Single symbols repeatedly re-entered and stopped out on consecutive rebalances (`ICNTUSDT` short stopped 3x for `-$194.06`; `ERAUSDT` short stopped 2x for `-$193.20`).
2. **Short-leg volatility convexity in chop/default**: `scripts/factor_mining/quant_targets.py` weights shorts by `1/σ`, whereas `trader/auto_trader_factor_quant_regime.go` only applies `1/σ²` inverse-variance weighting inside bull/bear `regime_tilt`. Average annualized volatility of the short leg (`136.6%`) is `2.46x` that of the long leg (`55.5%`).
3. **Fast 3-day squeeze acceleration**: The current `ret7 >= 90th pct` filter misses rapid 1–3 day short squeezes and sudden volatility explosions (`vol5 / vol30 >= 2.0`).

---

## 2. Frozen Data, Splits & Cost Model

- **Dataset**: Local `panel.load_panels("crypto", tier="all")` for causal signals, eligibility mask, and historical 8h funding rates (`panels.fields["funding"]`), paired with NAS MariaDB `crypto_klines` (`1d` OHLCV, `2020-01-01 .. 2026-11-15`) via `scripts/backtest/nas_klines.py`.
- **Frozen Split Boundaries** (identical to `ltw_crypto_study.py` / `panel.make_splits(..., h=3)`):
  - **Val**: `splits["val"]` (`~2025-05-19 .. 2026-05-18`, ~365 days)
  - **Holdout**: `splits["holdout"]` (`~2026-05-19 .. 2026-11-14`, ~179 days)
  - Each split runs as a clean independent NautilusTrader portfolio resetting capital to `$100,000`.
- **Frozen Signal**: Frozen 7-factor `h=3` ensemble (`data/factor_mining/crypto/ensemble.json` + `zoo.json`, exact `ensemble.ensemble_scores`). Zero factor re-mining or parameter tuning.
- **Execution & Costs**:
  - `execution = "next_open"` (target decided at bar `t` close, filled at bar `t+1` open ± slippage).
  - `commission = 0.0005` (5 bp per fill), `slippage = 0.0003` (3 bp per fill).
  - `carry = CarryConfig(market="crypto", funding_rates=panels.fields["funding"], default_funding_rate=0.0001, funding_multiplier=3.0)` (exact 8h perpetual funding).
  - Rebalance cadence: every `h = 3` calendar days (`dates[::3]`).
  - Protective stops: `long_stop_pct = 0.20`, `short_stop_pct = 0.20` via native NautilusTrader `STOP_MARKET` orders.
  - Annualization: `periods_per_year = 365`, `rf = 0.0`.

---

## 3. Baseline (`B`) vs Single Pre-Registered Candidate (`C1`, `K=16`)

### Baseline `B` (`ls_invvol_protected_v1_s20`)
- Exact production `quant_targets.build_targets("crypto", ...)` (`ls_invvol_protected_v1`: top/bottom 20%, short exclusions `ret7 >= 90th pct` and `listing < 90d`, `1/σ` capped at `0.10` per name, `0.50` long / `0.50` short gross).
- Native NautilusTrader `PortfolioRiskOverlay(long_stop_pct=0.20, short_stop_pct=0.20)`.

### Candidate `C1` (`fq-crypto-squeeze-cooldown-invvar-v1`, `K = 16`)
Changes ONLY short-leg squeeze protection, short-leg variance sizing, and post-stop cooldown (all strictly causal `<= t`):

1. **Universal `1/σ²` short-leg inverse-variance weighting**: Replace `1/σ_30` with `1/σ_30²` on the short leg across all market regimes (still water-fill normalized to `short_gross = 0.50`, `max_name_weight = 0.10`). Long leg stays `1/σ_30`.
2. **Short-term squeeze guard on short candidates**: In addition to `ret7 >= 90th pct` and `listing < 90d`, exclude any short candidate where:
   - `ret3(t) = close(t)/close(t-3) - 1 >= cross_sectional_90th_pct(ret3(t))`, OR
   - `vol5(t) / vol30(t) >= 2.0` (where `vol5` is 5-day sample std of daily returns, `min_periods=5`).
   - Fail-safe: if all short candidates would be excluded on a date, fall back to the baseline exclusion set.
3. **Same-side stop-loss cooldown (`10` calendar days)**: When a native `STOP_MARKET` order fills for `(symbol, side)` on day `d`, block re-entry of `(symbol, side)` on any rebalance strictly before `d + 10 days`, and redistribute the freed leg weight proportionally across surviving same-leg names up to `max_name_weight = 0.10` (preserving gross exposure).

---

## 4. Primary Metric, Placebo Design & S1 Decision Rule

- **Primary Metric**: Paired daily return difference `Δ_t = r_{C1,t} - r_{B,t}` over concatenated `val + holdout` (~542 daily observations), evaluated by paired t/z-statistic `z = mean(Δ) / (sd(Δ) / sqrt(N))`, plus separate `val` mean `Δ_val` and `holdout` mean `Δ_ho`.
- **Secondary Diagnostics** (reported, not optimized): QuantStats net Sharpe, total return, MaxDD, Calmar, stop-loss count, repeat-stop count, total commission, total funding paid, mean gross exposure.
- **Placebo Control (`n = 200`, `SEED = 20261120`)**:
  - Exposure- and turnover-matched random placebo: on each rebalance date, randomly drop the exact same number of short candidates as `C1`'s extra squeeze filter (`ret3`/`vol5/vol30`), apply randomly permuted `1/σ²` variance weights (shuffling the `σ²` vector across the short leg), and apply random 10-day cooldown bans to the same number of random `(symbol, side)` pairs stopped in `C1`.
  - Computes `placebo_pct = fraction of 200 draws with mean(Δ_placebo) < mean(Δ_C1)`.
- **Pre-Registered Decision Gate** (per `docs/architecture/INCREMENTAL_EVAL_DESIGN_2026-11-03.md`, `K = 16`):
  - `z_16 = incremental_filter.z_threshold(16)` (`≈ 2.75`).
  - **`strong`**: `z >= z_16` AND `Δ_val > 0` AND `Δ_ho > 0` AND `placebo_pct >= 0.90`.
  - **`screen_pass`**: `1.0 <= z < z_16` AND `Δ_val > 0` AND `Δ_ho > 0` AND `placebo_pct >= 0.90`.
  - **`fewer_trades`**: `z >= 1.0` AND `Δ_val > 0` AND `Δ_ho > 0` AND `placebo_pct < 0.90`.
  - **`fail`**: otherwise (`z < 1.0` or `Δ_val <= 0` or `Δ_ho <= 0`).

---

## 5. Companion Non-K Descriptive Diagnostic: PR #121 Four-Switch Ablation

Because PR #121 enabled 4 switches by default in live/paper `fq_crypto` (`regime_tilt`, `rolling_exit`, `vol_adaptive_stop`, `drawdown_scale`), the study also runs a **descriptive ablation table (non-K, not entered into the evolution ledger as a candidate)** on the exact same `val` + `holdout` NautilusTrader + 8h funding setup:
- `B0` (no stops, all 4 switches OFF)
- `B` (`B0 + S20` fixed 20% stops)
- Single-switch additions onto `B`: `+regime_tilt`, `+rolling_exit`, `+vol_adaptive_stop`, `+drawdown_scale`
- `ALL4` (exact current live paper config)
- Leave-one-out from `ALL4`: `ALL4 - rolling_exit`, `ALL4 - regime_tilt`, `ALL4 - vol_adaptive_stop`, `ALL4 - drawdown_scale`

---

## 6. Known Biases & Governance Commitments

- **Already-inspected windows**: `val` and `holdout` have been inspected in prior `fq_crypto` studies (`K=1..15`). Any `screen_pass` requires prospective S2 online verification before live promotion.
- **No tuning**: No threshold (`10d`, `2.0x`, `90th pct`, `1/σ²`) may be adjusted after running this script.
- **Ledger disclosure**: `K = 16` will be appended to `data/evolution/ledger.jsonl` regardless of pass or fail, and recorded in `web/src/constants/researchLog.ts` and `docs/research/quant-research-log.html`.
