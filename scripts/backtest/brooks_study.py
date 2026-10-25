#!/usr/bin/env python3
"""PRE-REGISTRATION (2026-10-23): Brooks signal study, version 1.

NO orders, stops, positions, funding or strategy P&L. Only completed-bar labels.
The specification below is frozen before examining any forward-return results.

Universe: all valid local daily files (US SPY excluded from the stock-file pool,
reported separately); crypto daily uses the existing causal 60-day eligibility /
top-60 trailing-20-day turnover mask. No re-fetching. 1h primary universe: six-
column files starting by 2022-12-31 AND spanning >=730 days; later-listed crypto
and tokenized instruments have separate coverage reports, no pooled inference.
All tokenized instruments must also have >=730 days to qualify. Old non-six-
column hourly files are ignored. No filling, resampling or synthetic candles.

Default BrooksParams only; strength bins are FIXED thirds of normalized score:
low <1/3, medium [1/3,2/3), high >=2/3. strength_ready is required everywhere.
H2/L2 are known at TRIGGER close t, their signal attributes are from t-1.
Reversal-minimum signals are known at t. F4 is delayed and is NOT in quality.
Crypto sides +1/-1; US +1 is long, -1 ONLY evidence for avoidance, never a short
recommendation. Every signed return is sign*log(close[t+k]/close[t]). ATR units
are signed log return/(ATR[t]/close[t]); NOT a future ATR or a price-unit return.
Daily k=(1,3,5,10,20), hourly k=(1,3,6,12,24). Cost-adjusted log-return proxy
subtracts 2*(0.0005+0.0003) crypto or 2*0.0005 US; descriptive, no fill model.

Split by signal UTC date: train through 2023-12-31, validation calendar 2024,
holdout from 2025-01-01. Purge EACH label whose endpoint crosses its split.
Daily date is session label; hourly date is UTC open date (known at bar close).
Feature caches may include later causal features; discovery labels NEVER do.
Discovery writes a hash-checked freeze file BEFORE the one-shot holdout phase.

T0: each k, drift vs zero; P(return>0) vs .5 (zero returns are not positive).
T1: for each H2/L2 and reversal minimum, each side, k and aligned/counter/
uncertain always-in state: signed mean minus same-symbol unconditional drift;
also aligned minus counter and aligned minus uncertain. Failure to reject is
NOT evidence of equivalence or of 'no edge'.
T2: N=5 daily/12 hourly. In explicit high-score always-in states, opposite-side
reversal-minimum attempts: P(next N highs/lows strictly exceed the trailing
20-bar high/low INCLUDING t). This is a disclosed proxy for trend resumption,
not a stopped-out reversal trade. Each direction: probability vs .8 AND excess
over same-symbol random-date probability. Range: AI[t-1]=AI[t]=0, previous 3
bars each overlap >=.5 and >=1 inside bar, current close outside their range.
P(any next-N close returns INSIDE that frozen range), each breakout direction,
vs .8 AND vs baseline of same-direction 3-bar close breakouts at other dates.
T3: explicit AI in scored direction, each fixed strength bin / side / k:
signed return and positive-return probability vs unconditional symbol baseline;
also high-minus-low for both outcomes.
T4: H2/L2 a=pattern; b=a & aligned AI; c=b & signal quality>=Q. Each side/k:
a,b,c minus random-date baseline and b-a,c-a,c-b. Random d distribution is
included for EVERY estimate, with exactly the observed per-symbol counts.
ONE global Q chosen on TRAIN ONLY from (1,2,3,4): maximize equal-market mean
ATR excess of c over b at primary k=5 daily/12 hourly, requiring >=100 events
and >=30 dates per market/side-pooled cell. If none eligible, Q=2. All four
values on train AND validation are output as descriptive sensitivity results;
none of those search statistics gets a confirmatory p-value. No OOS tuning.
T5: separately climax end and 20-bar EMA-gap first touch, each direction:
P(any next 1..10 close has |close-EMA20|<=.5*ATR), excluding t; excess over
random-date baseline. This tests the requested EMA-return proxy, NOT duration
>=10 bars or two corrective legs. Forward-close return at k=10 is also shown
as descriptive context for the SAME probability sample, not another test.
T6: candidate=H2/L2 OR directional reversal minimum (deduplicated by side/t;
H2 takes priority for signal-bar attributes). Each filter separately: doji,
excessive overlap, opposite micro-channel at t. Each side/k: banned and allowed
minus random-date baseline, and banned-minus-allowed. No composite search.

Inference: observation-weighted means, dates contain all assets/hours. CR1
date-cluster delta-method SE, normal two-sided p; Monday-week cluster SE/p as
robustness. NO IID candle t-tests. Paired group/baseline covariance retained.
2000 percentile bootstrap draws of WHOLE dates, recomputing mean ratios; same
draws for paired arms. Baseline has observed-signal symbol weights, fixed for
this conditional-universe estimand. Other-date controls: 500 independent
within-symbol circular shifts of each event mask over same-split eligible
labels (>=24-hour offset on 1h), with a common offset for contrast arms.
Counts and symbol identities are EXACT, outcomes/time structure preserved
within each symbol, cross-symbol date alignment is not preserved by controls.
FFT circular correlations make these 500 exact shifted-mask means efficient.
Report midrank percentile of the observed statistic, plus control moments.

Every row above at 3 markets x 3 splits is a PLANNED scalar test, including
empty cells (p=1 retained in BH denominator). ATR/cost/context means, weekly
checks, bootstrap CIs and controls do not add primary hypotheses. BH across
ALL planned rows; discovery-only BH separately frozen before OOS. Insufficient
= arm n<30 or <20 date clusters; estimates still shown, primary p=1. All p are
two-sided; expected direction must separately match. No post-hoc added tests.
No exploratory outcome tests. T0 has no pattern and no shifted-mask test.

Promotion rule frozen ex ante: only T4 b or c at primary k may enter order-
backtest consideration; BOTH train and validation must have positive baseline
excess, positive cost-adjusted mean, discovery q<.05 and week p<.05, >=100
events and >=30 dates. Only those nominated BEFORE OOS may be confirmed, using
positive OOS excess/cost mean, global q<.05, week p<.05, >=100 / >=30. US short
side never eligible. Additional filters require their own future pre-registered
test; no OOS-driven picking of horizons or rule subsets.

Usage (offline; all writes restricted to the two /private/tmp directories):
  python -m scripts.backtest.brooks_study --phase discovery
  python -m scripts.backtest.brooks_study --phase holdout
  python -m scripts.backtest.brooks_study --phase report
"""

from __future__ import annotations

import argparse
from dataclasses import asdict, dataclass
import hashlib
import json
import math
from pathlib import Path
import time

import numpy as np
import pandas as pd

from scripts.backtest.brooks import BrooksParams, compute_features


OUTPUT = Path('/private/tmp/claude-501/brooks/study')
CACHE = Path('/private/tmp/claude-501/brooks/features')
DAILY_ROOT = Path('/Users/zhangyun/workspace/nofx/data/factor_mining')
HOURLY_ROOT = Path('/private/tmp/claude-501/brooks/kl1h')
HORIZONS = {'crypto_daily': (1, 3, 5, 10, 20),
            'crypto_1h': (1, 3, 6, 12, 24), 'us_daily': (1, 3, 5, 10, 20)}
MARKETS = tuple(HORIZONS)
SPLITS = ('train', 'validation', 'holdout')
BOUNDS = {'train': (pd.Timestamp.min, pd.Timestamp('2024-01-01')),
          'validation': (pd.Timestamp('2024-01-01'), pd.Timestamp('2025-01-01')),
          'holdout': (pd.Timestamp('2025-01-01'), pd.Timestamp.max)}
TOKENIZED = frozenset('BZ CL KORU MSTR MU SKHYNIX SKHY SNDK SNXX SOXL SOXS SPCX SPY TSLA XAG XAU'.split())
KEEP = ['atr', 'ema20', 'strength_ready', 'always_in', 'inside', 'overlap_ratio',
        'h2', 'l2', 'is_doji', 'micro_channel_bull', 'micro_channel_bear']
KEEP += [f'{p}_{s}{suffix}' for p, suffixes in (
    ('reversal', ('_minimum', '_quality_score', '_excessive_overlap')),
    ('strength', ('_normalized',)), ('climax', ('_end',)),
    ('ema_gap', ('_first_touch',))) for s in ('bull', 'bear') for suffix in suffixes]


def stable_seed(key: str) -> int:
    return int.from_bytes(hashlib.sha256(key.encode()).digest()[:8], 'little')


def split_mask(index, split, endpoint=None):
    """A row AND its known label endpoint must be within the same fixed split."""
    index = pd.DatetimeIndex(index)
    lo, hi = BOUNDS[split]
    mask = (index >= lo) & (index < hi)
    if endpoint is not None:
        end = pd.DatetimeIndex(endpoint)
        mask &= end.notna() & (end >= lo) & (end < hi)
    return np.asarray(mask)


def forward_labels(frame, horizons, atr):
    """Labels only; no signal/feature is computed here. Tail k rows stay NaN."""
    out = pd.DataFrame(index=frame.index)
    c = frame.close.astype(float)
    scale = pd.Series(np.asarray(atr), index=frame.index) / c
    time_values = pd.Series(frame.index, index=frame.index)
    for k in horizons:
        if not isinstance(k, int) or k < 1:
            raise ValueError('horizon must be a positive integer')
        out[f'return_{k}'] = np.log(c.shift(-k) / c)
        out[f'atr_{k}'] = out[f'return_{k}'] / scale.where(scale > 0)
        out[f'end_{k}'] = time_values.shift(-k)
    return out


def future_extreme(series, n, kind):
    """Exactly t+1..t+n, requiring ALL n future bars, no partial tail window."""
    rev = series.iloc[::-1].rolling(n, min_periods=n)
    value = rev.max() if kind == 'max' else rev.min()
    return value.iloc[::-1].shift(-1)


def event_labels(frame, features, kind, sign, n):
    if kind == 'new_extreme':
        prior = frame.high.rolling(20).max() if sign > 0 else frame.low.rolling(20).min()
        future = future_extreme(frame.high if sign > 0 else frame.low, n,
                                'max' if sign > 0 else 'min')
        y = (sign * (future - prior) > 0).astype(float)
        return y.where(future.notna() & prior.notna())
    if kind == 'range_return':
        hi = frame.high.shift(1).rolling(3).max()
        lo = frame.low.shift(1).rolling(3).min()
        result = pd.Series(False, index=frame.index)
        for lag in range(1, n + 1):
            result |= frame.close.shift(-lag).between(lo, hi)
        return result.astype(float).where(frame.close.shift(-n).notna() & lo.notna())
    if kind == 'ema_return':
        near = (frame.close - features.ema20).abs() <= .5 * features.atr
        return future_extreme(near.astype(float), n, 'max')
    raise ValueError(kind)


def cluster_mean_se(values, dates):
    """CR1 SE of an intercept-only mean, clusters are entire calendar dates."""
    y = np.asarray(values, dtype=float)
    d = np.asarray(dates)
    keep = np.isfinite(y)
    y, d = y[keep], d[keep]
    if len(y) == 0:
        return float('nan')
    _, inv = np.unique(d, return_inverse=True)
    g = inv.max() + 1
    if g < 2:
        return float('nan')
    scores = np.bincount(inv, weights=y - y.mean()) / len(y)
    return float(np.sqrt(g / (g - 1) * (scores @ scores)))


def bh_adjust(pvalues):
    """BH adjusted q-values; NaN reserves a planned slot with p=1."""
    p = np.asarray(pvalues, dtype=float)
    p = np.where(np.isfinite(p), p, 1.)
    if np.any((p < 0) | (p > 1)):
        raise ValueError('p-values must be in [0,1]')
    if not len(p):
        return p.copy()
    order = np.argsort(p, kind='stable')
    adjusted = np.minimum.accumulate((p[order] * len(p) / np.arange(1, len(p) + 1))[::-1])[::-1]
    q = np.empty(len(p))
    q[order] = np.minimum(adjusted, 1.)
    return q


def normal_p(effect, se):
    if not np.isfinite(se):
        return float('nan')
    if se == 0:
        return 1. if abs(effect) < 1e-14 else 0.
    return math.erfc(abs(effect / se) / math.sqrt(2))


def shifted_control(values, group_a, group_b=None, *, repeats=500, seed=0,
                    min_shift=1, symbol=None, dates=None, reference=0.):
    """Exact within-symbol circular-mask shifts, efficiently evaluated by FFT.

    group arrays are nonnegative weights, not necessarily Boolean. Shift both
    arms identically. No independent-bar resampling or label permutation.
    """
    y, a = np.asarray(values, float), np.asarray(group_a, float)
    b = None if group_b is None else np.asarray(group_b, float)
    symbols = np.zeros(len(y), dtype=int) if symbol is None else np.asarray(symbol)
    rng = np.random.default_rng(seed)
    sums_a, sums_b = np.zeros(repeats), np.zeros(repeats)
    na, nb = a.sum(), 0. if b is None else b.sum()
    if na <= 0 or (b is not None and nb <= 0):
        return np.full(repeats, np.nan)
    for s in np.unique(symbols):
        ix = np.flatnonzero(symbols == s)
        m = len(ix)
        if a[ix].sum() == 0 and (b is None or b[ix].sum() == 0):
            continue
        if m < 2 * min_shift:
            return np.full(repeats, np.nan)
        shifts = rng.integers(min_shift, m - min_shift + 1, size=repeats)
        # With daily data each nonzero shift changes date; min_shift=24 on the
        # regular hourly universe also guarantees a different UTC date.
        if dates is not None and np.unique(np.asarray(dates)[ix], return_counts=True)[1].max() > min_shift:
            ds = np.asarray(dates)[ix]
            active = np.flatnonzero((a[ix] > 0) | (b[ix] > 0 if b is not None else False))
            for j in range(repeats):
                tries = 0
                while np.any(ds[(active + shifts[j]) % m] == ds[active]):
                    shifts[j] = rng.integers(min_shift, m - min_shift + 1)
                    tries += 1
                    if tries > 100:
                        return np.full(repeats, np.nan)
        fft_y = np.fft.rfft(y[ix])
        sums_a += np.fft.irfft(np.conj(np.fft.rfft(a[ix])) * fft_y, n=m)[shifts]
        if b is not None:
            sums_b += np.fft.irfft(np.conj(np.fft.rfft(b[ix])) * fft_y, n=m)[shifts]
    return sums_a / na - (sums_b / nb if b is not None else reference)


def control_percentile(observed, distribution):
    d = np.asarray(distribution)
    d = d[np.isfinite(d)]
    if not len(d) or not np.isfinite(observed):
        return float('nan')
    # Tolerance covers roundoff from FFT; exact ties receive a midrank.
    tol = 1e-12 * max(1., abs(observed))
    return float((np.count_nonzero(d < observed - tol) +
                  .5 * np.count_nonzero(np.abs(d - observed) <= tol)) / len(d))


def read_bars(path, hourly, asof):
    """Read only exact schemas; drop malformed/incomplete rows, record counts."""
    raw = json.loads(path.read_text())
    rows = raw if isinstance(raw, list) else []
    valid, rejected = {}, 0
    for row in rows:
        try:
            if not isinstance(row, list) or len(row) != (6 if hourly else 7):
                raise ValueError('schema')
            if hourly:
                if isinstance(row[0], (str, bool)):
                    raise ValueError('timestamp')
                stamp = pd.Timestamp(row[0], unit='ms')
            else:
                if not isinstance(row[0], str) or len(row[0]) != 10:
                    raise ValueError('date')
                stamp = pd.Timestamp(row[0])
            values = np.asarray(row[1:6], float)
            o, h, l, c, v = values
            if (stamp.tzinfo is not None or not np.isfinite(values).all() or
                    min(o, l, c) <= 0 or h < max(o, c) or l > min(o, c) or v < 0):
                raise ValueError('geometry')
            if stamp + (pd.Timedelta(hours=1) if hourly else pd.Timedelta(days=1)) > asof:
                rejected += 1
                continue
            if stamp in valid:
                rejected += 1
            valid[stamp] = values
        except (ValueError, TypeError, OverflowError):
            rejected += 1
    f = pd.DataFrame.from_dict(valid, orient='index', columns=['open', 'high', 'low', 'close', 'volume']).sort_index()
    f.index = pd.DatetimeIndex(f.index)
    return f, rejected


def source_hash():
    return hashlib.sha256(Path(__file__).read_bytes()).hexdigest()


def feature_frame(frame, path, market, asof):
    fingerprint = hashlib.sha256(path.read_bytes() +
        Path(__file__).with_name('brooks').joinpath('features.py').read_bytes() +
        json.dumps(asdict(BrooksParams()), sort_keys=True).encode() +
        ('|'.join(KEEP) + str(asof)).encode()).hexdigest()
    target = CACHE / f'{market}_{path.stem}_{fingerprint[:20]}.pkl'
    if target.exists():
        return pd.read_pickle(target), fingerprint
    result = compute_features(frame, BrooksParams())[KEEP]
    result.to_pickle(target)
    return result, fingerprint


def load_inputs(asof, expected_manifest=None):
    inputs, inventory, manifest = {m: [] for m in MARKETS}, [], []
    sources = [('crypto_daily', DAILY_ROOT / 'crypto' / 'daily', False),
               ('us_daily', DAILY_ROOT / 'us' / 'daily', False),
               ('crypto_1h', HOURLY_ROOT, True)]
    for market, root, hourly in sources:
        for path in sorted(root.glob('*.json')):
            frame, rejected = read_bars(path, hourly, asof)
            symbol = path.stem
            entry = dict(market=market, symbol=symbol, rows=len(frame), rejected_rows=rejected,
                         start=str(frame.index.min()), end=str(frame.index.max()), used=False)
            reason = 'primary'
            if frame.empty:
                reason = 'no_exact_schema'
            elif hourly:
                token = symbol.removesuffix('USDT') in TOKENIZED
                span = (frame.index[-1] - frame.index[0]).days
                if token:
                    reason = 'tokenized_under_2y' if span < 730 else 'tokenized_separate'
                elif span < 730:
                    reason = 'crypto_under_2y'
                elif frame.index[0] > pd.Timestamp('2022-12-31 23:59:59'):
                    reason = 'later_crypto_over_2y_separate'
            elif market == 'us_daily' and symbol == 'SPY':
                reason = 'SPY_benchmark'
            entry['cohort'] = reason
            used = reason == 'primary'
            entry['used'] = used
            inventory.append(entry)
            file_hash = hashlib.sha256(path.read_bytes()).hexdigest()
            manifest.append(dict(path=str(path), sha256=file_hash, rows=len(frame), cohort=reason))
            if used:
                features, _ = feature_frame(frame, path, market, asof)
                inputs[market].append(dict(symbol=symbol, frame=frame, features=features))
    if expected_manifest is not None and manifest != expected_manifest:
        raise RuntimeError('Input snapshot changed after freeze; holdout must not be read')
    # Exact point-in-time daily top-60 selection, using only past/current bars.
    members = inputs['crypto_daily']
    turnover = pd.DataFrame({m['symbol']: m['frame'].volume *
        ((m['frame'].high + m['frame'].low + m['frame'].close) / 3) for m in members})
    # Use source vwap, rather than the typical-price fallback, for daily mask.
    for m in members:
        rows = json.loads((DAILY_ROOT / 'crypto' / 'daily' / (m['symbol'] + '.json')).read_text())
        vw = {pd.Timestamp(r[0]): float(r[6]) for r in rows if isinstance(r, list) and len(r) == 7}
        turnover[m['symbol']] = m['frame'].volume * pd.Series(vw).reindex(m['frame'].index)
    calendar = pd.DatetimeIndex(sorted(set().union(*(m['frame'].index for m in members))))
    observed = pd.DataFrame({m['symbol']: pd.Series(True, index=m['frame'].index) for m in members}).reindex(calendar).fillna(False).astype(bool)
    eligibility = observed & (observed.cumsum() >= 60)
    ranks = turnover.reindex(calendar).rolling(20, min_periods=20).mean().where(eligibility).rank(axis=1, ascending=False, method='first')
    eligibility &= ranks <= 60
    for market in MARKETS:
        for m in inputs[market]:
            m['eligible'] = (eligibility[m['symbol']].reindex(m['frame'].index).to_numpy() if market == 'crypto_daily'
                             else np.ones(len(m['frame']), dtype=bool))
    return inputs, pd.DataFrame(inventory), manifest


@dataclass(frozen=True)
class TestSpec:
    test: str
    variant: str
    side: int
    k: int
    outcome: str = 'return'
    arm_a: str = ''
    arm_b: str = 'baseline'
    reference: float = 0.
    expected: int = 1
    label: str = ''

    @property
    def key(self):
        return f'{self.test}|{self.variant}|{self.side}|{self.k}|{self.outcome}'


def specifications(market):
    """Entire scalar hypothesis grid, independent of any observed outcome."""
    ks = HORIZONS[market]
    n = 12 if market == 'crypto_1h' else 5
    specs = []
    for k in ks:
        specs += [TestSpec('T0', 'drift', 1, k, arm_a='all', arm_b='constant'),
                  TestSpec('T0', 'positive', 1, k, 'positive', 'all', 'constant', .5)]
    for side in (1, -1):
        for k in ks:
            for pattern in ('h2', 'reversal'):
                for state in ('aligned', 'counter', 'uncertain'):
                    specs.append(TestSpec('T1', f'{pattern}_{state}', side, k,
                                          arm_a=f'{pattern}_{state}'))
                for other in ('counter', 'uncertain'):
                    specs.append(TestSpec('T1', f'{pattern}_aligned_minus_{other}', side, k,
                        arm_a=f'{pattern}_aligned', arm_b=f'{pattern}_{other}'))
            for outcome in ('return', 'positive'):
                for band in ('low', 'medium', 'high'):
                    specs.append(TestSpec('T3', band, side, k, outcome, f'strength_{band}'))
                specs.append(TestSpec('T3', 'high_minus_low', side, k, outcome,
                                      'strength_high', 'strength_low'))
            for arm in ('a', 'b', 'c'):
                specs.append(TestSpec('T4', arm, side, k, arm_a='t4_' + arm))
            for a, b in (('b', 'a'), ('c', 'a'), ('c', 'b')):
                specs.append(TestSpec('T4', a + '_minus_' + b, side, k,
                                      arm_a='t4_' + a, arm_b='t4_' + b))
            for filter_ in ('doji', 'overlap', 'micro'):
                for arm in ('banned', 'allowed'):
                    specs.append(TestSpec('T6', filter_ + '_' + arm, side, k,
                        arm_a=filter_ + '_' + arm, expected=-1 if arm == 'banned' else 1))
                specs.append(TestSpec('T6', filter_ + '_banned_minus_allowed', side, k,
                    arm_a=filter_ + '_banned', arm_b=filter_ + '_allowed', expected=-1))
        for label in ('new_extreme', 'range_return'):
            for comparator in ('80pct', 'baseline'):
                specs.append(TestSpec('T2', label + '_' + comparator, side, n, 'event',
                    label, 'constant' if comparator == '80pct' else 'baseline',
                    .8 if comparator == '80pct' else 0., label=label))
        for event in ('climax', 'gap_touch'):
            specs.append(TestSpec('T5', event, side, 10, 'event', event,
                                  label='ema_return'))
    return specs


def pattern_masks(frame, f, side, quality):
    s, other = ('bull', 'bear') if side > 0 else ('bear', 'bull')
    event = f.h2 if side > 0 else f.l2
    reversal = f[f'reversal_{s}_minimum']
    ai = f.always_in
    candidate = event | reversal
    attrs = {}
    for name, col in [('doji', 'is_doji'), ('overlap', f'reversal_{s}_excessive_overlap')]:
        attrs[name] = f[col].where(~event, f[col].shift(1, fill_value=False))
    attrs['micro'] = f[f'micro_channel_{other}']
    masks = {'all': np.ones(len(frame), bool), 'h2': event.to_numpy(),
             't4_a': event.to_numpy(), 't4_b': (event & (ai == side)).to_numpy()}
    masks['t4_c'] = (event & (ai == side) &
                    (f[f'reversal_{s}_quality_score'].shift(1) >= quality)).to_numpy()
    for pattern, series in (('h2', event), ('reversal', reversal)):
        for state, condition in [('aligned', ai == side), ('counter', ai == -side), ('uncertain', ai == 0)]:
            masks[f'{pattern}_{state}'] = (series & condition).to_numpy()
    strength = f[f'strength_{s}_normalized']
    for band, condition in [('low', strength < 1/3),
                            ('medium', (strength >= 1/3) & (strength < 2/3)), ('high', strength >= 2/3)]:
        masks['strength_' + band] = ((ai == side) & condition).to_numpy()
    for filter_, flag in attrs.items():
        masks[filter_ + '_banned'] = (candidate & flag).to_numpy()
        masks[filter_ + '_allowed'] = (candidate & ~flag).to_numpy()
    masks['new_extreme'] = ((ai == side) & (strength >= 2/3) & f[f'reversal_{other}_minimum']).to_numpy()
    hi, lo = frame.high.shift(1).rolling(3).max(), frame.low.shift(1).rolling(3).min()
    breakout = frame.close > hi if side > 0 else frame.close < lo
    masks['breakout_pool'] = breakout.to_numpy()
    range_ = ((ai == 0) & (ai.shift(1) == 0) &
              (f.overlap_ratio.shift(1).rolling(3).min() >= .5) &
              (f.inside.shift(1).rolling(3).max() > 0))
    masks['range_return'] = (range_ & breakout).to_numpy()
    masks['climax'] = f[f'climax_{s}_end'].to_numpy()
    masks['gap_touch'] = f[f'ema_gap_{s}_first_touch'].to_numpy()
    return masks


def panel_for(inputs, market, split, k, side, quality, label=''):
    pieces = []
    for si, m in enumerate(inputs):
        frame, f = m['frame'], m['features']
        # Truncate BEFORE constructing labels: discovery never reads later
        # prices even to compute subsequently purged returns.
        lo, hi = BOUNDS[split]
        visible = frame.index < hi
        frame, f = frame.loc[visible], f.loc[visible]
        eligible = m['eligible'][visible] & f.strength_ready.to_numpy()
        labels = forward_labels(frame, (k,), f.atr)
        mask = eligible & split_mask(frame.index, split, labels[f'end_{k}'])
        raw = side * labels[f'return_{k}'].to_numpy()
        norm = side * labels[f'atr_{k}'].to_numpy()
        y = raw
        if label:
            y = event_labels(frame, f, label, side, k).to_numpy()
        masks = pattern_masks(frame, f, side, quality)
        if label == 'range_return':
            mask &= masks['breakout_pool']
        mask &= np.isfinite(y) & np.isfinite(raw) & np.isfinite(norm)
        ix = np.flatnonzero(mask)
        if not len(ix):
            continue
        part = pd.DataFrame({'date': frame.index.normalize()[ix], 'symbol': si,
                             'y': y[ix], 'raw': raw[ix], 'atr': norm[ix],
                             'scale_inverse': (frame.close / f.atr).to_numpy()[ix]})
        for key, value in masks.items():
            part[key] = value[ix]
        pieces.append(part)
    if not pieces:
        return pd.DataFrame()
    return pd.concat(pieces, ignore_index=True)


class StatisticsBatch:
    """Accumulate date sufficient statistics, bootstrap all contrasts together."""

    def __init__(self, dates, seed, draws=2000):
        self.dates = np.unique(np.asarray(dates, dtype='datetime64[D]'))
        self.seed, self.draws = seed, draws
        self.parts, self.records = [], []

    def add(self, panel, spec, market, split):
        row = dict(market=market, split=split, test=spec.test, variant=spec.variant,
                   side='long' if spec.side > 0 else ('avoid' if market == 'us_daily' else 'short'),
                   k=spec.k, outcome=spec.outcome, key=spec.key, expected=spec.expected,
                   unit='bps' if spec.outcome == 'return' else 'pp', reference=spec.reference,
                   status='insufficient', n=0, n_b=0, dates=0, dates_b=0,
                   effect=np.nan, mean=np.nan, baseline=np.nan, raw_bps=np.nan,
                   net_bps=np.nan, atr_mean=np.nan, net_atr_mean=np.nan,
                   baseline_raw_bps=np.nan, baseline_atr=np.nan, effect_atr=np.nan,
                   random_d_mean_bps=np.nan, random_d_low_bps=np.nan, random_d_high_bps=np.nan,
                   positive_rate=np.nan, se=np.nan, week_se=np.nan,
                   p_raw=1., p=1., week_p=np.nan, ci_low=np.nan, ci_high=np.nan,
                   control_percentile=np.nan, control_mean=np.nan, control_sd=np.nan,
                   control_low=np.nan, control_high=np.nan)
        self.records.append(row)
        if panel.empty:
            self.parts.append(None)
            return
        y = panel.y.to_numpy() if spec.outcome == 'event' else (
            (panel.raw.to_numpy() > 0).astype(float) if spec.outcome == 'positive' else panel.raw.to_numpy() * 10000)
        if spec.outcome == 'event':
            y = y * 100
        elif spec.outcome == 'positive':
            y = y * 100
        a = panel[spec.arm_a].to_numpy(dtype=float)
        if not a.sum():
            self.parts.append(None)
            return
        sym = panel.symbol.to_numpy()
        if spec.arm_b == 'baseline':
            counts = np.bincount(sym, weights=a, minlength=sym.max() + 1)
            sizes = np.bincount(sym, minlength=len(counts))
            b = (counts / np.maximum(sizes, 1))[sym]
        elif spec.arm_b == 'constant':
            b = None
        else:
            b = panel[spec.arm_b].to_numpy(dtype=float)
        if b is not None and not b.sum():
            self.parts.append(None)
            row['n'] = int(a.sum())
            return
        na, nb = a.sum(), b.sum() if b is not None else 0.
        mu_a = float(a @ y / na)
        reference = spec.reference * (100 if spec.outcome != 'return' else 1)
        mu_b = float(b @ y / nb) if b is not None else reference
        day = panel.date.to_numpy(dtype='datetime64[D]')
        inv = np.searchsorted(self.dates, day)
        g = len(self.dates)
        sums_a = np.bincount(inv, weights=a * y, minlength=g)
        counts_a = np.bincount(inv, weights=a, minlength=g)
        sums_b = np.bincount(inv, weights=b * y, minlength=g) if b is not None else np.zeros(g)
        counts_b = np.bincount(inv, weights=b, minlength=g) if b is not None else np.zeros(g)
        influence = (sums_a - mu_a * counts_a) / na
        if b is not None:
            influence -= (sums_b - mu_b * counts_b) / nb
        support = (counts_a > 0) | (counts_b > 0)
        gd = np.count_nonzero(support)
        se = np.sqrt(gd / (gd - 1) * np.sum(influence ** 2)) if gd > 1 else np.nan
        # Monday-start weeks; preserve all dates within their common week.
        week = self.dates.astype('int64') - ((self.dates.astype('int64') + 3) % 7)
        weeks, winv = np.unique(week[support], return_inverse=True)
        wi = np.bincount(winv, weights=influence[support])
        gw = len(weeks)
        week_se = np.sqrt(gw / (gw - 1) * (wi @ wi)) if gw > 1 else np.nan
        da, db = np.count_nonzero(counts_a), np.count_nonzero(counts_b)
        ok = na >= 30 and da >= 20 and (b is None or (nb >= 30 and db >= 20))
        cost = .001 if market == 'us_daily' else .0016
        raw, atr = panel.raw.to_numpy(), panel.atr.to_numpy()
        scale_inverse = panel.scale_inverse.to_numpy()
        row.update(n=int(na), n_b=int(np.count_nonzero(b)) if b is not None else 0,
                   dates=int(da), dates_b=int(db), effect=mu_a-mu_b, mean=mu_a, baseline=mu_b,
                   raw_bps=float(a @ raw / na * 10000), net_bps=float(a @ (raw-cost) / na * 10000),
                   atr_mean=float(a @ atr / na), net_atr_mean=float(a @ (atr-cost*scale_inverse) / na),
                   positive_rate=float(a @ (raw > 0) / na), se=float(se), week_se=float(week_se),
                   p_raw=normal_p(mu_a-mu_b, se), p=normal_p(mu_a-mu_b, se) if ok else 1.,
                   week_p=normal_p(mu_a-mu_b, week_se), status='ok' if ok else 'insufficient')
        if b is not None:
            row.update(baseline_raw_bps=float(b @ raw / nb * 10000),
                       baseline_atr=float(b @ atr / nb),
                       effect_atr=float(a @ atr / na - b @ atr / nb))
        elif spec.outcome == 'return':
            row.update(baseline_raw_bps=0., baseline_atr=0., effect_atr=row['atr_mean'])
        if spec.test != 'T0':
            distribution = shifted_control(y, a, b, repeats=500,
                seed=stable_seed(f'{market}|{split}|{spec.key}'),
                min_shift=24 if market == 'crypto_1h' else 1, symbol=sym, dates=day,
                reference=reference)
            finite = distribution[np.isfinite(distribution)]
            if len(finite):
                row.update(control_percentile=control_percentile(row['effect'], finite),
                    control_mean=float(finite.mean()), control_sd=float(finite.std(ddof=1)),
                    control_low=float(np.quantile(finite, .025)), control_high=float(np.quantile(finite, .975)))
                if spec.outcome == 'return' and spec.arm_b == 'baseline':
                    row.update(random_d_mean_bps=float(finite.mean()+mu_b),
                        random_d_low_bps=float(np.quantile(finite, .025)+mu_b),
                        random_d_high_bps=float(np.quantile(finite, .975)+mu_b))
            else:
                row.update(status='control_insufficient', p=1.)
        self.parts.append((sums_a, counts_a, sums_b, counts_b, b is not None, reference))

    def finish(self):
        active = [i for i, x in enumerate(self.parts) if x is not None]
        g = len(self.dates)
        if not active or not g:
            return self.records
        rng = np.random.default_rng(self.seed)
        weights = rng.multinomial(g, np.full(g, 1/g), size=self.draws).astype(float)
        # Chunking keeps peak memory modest; means use the SAME date draw in
        # both arms and recalculate denominators on every bootstrap replicate.
        for start in range(0, len(active), 64):
            ids = active[start:start+64]
            x = [self.parts[i] for i in ids]
            sa, ca, sb, cb = (np.column_stack([z[j] for z in x]) for j in range(4))
            with np.errstate(divide='ignore', invalid='ignore'):
                estimate = (weights @ sa) / (weights @ ca)
                base = np.zeros_like(estimate)
                flags = np.array([z[4] for z in x])
                base[:, flags] = ((weights @ sb[:, flags]) / (weights @ cb[:, flags]))
                base[:, ~flags] = np.array([z[5] for z in x])[~flags]
                estimate -= base
            for col, i in enumerate(ids):
                finite = estimate[:, col][np.isfinite(estimate[:, col])]
                self.records[i]['bootstrap_valid'] = len(finite)
                if len(finite) >= .95 * self.draws:
                    self.records[i]['ci_low'], self.records[i]['ci_high'] = np.quantile(finite, [.025, .975])
        return self.records


def run_split(inputs, market, split, quality):
    all_dates = sorted(set().union(*(m['frame'].index[split_mask(m['frame'].index, split)].normalize()
                                    for m in inputs)))
    batch = StatisticsBatch(all_dates, stable_seed(f'bootstrap|{market}|{split}'))
    specs = specifications(market)
    grouped = {}
    for spec in specs:
        grouped.setdefault((spec.k, spec.side, spec.label), []).append(spec)
    for (k, side, label), group in grouped.items():
        panel = panel_for(inputs, market, split, k, side, quality, label)
        for spec in group:
            batch.add(panel, spec, market, split)
    return batch.finish()


def quality_selection(inputs):
    rows = []
    for market in MARKETS:
        k = 12 if market == 'crypto_1h' else 5
        sides = (1,) if market == 'us_daily' else (1, -1)
        for split in ('train', 'validation'):
            panels = [panel_for(inputs[market], market, split, k, side, 1) for side in sides]
            for q in (1, 2, 3, 4):
                selected, base = [], []
                dates = set()
                for panel, side in zip(panels, sides):
                    if panel.empty:
                        continue
                    # Rebuild masks only: no different outcome horizon is seen.
                    p = panel_for(inputs[market], market, split, k, side, q)
                    a, b = p.t4_c.to_numpy(), p.t4_b.to_numpy()
                    selected.extend(p.loc[a, 'atr'].tolist())
                    base.extend(p.loc[b, 'atr'].tolist())
                    dates.update(p.loc[a, 'date'])
                gain = np.mean(selected)-np.mean(base) if selected and base else np.nan
                rows.append(dict(market=market, split=split, threshold=q, n=len(selected),
                    dates=len(dates), atr_mean=float(np.mean(selected)) if selected else np.nan,
                    b_atr_mean=float(np.mean(base)) if base else np.nan, atr_gain=gain,
                    eligible=len(selected) >= 100 and len(dates) >= 30))
    sensitivity = pd.DataFrame(rows)
    training = sensitivity[sensitivity.split == 'train']
    candidates = []
    for q in (1, 2, 3, 4):
        a = training[training.threshold == q]
        if len(a) == len(MARKETS) and a.eligible.all() and np.isfinite(a.atr_gain).all():
            candidates.append((float(a.atr_gain.mean()), -q, q))
    chosen = max(candidates)[2] if candidates else 2
    return chosen, sensitivity


def nominate(results):
    nominations = []
    for market in MARKETS:
        k = 12 if market == 'crypto_1h' else 5
        sides = ('long',) if market == 'us_daily' else ('long', 'short')
        for side in sides:
            for variant in ('b', 'c'):
                a = results[(results.market == market) & (results.side == side) &
                    (results.test == 'T4') & (results.variant == variant) & (results.k == k)]
                if (len(a) == 2 and (a.effect > 0).all() and (a.net_bps > 0).all() and
                    (a.q_discovery < .05).all() and (a.week_p < .05).all() and
                    (a.n >= 100).all() and (a.dates >= 30).all()):
                    nominations.append(dict(market=market, side=side, variant=variant, k=k))
    return nominations


def discovery(asof):
    if (OUTPUT / 'holdout_started.json').exists():
        raise RuntimeError('Holdout has already been opened; cannot rerun discovery')
    started = time.monotonic()
    inputs, inventory, manifest = load_inputs(asof)
    inventory.to_csv(OUTPUT / 'inventory.csv', index=False)
    quality, sensitivity = quality_selection(inputs)
    sensitivity.to_csv(OUTPUT / 'quality_sensitivity.csv', index=False)
    print(f'Frozen quality threshold: {quality}', flush=True)
    rows = []
    for market in MARKETS:
        for split in ('train', 'validation'):
            print(f'Computing {market} {split}', flush=True)
            rows.extend(run_split(inputs[market], market, split, quality))
    results = pd.DataFrame(rows)
    results['q_discovery'] = bh_adjust(results.p)
    results.to_csv(OUTPUT / 'discovery_results.csv', index=False)
    freeze = dict(version=1, source_sha256=source_hash(), asof=str(asof), manifest=manifest,
                  params=asdict(BrooksParams()), quality_threshold=quality,
                  planned_per_split={m: len(specifications(m)) for m in MARKETS},
                  nominations=nominate(results), bootstrap_draws=2000, control_draws=500,
                  elapsed_seconds=time.monotonic()-started,
                  discovery_sha256=hashlib.sha256((OUTPUT / 'discovery_results.csv').read_bytes()).hexdigest())
    (OUTPUT / 'FROZEN.json').write_text(json.dumps(freeze, indent=2, ensure_ascii=False))
    print(f'Discovery frozen: {len(results)} tests; nominations={freeze["nominations"]}; '
          f'{freeze["elapsed_seconds"]:.1f}s', flush=True)


def holdout():
    freeze = json.loads((OUTPUT / 'FROZEN.json').read_text())
    if freeze['source_sha256'] != source_hash():
        raise RuntimeError('Study source differs from pre-holdout freeze')
    if hashlib.sha256((OUTPUT / 'discovery_results.csv').read_bytes()).hexdigest() != freeze['discovery_sha256']:
        raise RuntimeError('Discovery results differ from freeze')
    inputs, _, _ = load_inputs(pd.Timestamp(freeze['asof']), freeze['manifest'])
    # Exclusive marker makes a second holdout label pass impossible via CLI.
    with (OUTPUT / 'holdout_started.json').open('x') as f:
        json.dump(dict(source_sha256=source_hash(), started=str(pd.Timestamp.now('UTC'))), f)
    rows = []
    for market in MARKETS:
        print(f'One-shot holdout {market}', flush=True)
        rows.extend(run_split(inputs[market], market, 'holdout', freeze['quality_threshold']))
    result = pd.DataFrame(rows)
    result.to_csv(OUTPUT / 'holdout_results.csv', index=False)
    combine_results()


def combine_results():
    dev = pd.read_csv(OUTPUT / 'discovery_results.csv')
    test = pd.read_csv(OUTPUT / 'holdout_results.csv')
    all_ = pd.concat([dev, test], ignore_index=True)
    all_['q'] = bh_adjust(all_.p)
    all_.to_csv(OUTPUT / 'results.csv', index=False)
    all_[(all_.q < .05) & (all_.status == 'ok')].to_csv(OUTPUT / 'significant_tests.csv', index=False)
    for name, subset in all_.groupby('test', sort=True):
        subset.to_csv(OUTPUT / f'{name}.csv', index=False)
    return all_


def repair_missing_controls():
    """Complete previously unavailable controls ONLY, never refit OOS effects.

    Technical correction: m==2*minimum_shift has one legal offset; the original
    strict guard rejected it. A genuine singleton has no other eligible date
    and is conservatively insufficient (planned slot retained). No hypothesis,
    horizon, threshold, effect, SE, bootstrap draw or raw p-value is changed.
    Original frozen source/results must be preserved in first_pass beforehand.
    """
    if (OUTPUT / 'CONTROL_CORRECTION.json').exists():
        raise RuntimeError('Missing-control completion has already run')
    freeze = json.loads((OUTPUT / 'FROZEN.json').read_text())
    original = OUTPUT / 'first_pass'
    if hashlib.sha256((original / 'brooks_study.py').read_bytes()).hexdigest() != freeze['source_sha256']:
        raise RuntimeError('Original frozen source backup does not match')
    inputs, _, _ = load_inputs(pd.Timestamp(freeze['asof']), freeze['manifest'])
    log = []
    cols = ['control_percentile', 'control_mean', 'control_sd', 'control_low', 'control_high',
            'random_d_mean_bps', 'random_d_low_bps', 'random_d_high_bps']
    for file in ('discovery_results.csv', 'holdout_results.csv'):
        rows = pd.read_csv(OUTPUT / file)
        original_rows = rows.copy(deep=True)
        missing = rows[(rows.test != 'T0') & rows.effect.notna() & rows.control_percentile.isna()]
        for (market, split), subset in missing.groupby(['market', 'split']):
            specs = {s.key: s for s in specifications(market)}
            groups = {}
            for i in subset.index:
                s = specs[rows.loc[i, 'key']]
                groups.setdefault((s.k, s.side, s.label), []).append(i)
            for (k, side, label), ids in groups.items():
                panel = panel_for(inputs[market], market, split, k, side, freeze['quality_threshold'], label)
                for i in ids:
                    spec = specs[rows.loc[i, 'key']]
                    y = (panel.y.to_numpy()*100 if spec.outcome == 'event' else
                         ((panel.raw.to_numpy()>0).astype(float)*100 if spec.outcome == 'positive'
                          else panel.raw.to_numpy()*10000))
                    a = panel[spec.arm_a].to_numpy(float)
                    sym = panel.symbol.to_numpy()
                    if spec.arm_b == 'baseline':
                        count = np.bincount(sym, weights=a, minlength=sym.max()+1)
                        size = np.bincount(sym, minlength=len(count))
                        b = (count/np.maximum(size, 1))[sym]
                    else:
                        b = None if spec.arm_b == 'constant' else panel[spec.arm_b].to_numpy(float)
                    reference = spec.reference * (100 if spec.outcome != 'return' else 1)
                    baseline = b@y/b.sum() if b is not None else reference
                    observed = a@y/a.sum()-baseline
                    if not np.isclose(observed, rows.loc[i,'effect'], rtol=1e-10, atol=1e-9):
                        raise RuntimeError('Reconstructed control inputs differ from frozen effect')
                    d = shifted_control(y, a, b, repeats=500,
                        seed=stable_seed(f'{market}|{split}|{spec.key}'),
                        min_shift=24 if market=='crypto_1h' else 1,
                        symbol=sym, dates=panel.date.to_numpy(dtype='datetime64[D]'), reference=reference)
                    finite = d[np.isfinite(d)]
                    if len(finite):
                        rows.loc[i, cols[:5]] = [control_percentile(observed, finite), finite.mean(),
                            finite.std(ddof=1), np.quantile(finite,.025), np.quantile(finite,.975)]
                        if spec.outcome=='return' and spec.arm_b=='baseline':
                            rows.loc[i,cols[5:]] = [finite.mean()+baseline, np.quantile(finite,.025)+baseline,
                                                   np.quantile(finite,.975)+baseline]
                    else:
                        rows.loc[i,'status'] = 'control_insufficient'
                        rows.loc[i,'p'] = 1.
                    log.append(dict(market=market, split=split, key=spec.key,
                                    completed=bool(len(finite)), draws=len(finite)))
        # A technical completion must not change any forward-outcome statistic.
        changed = set(cols + ['status','p','q_discovery'])
        for col in original_rows.columns:
            if col not in changed:
                pd.testing.assert_series_equal(rows[col], original_rows[col], check_exact=True)
        rows.to_csv(OUTPUT / file, index=False)
    dev = pd.read_csv(OUTPUT / 'discovery_results.csv')
    dev['q_discovery'] = bh_adjust(dev.p)
    dev.to_csv(OUTPUT / 'discovery_results.csv', index=False)
    correction = dict(reason='Two eligible dates permit one nonzero circular shift; fixed off-by-one guard.',
        policy='True singleton cells retain estimates but are insufficient, p=1, planned BH slot retained.',
        original_source_sha256=freeze['source_sha256'], corrected_source_sha256=source_hash(),
        outcomes_refit=False, changed_hypotheses=False, changed_quality=False,
        original_nominations=freeze['nominations'], rows=log)
    (OUTPUT / 'CONTROL_CORRECTION.json').write_text(json.dumps(correction,indent=2,ensure_ascii=False))
    combine_results()
    print(f'Controls completed: {sum(r["completed"] for r in log)}; '
          f'true singleton insufficient: {sum(not r["completed"] for r in log)}; effects not refit', flush=True)


def fmt(x, digits=2):
    return '—' if pd.isna(x) else f'{x:.{digits}f}'


def markdown_table(frame, cols):
    lines = ['| ' + ' | '.join(label for key, label in cols) + ' |',
             '| ' + ' | '.join('---' for _ in cols) + ' |']
    for _, row in frame.iterrows():
        fields = []
        for key, _ in cols:
            value = row[key]
            if isinstance(value, (float, np.floating)):
                value = '—' if pd.isna(value) else (f'{value:.3g}' if key in ('p_raw', 'p', 'q', 'week_p') else f'{value:.2f}')
            fields.append(str(value))
        lines.append('| ' + ' | '.join(fields) + ' |')
    return '\n'.join(lines)


def summary_rows(result):
    """Fixed primary-horizon summaries; no best-cell or best-window search."""
    keep = []
    for market in MARKETS:
        primary = 12 if market == 'crypto_1h' else 5
        a = result[(result.market == market) & (result.side == 'long')]
        selectors = [
            ('T0', 'drift', 'return', primary), ('T0', 'positive', 'positive', primary),
            ('T1', 'h2_aligned_minus_counter', 'return', primary),
            ('T1', 'h2_aligned_minus_uncertain', 'return', primary),
            ('T1', 'reversal_aligned_minus_counter', 'return', primary),
            ('T1', 'reversal_aligned_minus_uncertain', 'return', primary),
            ('T2', 'new_extreme_80pct', 'event', primary),
            ('T2', 'new_extreme_baseline', 'event', primary),
            ('T2', 'range_return_80pct', 'event', primary),
            ('T2', 'range_return_baseline', 'event', primary),
            ('T3', 'high_minus_low', 'return', primary),
            ('T3', 'high_minus_low', 'positive', primary),
            ('T4', 'b', 'return', primary), ('T4', 'c', 'return', primary),
            ('T4', 'b_minus_a', 'return', primary), ('T4', 'c_minus_a', 'return', primary),
            ('T5', 'climax', 'event', 10), ('T5', 'gap_touch', 'event', 10),
            ('T6', 'doji_banned_minus_allowed', 'return', primary),
            ('T6', 'overlap_banned_minus_allowed', 'return', primary),
            ('T6', 'micro_banned_minus_allowed', 'return', primary),
        ]
        for test, variant, outcome, k in selectors:
            keep.append(a[(a.test == test) & (a.variant == variant) &
                          (a.outcome == outcome) & (a.k == k)])
    return pd.concat(keep, ignore_index=True)


def promotion_confirmations(result, nominations):
    confirmed = []
    for nominee in nominations:
        a = result[(result.market == nominee['market']) & (result.side == nominee['side']) &
                   (result.variant == nominee['variant']) & (result.k == nominee['k']) &
                   (result.test == 'T4') & (result.split == 'holdout')]
        if len(a) == 1:
            r = a.iloc[0]
            if (r.effect > 0 and r.net_bps > 0 and r.q < .05 and r.week_p < .05 and
                    r.n >= 100 and r.dates >= 30):
                confirmed.append(nominee)
    return confirmed


def conclusion_text(result):
    """Fixed cells only: summarize the one holdout pass without selecting winners."""
    def cells(test, variant, outcome='return', field='effect'):
        texts = []
        for market in MARKETS:
            k = 10 if test == 'T5' else (12 if market == 'crypto_1h' else 5)
            a = result[(result.test == test) & (result.variant == variant) &
                (result.outcome == outcome) & (result.market == market) &
                (result.side == 'long') & (result.k == k) & (result.split == 'holdout')]
            r = a.iloc[0]
            texts.append(f'{market} {fmt(r[field])} (n={r.n}, q={fmt(r.q,3)})')
        return '；'.join(texts)
    return [
        'T0：样本外主周期正收益比例（%）为 '+cells('T0','positive','positive','mean')+
        '；平均漂移（bps）为 '+cells('T0','drift')+'。50/50必须逐市场评估，未拒绝不代表等价。',
        'T1：样本外做多H2顺势减逆势收益（bps）为 '+cells('T1','h2_aligned_minus_counter')+
        '；逆势或不确定“无边际”不能靠不显著证明，完整两类形态和三状态见T1表。',
        'T2：样本外区间向上突破后收回比例（%）为 '+cells('T2','range_return_80pct','event','mean')+
        '；表中80pct效应为该比例减80个百分点。强趋势反转分支受固定高档的稀少样本限制。',
        'T3：样本外高档减低档做多收益（bps）为 '+cells('T3','high_minus_low')+
        '；高档稀疏时结论为样本不足，不能用低档/中档的显著性证明单调分层。',
        'T4：样本外b相对a的做多收益增量（bps）为 '+cells('T4','b_minus_a')+
        '；c相对a为 '+cells('T4','c_minus_a')+'；晋级取决于已冻结的训练/验证提名规则。',
        'T5：样本外高潮停止后10根内回EMA的基线增量（pp）为 '+cells('T5','climax','event')+
        '；20根缺口首次触线后的增量为 '+cells('T5','gap_touch','event')+
        '；只支持/否定均线回归代理，不能证明调整持续≥10根。',
        'T6：样本外禁做减允许的做多收益（bps），十字星：'+cells('T6','doji_banned_minus_allowed')+
        '；过度重叠：'+cells('T6','overlap_banned_minus_allowed')+
        '；微型通道逆势：'+cells('T6','micro_banned_minus_allowed')+
        '；负值才符合禁做断言，须同时审视q和周稳健性。',
    ]


def report():
    freeze = json.loads((OUTPUT / 'FROZEN.json').read_text())
    result = pd.read_csv(OUTPUT / 'results.csv')
    inventory = pd.read_csv(OUTPUT / 'inventory.csv')
    sensitivity = pd.read_csv(OUTPUT / 'quality_sensitivity.csv')
    significant = result[(result.q < .05) & (result.status == 'ok')]
    robust = significant[significant.week_p < .05]
    expected = robust[robust.effect * robust.expected > 0]
    primary = summary_rows(result)
    primary.to_csv(OUTPUT / 'primary_summary.csv', index=False)
    confirmed = promotion_confirmations(result, freeze['nominations'])
    lines = ['# Brooks 信号层预注册研究', '',
        '只研究收盘后概率和前瞻对数收益；未模拟订单、止损、仓位或盈亏。', '',
        f'数据快照截止：{freeze["asof"]} UTC；日线按文件交易日、1h 按 UTC 日期聚类。',
        f'预注册源码 SHA256：`{freeze["source_sha256"]}`；默认 BrooksParams，质量阈值冻结 Q={freeze["quality_threshold"]}。',
        f'共 **{len(result)} 个预注册标量检验**，包括 {sum(result.status != "ok")} 个样本不足单元（BH 分母保留，p=1）；'
        f'原始日期聚类 p<.05：{sum((result.p < .05) & (result.status == "ok"))} 个，'
        f'全局 BH q<.05：**{len(significant)}** 个，其中周聚类 p<.05：{len(robust)} 个，符合假设方向：{len(expected)} 个。', '',
        '## 七项结论（固定主周期，含一次样本外）', '',
        *[f'- {s}' for s in conclusion_text(result)], '',
        '## 口径和读表', '',
        '- 训练截至 2023-12-31，验证为 2024，样本外自 2025-01-01；每个标签终点必须在同一切分，末尾不足 k 根保留缺失。'
        ' discovery 在构造标签前截断价格；冻结文件之后只打开一次样本外，`holdout_started.json` 为防重跑标记。',
        '- 收益=方向×log(C[t+k]/C[t])；原始 bps=×10000；ATR 单位=对数收益/(ATR[t]/C[t])。'
        ' 双边成本币16bps、股10bps；净值列仅为扣成本前瞻收益代理，不含资金费、借券或执行路径。',
        '- 表中效应是 A−B；基线 B 按 A 的标的数配权，同一标的同切分的可用随机日期。'
        '比较两组时保留重叠协方差。n 是 A 的事件数，nB 是 B 的非零观察数；概率效应单位为百分点 pp，收益效应单位为 bps。',
        '- 日期 CR1 聚类稳健 SE，正态近似双侧 p；周为周一至周日。95% CI 为整日期重抽样2000次的百分位区间，'
        '每次重算两组分母。收益重叠产生跨日期相关，周检查仍不能完全处理20日标签的依赖。',
        '- 对照分位=观测 A−B 在500次同标的错位控制中的中位秩；方向为正的断言关注接近100%，T6禁做相对允许关注接近0%。'
        '同一对照两臂用相同位移，数量与标的权重不变；1h 至少错开24个可用观察。对照独立错位各标的，未保留跨标的同日相关。',
        '- p原是无门槛日期聚类 p；p 是样本门槛后进入 BH 的值。任一臂 n<30 或日期<20 标为不足且 p=1。'
        '无法为某标的找到其他符合条件日期的对照单元也标为control_insufficient，p=1。'
        '不显著不能证明“无边际”。q 为全部市场/周期/切分/方向/对比的单一 BH 家族；周检查未另作发现性检验。',
        '- 原始均值/净均值/ATR/方向比例为 A 臂的描述，双组差及其区间看效应列。'
        'T2/T5 的原始收益列是同一事件样本在其概率窗口终点的描述，不参与另一组显著性检验。',
        '- T1 H2/L2 收益从触发根收盘开始，质量来自前根；不将 t 的确认回填到 t−1。'
        'T6 如 H2 与反转最低要求同根出现，去重并优先用 H2 前根属性。',
        '- 强度固定以28分支持子集分成三等宽档（高档≥19分），不是原表39分的30/15阈值；'
        '这可能导致高档稀少，不能事后改成收益更好的阈值。',
        '- T2“反转失败”采用未来高低超越当前已知20根极值；不代表逆势交易亏损或先触止损。'
        '区间为明确不确定状态、此前3根重叠且含内包，突破收盘之后严格回到冻结区间；对照限定为同方向普通突破。',
        '- T5 排除信号根，只查接下来1..10根的动态 EMA20/ATR 附近收盘。它验证用户指定的均线回归代理，'
        '无法证明调整持续至少10根或有两腿；首次触线后回均线可能有机械性。', '',
        '## 数据覆盖', '',
        markdown_table(inventory.groupby(['market', 'cohort'], as_index=False).agg(symbols=('symbol','size'), rows=('rows','sum')),
            [('market','市场'),('cohort','组别'),('symbols','文件数'),('rows','有效K线')]), '',
        '逐文件起止时间、模式拒收数和纳入状态见 inventory.csv。主流1h纳入：' +
        '、'.join(inventory[(inventory.market=='crypto_1h') & inventory.used].symbol) + '。',
        '代币化1h标的不足两年时仅报告覆盖，不给显著性旁证；2022年后上市但满两年的币另列覆盖，未混入主流组。',
        '实盘数据目录实际有96份美股文件，本研究将SPY单列，其余95份按原文件宇宙纳入；'
        '其中可见EWY、SMH、TMF等ETF，不能将其全部称为正股。未用结果筛选或另行联网重建历史宇宙。',
        '加密日线复制 panel.py 的因果60根资格+滚动20日成交额前60掩码；'
        '源文件宇宙本身来自点时刻成交额前60的集合，存在存活者偏差，掩码不能修复已退市/未收集币的缺失。', '',
        '## 训练选阈值与冻结', '',
        f'唯一选择参数为Q；按文件头规则用训练主周期的等市场ATR增量排序，冻结为{freeze["quality_threshold"]}。'
        '所有候选在训练和验证的结果如下（参数选择用的描述性统计，无确认性p/q）：', '',
        markdown_table(sensitivity, [('market','市场'),('split','切分'),('threshold','Q'),('n','n'),
            ('dates','日期'),('atr_mean','过滤后ATR均值'),('b_atr_mean','b ATR均值'),('atr_gain','增量'),('eligible','选取资格')]), '',
        '样本外之前提名的可进入订单回测候选：' + (json.dumps(freeze['nominations'], ensure_ascii=False) if freeze['nominations'] else '**无**') + '。', '',
        '## 固定主周期结果与订单回测筛选', '',
        '以下固定查看做多侧，主周期为日线5根、1h12根；T5固定10根，未从多个周期中挑选最优。'
        '其余方向和全部预注册周期保留在后续表中。US空侧只作规避证据。', '',
        markdown_table(primary, [('test','检验'),('market','市场'),('split','切分'),('variant','对比'),
            ('outcome','标签'),('k','k'),('n','n'),('mean','A均值'),('effect','效应'),('unit','单位'),
            ('ci_low','CI下'),('ci_high','CI上'),('control_percentile','对照分位'),('p','p'),('q','q'),
            ('week_p','周p'),('net_bps','净bps'),('status','状态')]), '',
        ('按冻结的双阶段提名和样本外确认规则，**没有任何子集值得**进入Backtrader订单回测。'
         '这表示本次证据未达到预先设定的晋级标准，不表示所有未来策略都已被证明无效。' if not confirmed else
         '按冻结规则值得进入Backtrader的最小子集：' + json.dumps(confirmed, ensure_ascii=False) +
         '。理由是训练、验证和一次样本外的成本后均值、相对基线增量及周稳健性均通过预定门槛；仍须独立检验订单执行。'), '',
        '## 每项检验', '']
    display = result.copy()
    display['ci'] = [f'[{fmt(a)}, {fmt(b)}]' for a,b in zip(display.ci_low, display.ci_high)]
    display['period'] = ['样本外(一次)' if s=='holdout' else s for s in display.split]
    display['control'] = display.control_percentile * 100
    for name in ('T0', 'T1', 'T2', 'T3', 'T4', 'T5', 'T6'):
        a = display[display.test == name]
        lines += [f'### {name}', '', f'预注册{len(a)}项，BH显著{sum(a.q < .05)}项；完整列另见 {name}.csv。', '',
                  markdown_table(a, [('market','市场'),('k','k'),('period','切分'),('side','方向'),('variant','子检验'),
                    ('outcome','标签'),('n','n'),('n_b','nB'),('dates','日期'),('mean','A均值'),('baseline','B均值'),
                    ('effect','效应'),('unit','单位'),('se','日期SE'),('ci','95%日期自助CI'),
                    ('control','对照分位%'),('p_raw','p原'),('p','p'),('q','BH q'),
                    ('week_se','周SE'),('week_p','周p'),('raw_bps','原始bps'),('net_bps','净bps'),
            ('atr_mean','ATR'),('net_atr_mean','净ATR'),('effect_atr','ATR增量'),
            ('random_d_mean_bps','随机d bps'),('positive_rate','顺向比例'),('status','状态')]), '']
    lines += ['## BH后显著清单', '',
        '全量清单见 significant_tests.csv；下面按切分和假设汇总（包含反方向结果，不隐去负面结果）。', '',
        markdown_table(significant.groupby(['test','market','split'], as_index=False).agg(count=('q','size')),
            [('test','检验'),('market','市场'),('split','切分'),('count','q<.05数量')]), '',
        '## 局限与特征库审阅', '',
        '- 随机对照的两日期边界在汇总QA时修复：最初错误拒绝了只有一个非零错位的两日期池。'
        '仅补齐原本缺失的500次对照；未重估样本外均值、SE、自助区间或原始p，未改检验清单/阈值/提名。'
        '真正只有一个合格日期的两项训练单元无法构造其他日期对照，保留估计并标不足（p=1），再机械重算BH。'
        '原始冻结源码/结果保留在first_pass/，修复明细与两版hash见CONTROL_CORRECTION.json。',
        '- 来源是Al Brooks规则的可计算代理，完整裁量背景并未实现；DISCREPANCIES.md登记的46项差异仍适用。'
        '未修改已有 features.py；本次阅读未发现足以确认的新因果/数值错误。',
        '- 美股静态文件宇宙、复权历史、退市缺失、现代股票/ETF代码变化和币种存活者偏差限制外推。'
        '没有把各市场日期串成一个独立样本，也没有用IID标准误。',
        '- UTC日期整块重抽样并非连续多日区块自助法；周聚类仍可能低估较长记忆相关。'
        '500次错位分位的分辨率约0.2%，不是独立的p值。正常近似在少聚类/极端概率时受限。',
        '- 本研究没有风险/回报路径、可成交性或盈亏证据。即使均值为正，也不足以推导策略收益、胜率或仓位。'
        '任何后续订单实验必须使用scripts/backtest的Backtrader。', '',
        '- Q在训练上选择，训练中涉及c的p/q含参数选择的乐观偏差；BH不消除此偏差，'
        '验证/样本外没有重选Q。没有任何候选在样本外前获提名。', '',
        '## 复现与产物', '',
        '先运行验收单元测试，再依次执行 discovery、holdout、report。holdout 已封为一次执行，'
        '当前结果可用 report 重建Markdown而不再读取价格标签。',
        'results.csv 为全部标量检验；discovery_results.csv/holdout_results.csv 为冻结两阶段结果；'
        'FROZEN.json 包含输入SHA256、参数、阈值、源码hash和样本外前提名。'
        'quality_sensitivity.csv、inventory.csv和各T文件为原始分组结果。', '']
    (OUTPUT / 'REPORT.md').write_text('\n'.join(lines), encoding='utf-8')
    print(f'Report written: {OUTPUT / "REPORT.md"}', flush=True)


def main():
    parser = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    parser.add_argument('--phase', choices=('discovery', 'holdout', 'report', 'repair-controls'), required=True)
    parser.add_argument('--asof', default='2026-10-23 01:00:00', help='UTC snapshot cutoff; discovery only')
    args = parser.parse_args()
    OUTPUT.mkdir(parents=True, exist_ok=True)
    CACHE.mkdir(parents=True, exist_ok=True)
    if args.phase == 'discovery':
        discovery(pd.Timestamp(args.asof))
    elif args.phase == 'holdout':
        holdout()
    elif args.phase == 'repair-controls':
        repair_missing_controls()
    else:
        report()


if __name__ == '__main__':
    main()
