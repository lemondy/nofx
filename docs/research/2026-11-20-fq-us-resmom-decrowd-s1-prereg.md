# Pre-Registration: `fq_us` Residual Momentum & Correlation De-Crowding (S1, K=12..13)

- **Date**: 2026-11-20
- **Track**: `fq_us`
- **Planned Ledger K**: `K = 12` (`H1`: Residual Momentum) and `K = 13` (`H2`: Correlation De-Crowding); prior `fq_us` ledger has `K = 1..11`.
- **Status**: FROZEN before computing any candidate return, Sharpe, drawdown, or placebo statistic.
- **Engine**: `scripts/backtest/nt_engine.py` (**NautilusTrader 1.222.0** + **QuantStats 0.0.86**) with `CarryConfig(market="us")`.

---

## 1. Motivation & Empirical Problem Statement

Audit of the paper-trading US Factor Quant portfolio (`1025e717...`, `mom_ls_invvol_v1`) and historical `fq_us` stress tests (`docs/research/2026-11-05-fq-us-momentum-stress-report.md`) revealed two structural flaws in raw 12-1 cross-sectional momentum:

1. **Time-varying market-Beta exposure & crash vulnerability**: Raw 12-1 total-return momentum implicitly loads on recent high-Beta winners in bull markets and low-Beta defensives after bear markets, causing severe momentum crashes during market rebounds (e.g., 2009 `-29%`). Blitz, Huij & Martens (2011, *Journal of Empirical Finance*) show that ranking on market-regression idiosyncratic residuals (`Residual Momentum`) halves crash risk and roughly doubles risk-adjusted return.
2. **Severe sector/cluster crowding under `1/σ` weighting**: Live `data/factor_mining/us/quant_targets.json` holds 13 longs concentrated in AI-memory/optical hardware (`MU`, `SNDK`, `LITE`, `AAOI`, `AMD`, `MRVL`, `WDC`, `STX`) with 60-day average pairwise correlation **`0.644`** (`48.7%` of pairs `> 0.70`, `N_eff ≈ 1.5`), and shorts concentrated in crypto-proxies (`BMNR`, `COIN`, `MSTR`, `HOOD`, corr `0.81–0.90`). Inverse-volatility weighting treats correlated clones as independent bets, amplifying single-theme drawdowns.

---

## 2. Frozen Datasets, Splits & Cost Model

All datasets reuse the exact frozen snapshots verified in `scripts/backtest/fq_us_volscale_study.py` (no fetching, no writes):

- **Primary S1 Dataset — `D3` (S&P 500 Broad, 498 stocks, `~2012..2026`)**:
  - `data/factor_mining/us_broad/daily` (`EXPECTED_SHA256 = 2e621c842446cb82ddb1f6dd8744b333bacd8817f0e6ef4ff39543a8f9fc4814`)
  - Benchmark: `data/factor_mining/us_broad/benchmark/SPY.json`
  - Constituents: `data/factor_mining/us_broad/constituents_20261105.json`
  - Split into two chronological equal halves (`Half 1` / `Half 2`) by 20-trading-day rebalance periods.
- **Secondary Deployable Check — `D2` (bStock 90, `~2019..2026`)**:
  - `data/factor_mining/us_momentum/snapshot_20261105/daily` (`EXPECTED_SHA256 = 5bd711af9dbeef6d2131005aea89a5fc65897e3d47485145bd3868448d3e8042`)
  - Reported as deployable-universe validation (not used for threshold tuning).
- **Secondary Crash Stress Check — `D1` (2008–2009 GFC Momentum Crash, S&P 500)**:
  - `data/factor_mining/us_stress/daily` (`EXPECTED_SHA256 = 46ec51b880a7a8cc724c71be3650f19e6d4e1880e3c05fc0710c4bfdef44a256`)
  - Reported to verify crash-period behavior (`2008-01..2010-12`).
- **Execution & Cost Model (Unified NautilusTrader `nt_engine.run_portfolio`)**:
  - `market = "us"`, `initial_capital = 100_000.0`, `execution = "close"` (matching frozen `fq_us_momentum_study` / `fq_us_volscale_study` rebalance clock), `rebalance_trading_days = 20`.
  - `commission = 0.0010` (10 bp per fill — exact midpoint of frozen `13 bp` long / `7 bp` short), `slippage = 0.0`.
  - `carry = CarryConfig(market="us", us_short_borrow_annual=0.005, us_margin_finance_annual=0.05)` (50 bp/yr short borrow, 5% margin financing on excess long over equity).
  - Annualization: `periods_per_year = 252`, `rf = 0.0`.
- **Eligibility Rules (identical across `B`, `H1`, `H2`)**:
  - `>= 253` valid trading bars up to `t`, `close(t) >= $5.0`, 20-day median dollar volume `>= $10,000,000`, finite 30-day sample volatility `σ_30(t) > 0`, minimum `15` eligible stocks per rebalance.

---

## 3. Baseline (`B`) and Pre-Registered Candidates (`H1` `K=12`, `H2` `K=13`)

### Baseline `B` (`mom_ls_invvol_v1`)
- Signal: `close[t-21] / close[t-252] - 1` (12-1 total return).
- Selection: top `floor(0.2 * N)` longs, bottom `floor(0.2 * N)` shorts.
- Weighting: `_capped_invvol(vol30, gross=0.5, cap=0.10)` on each leg (`1.0` total gross).

### Candidate `H1` (`fq-us-residual-momentum-v1`, `K = 12`) — Blitz et al. (2011) Residual Momentum
- **Signal**: On each rebalance date `t`, using strictly causal daily simple returns over `[t-251, t]` (252 trading days) for each eligible stock `i` and `SPY`:
  - Estimate OLS market regression $r_{i,s} = \hat{\alpha}_i + \hat{\beta}_i r_{\text{SPY}, s} + \hat{\varepsilon}_{i,s}$ over all valid paired days in `[t-251, t]` (requiring `>= 126` valid return pairs; stocks failing this are excluded from `H1` on date `t`).
  - Form 12-1 standardized residual momentum over the 11-month window `[t-251, t-21]` (skipping the last 21 trading days):
    $$\text{ResMom}_i(t) = \frac{\sum_{s \in [t-251, t-21]} \hat{\varepsilon}_{i,s}}{\text{sd}(\hat{\varepsilon}_{i, [t-251, t-21]}) \cdot \sqrt{M_i}}$$
    where $M_i$ is the number of valid residual days in `[t-251, t-21]`.
- **Selection & Weighting**: Identical to `B` (top/bottom 20%, `_capped_invvol(vol30, gross=0.5, cap=0.10)`). Only the ranking signal changes.
- **Placebo Control (`n = 200`, `SEED = 20261120`)**:
  - At each rebalance date `t`, randomly permute the vector of estimated market betas $\{\hat{\beta}_i\}$ across eligible stocks, recompute pseudo-residuals $r_{i,s} - \hat{\beta}_{\pi(i)} r_{\text{SPY}, s}$ (demeaned over the estimation window), and rank by the resulting pseudo-ResMom. Tests whether removing stock `i`'s *actual* SPY beta beats subtracting a random stock's beta.

### Candidate `H2` (`fq-us-decrowd-corr-invvol-v1`, `K = 13`) — Correlation-Penalized De-Crowding
- **Signal & Selection**: 100% identical to `B` (`close[t-21] / close[t-252] - 1`, exact same long and short names).
- **Weighting**: Using strictly causal 60-day daily simple returns ending at `t` (`[t-59, t]`, `min_periods=30`), compute the pairwise correlation matrix among selected stocks within each leg (`L` for longs, `S` for shorts). For each stock `i` in leg `L`:
  $$\bar{\rho}^+_{60,i} = \frac{1}{|L| - 1} \sum_{j \in L,\, j \neq i} \max\!\bigl(0,\; \rho_{60}(i, j)\bigr)$$
  (if `|L| <= 1` or undefined, $\bar{\rho}^+_{60,i} = 1.0$).
  Replace effective risk denominator $\sigma_{30,i}$ with:
  $$\tilde{\sigma}_i = \sigma_{30,i} \cdot \max\!\bigl(0.25,\; \bar{\rho}^+_{60,i}\bigr)$$
  and feed $\tilde{\sigma}$ into the exact same `_capped_invvol(..., gross=0.5, cap=0.10)` water-fill allocator.
- **Placebo Control (`n = 200`, `SEED = 20261121`)**:
  - At each rebalance date `t`, randomly permute the penalty vector $\{\max(0.25, \bar{\rho}^+_{60,i})\}$ across the selected names within each leg. Matches exact weight dispersion and turnover while destroying true cluster alignment.

### Secondary Exploratory Diagnostics (Non-K, Reported Only)
- Combined `H1 + H2` (Residual Momentum + Correlation De-Crowding)
- Interaction with frozen Volscale (`k_t` from `fq_us_volscale_study.py`) on `D3`, `D2`, `D1`
- Portfolio crowding metrics: mean pairwise 60d leg correlation and effective number of independent bets $N_{\text{eff}} = 1 / (\mathbf{w}^\top \mathbf{C} \mathbf{w})$ (normalized within each leg).

---

## 4. Primary S1 Gate & Decision Rule

Evaluated independently for `H1 (K=12)` and `H2 (K=13)` on primary dataset **D3 (S&P 500 498, 2012–2026)**:
- **Paired Period Return Difference**: Over all complete 20-trading-day rebalance periods $p = 1, \dots, P$:
  $$\Delta_p = R_{\text{cand}, p} - R_{B, p}, \qquad z = \frac{\bar{\Delta}}{\text{sd}(\Delta) / \sqrt{P}}$$
  and split chronologically at `half = P // 2` into `Δ_half1` and `Δ_half2`.
- **Gate** (per `docs/architecture/INCREMENTAL_EVAL_DESIGN_2026-11-03.md`):
  - `z_K = incremental_filter.z_threshold(K)` (`z_12 ≈ 2.66`, `z_13 ≈ 2.68`).
  - **`strong`**: `z >= z_K` AND `Δ_half1 > 0` AND `Δ_half2 > 0` AND `placebo_pct >= 0.90`.
  - **`screen_pass`**: `1.0 <= z < z_K` AND `Δ_half1 > 0` AND `Δ_half2 > 0` AND `placebo_pct >= 0.90`.
  - **`fewer_trades`**: `z >= 1.0` AND `Δ_half1 > 0` AND `Δ_half2 > 0` AND `placebo_pct < 0.90`.
  - **`fail`**: otherwise.
- **Alternative Sharpe / Drawdown Risk-Reduction Clause**: Because Residual Momentum and De-Crowding primarily target variance/tail-risk reduction (where mean return Δ may be modest while Sharpe Δ and MaxDD improvement are large), we ALSO pre-register **paired daily Sharpe-utility / Jobson-Korkie-Memmel z-stat** and **MaxDD reduction** as a secondary diagnostic; however, the **ledger `verdict`** strictly follows the primary `INCREMENTAL_EVAL_DESIGN` mean-return paired-z gate above (no post-hoc goalpost moving).

---

## 5. Known Biases & Governance Commitments

- **Survivorship bias**: `D3` (current S&P 500 constituents) and `D2` (bStock 90) have survivorship bias; however, paired comparison `C - B` on the exact same universe cancels first-order survivorship level shift.
- **Already-inspected data**: `D1`, `D2`, `D3` were inspected in prior `fq_us` studies (`K=1..11`). Passing S1 only qualifies for prospective S2 online testing and MUST NOT alter the frozen active S2 pair (`1025e717` / `490f8b28`).
- **Ledger disclosure**: Both `K = 12` (`H1`) and `K = 13` (`H2`) will be appended to `data/evolution/ledger.jsonl` regardless of pass or fail, and logged in `web/src/constants/researchLog.ts` and `docs/research/quant-research-log.html`.
