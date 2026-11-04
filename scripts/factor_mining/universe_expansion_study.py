"""Research-only study: what a wider, survivorship-free crypto universe would change.

Reads ``data/factor_mining/crypto_full`` (built by ``full_universe``) and the live
``data/factor_mining/crypto`` panel, never writes either; output goes to
``crypto_full/study/result.json``. Train/val windows only (holdout stays sealed),
dates aligned to the zoo's frozen split. Sections:

  census     point-in-time counts of listed / eligible / liquid USDT-M perps and
             top-N composition (delisted members, marginal-name turnover)
  breadth    mean pairwise return correlation (raw and cross-sectionally
             demeaned) and the eigenvalue participation ratio of top-N members
  costs      per rank bucket: ADV, volatility, square-root impact, forward-7d
             pump frequency, delisting within 30 days
  book       one live fapi order-book snapshot per current rank bucket: quoted
             spread and the cost of walking the book for 10k/50k USDT (network)
  rescore    zoo accepted factors and the frozen ensembles re-evaluated with the
             frozen evaluator on each universe (config.MARKETS is mutated in this
             process only; config.py is not touched)
"""
import argparse
import datetime as dt
import json
import math
import os
from pathlib import Path
import sys
import tempfile

import numpy as np
import pandas as pd

from . import config, evaluate
from .data_fetch import ROOT, write_symbol
from .ensemble import ensemble_scores
from .expr import ExprError, evaluate_expr
from .panel import forward_target, load_panels, make_splits

FULL = ROOT / 'crypto_full'
BASE = ROOT / 'crypto'
UNIVERSES = (60, 100, 150)
BUCKETS = ((1, 60), (61, 100), (101, 150), (151, 250))
ORDER_USD = (10_000, 50_000)


def _panels(root_dir, n, symbols=None):
    """load_panels on ``root_dir`` (a crypto-layout dir) with universe_n=n, in-process only."""
    cfg = config.MARKETS['crypto']
    old = cfg['universe_n']
    with tempfile.TemporaryDirectory() as tmp:
        if symbols is None:
            os.symlink(Path(root_dir).resolve(), Path(tmp) / 'crypto')
        else:
            (Path(tmp) / 'crypto' / 'daily').mkdir(parents=True)
            for s in symbols:
                for sub in ('daily', 'alt_daily/funding', 'alt_daily/premium'):
                    src = Path(root_dir) / sub / f'{s}.json'
                    if src.exists():
                        (Path(tmp) / 'crypto' / sub).mkdir(parents=True, exist_ok=True)
                        os.symlink(src.resolve(), Path(tmp) / 'crypto' / sub / f'{s}.json')
        cfg['universe_n'] = n
        try:
            return load_panels('crypto', tmp)
        finally:
            cfg['universe_n'] = old


def _truncate(panels, end):
    keep = panels.mask.index <= pd.Timestamp(end)
    panels.fields = {k: v.loc[keep] for k, v in panels.fields.items()}
    panels.mask = panels.mask.loc[keep]
    return panels


def _status(manifest):
    return {s: m['status'] for s, m in manifest['symbols'].items()}


def turnover_ranks(fields, min_history):
    observed = fields['close'].notna()
    eligible = observed & observed.cumsum().ge(min_history)
    amount = (fields['volume'] * fields['vwap']).rolling(20, min_periods=20).mean()
    ranks = amount.where(eligible).rank(axis=1, ascending=False, method='first')
    return amount, ranks, observed, eligible


def census(full, status, base_symbols, dates):
    amount, ranks, observed, eligible = turnover_ranks(full.fields, config.MARKETS['crypto']['min_history'])
    delisted = pd.Series({s: status.get(s) != 'TRADING' for s in amount.columns})
    in_base = pd.Series({s: s in base_symbols for s in amount.columns})
    rows = []
    for d in dates:
        if d not in amount.index:
            continue
        a, r = amount.loc[d], ranks.loc[d]
        row = dict(date=d.strftime('%Y-%m-%d'), listed=int(observed.loc[d].sum()),
                   eligible=int(eligible.loc[d].sum()),
                   base_listed=int(observed.loc[d, list(base_symbols & set(amount.columns))].sum()),
                   adv_ge_1m=int((a >= 1e6).sum()), adv_ge_5m=int((a >= 5e6).sum()),
                   adv_ge_20m=int((a >= 20e6).sum()))
        for n in UNIVERSES:
            members = r[r <= n].index
            row[f'top{n}'] = len(members)
            row[f'top{n}_delisted_later'] = int(delisted[members].sum())
            row[f'top{n}_in_base'] = int(in_base[members].sum())
            nth = a[r == min(n, len(members))]
            row[f'top{n}_marginal_adv'] = float(nth.iloc[0]) if len(members) and len(nth) else None
        rows.append(row)
    return rows


def breadth(panels, dates, window=60):
    """Mean pairwise corr of trailing daily returns among members at each date."""
    ret = panels.fields['close'].pct_change(fill_method=None)
    out = []
    for d in dates:
        if d not in panels.mask.index:
            continue
        pos = panels.mask.index.get_loc(d)
        if pos < window:
            continue
        members = panels.mask.columns[panels.mask.loc[d].to_numpy()]
        block = ret.iloc[pos - window + 1:pos + 1][members].dropna(axis=1)
        n = block.shape[1]
        if n < 10:
            continue
        raw = np.corrcoef(block.to_numpy().T)
        resid_block = block.sub(block.mean(axis=1), axis=0)
        resid = np.corrcoef(resid_block.to_numpy().T)
        off = ~np.eye(n, dtype=bool)
        lam = np.linalg.eigvalsh(resid)
        rho_raw = float(raw[off].mean())
        out.append(dict(date=d.strftime('%Y-%m-%d'), n=n, rho_raw=rho_raw,
                        rho_resid=float(resid[off].mean()),
                        n_eff_raw=float(n / (1 + (n - 1) * rho_raw)),
                        participation_resid=float(lam.sum() ** 2 / (lam ** 2).sum())))
    return out


def costs(full, status, dates):
    f = full.fields
    amount, ranks, observed, _ = turnover_ranks(f, config.MARKETS['crypto']['min_history'])
    ret = f['close'].pct_change(fill_method=None)
    sigma = ret.rolling(20, min_periods=15).std()
    fwd7 = f['open'].shift(-8) / f['open'].shift(-1) - 1
    last_bar = observed[::-1].cumsum()[::-1].eq(1) & observed      # final traded bar of each symbol
    delisted = pd.Series({s: status.get(s) != 'TRADING' for s in f['close'].columns})
    last_date = {s: (last_bar[s][last_bar[s]].index[-1] if last_bar[s].any() else None)
                 for s in f['close'].columns}
    out = {}
    for lo, hi in BUCKETS:
        acc = {k: [] for k in ('adv', 'sigma', 'fwd7')}
        delist_events, delist_ret, member_days = 0, [], 0
        for d in dates:
            if d not in ranks.index:
                continue
            r = ranks.loc[d]
            members = r[(r >= lo) & (r <= hi)].index
            member_days += len(members)
            acc['adv'] += amount.loc[d, members].dropna().tolist()
            acc['sigma'] += sigma.loc[d, members].dropna().tolist()
            acc['fwd7'] += fwd7.loc[d, members].dropna().tolist()
            for s in members:
                ld = pd.Timestamp(last_date[s]) if last_date[s] is not None else None
                if delisted[s] and ld is not None and d < ld <= d + pd.Timedelta(days=30):
                    delist_events += 1
                    delist_ret.append(float(f['close'].at[ld, s] / f['close'].at[d, s] - 1))
        if not member_days:
            continue
        adv, sig, fwd = (np.asarray(acc[k]) for k in ('adv', 'sigma', 'fwd7'))
        med_adv, med_sig = float(np.median(adv)), float(np.median(sig))
        row = dict(bucket=f'{lo}-{hi}', member_days=member_days, median_adv=med_adv, median_sigma=med_sig,
                   p_fwd7_gt_50=float((fwd > 0.5).mean()), p_fwd7_gt_100=float((fwd > 1.0).mean()),
                   fwd7_p99=float(np.quantile(fwd, 0.99)), fwd7_p01=float(np.quantile(fwd, 0.01)),
                   fwd7_kurt=float(pd.Series(fwd).kurt()),
                   delist_within_30d_per_1000=1000 * delist_events / member_days,
                   delist_median_ret=float(np.median(delist_ret)) if delist_ret else None)
        for q in ORDER_USD:
            # Square-root impact (Y=1), one way, for the median name; spread comes from ``book``.
            row[f'impact_one_way_{q // 1000}k'] = med_sig * math.sqrt(q / med_adv)
        out[row['bucket']] = row
    return out


def walk_book(levels, usd):
    """Average fill price slippage vs the touch for ``usd`` notional; None if the book is too thin."""
    left, cost, touch = usd, 0.0, float(levels[0][0])
    for price, qty in levels:
        price, qty = float(price), float(qty)
        take = min(left, price * qty)
        cost += take * abs(price / touch - 1)
        left -= take
        if left <= 0:
            return cost / usd
    return None


def book(full, status, http=None):
    """Live quoted spread and book-walk cost for today's rank buckets (TRADING symbols only)."""
    from .data_fetch import FAPI, Http
    http = http or Http(spacing=0.25)
    _, ranks, _, _ = turnover_ranks(full.fields, config.MARKETS['crypto']['min_history'])
    last = ranks.iloc[-1]
    out, snapshot = {}, {}
    for lo, hi in BUCKETS:
        rows = []
        for s in last[(last >= lo) & (last <= hi)].sort_values().index:
            if status.get(s) != 'TRADING':
                continue
            try:
                b = http.get(FAPI + '/fapi/v1/depth', {'symbol': s, 'limit': 100})
            except RuntimeError:
                continue
            bid, ask = float(b['bids'][0][0]), float(b['asks'][0][0])
            mid = (bid + ask) / 2
            row = dict(symbol=s, spread=(ask - bid) / mid)
            for q in ORDER_USD:
                buy, sell = walk_book(b['asks'], q), walk_book(b['bids'], q)
                row[f'cost_{q // 1000}k'] = (None if buy is None or sell is None
                                             else row['spread'] / 2 + (buy + sell) / 2)
            rows.append(row)
        snapshot[f'{lo}-{hi}'] = rows
        def med(k):
            v = [r[k] for r in rows if r[k] is not None]
            return float(np.median(v)) if v else None
        out[f'{lo}-{hi}'] = dict(n=len(rows), median_spread=med('spread'),
                                 **{f'median_cost_one_way_{q // 1000}k': med(f'cost_{q // 1000}k') for q in ORDER_USD},
                                 **{f'too_thin_{q // 1000}k': sum(r[f'cost_{q // 1000}k'] is None for r in rows)
                                    for q in ORDER_USD})
    return dict(taken_at=dt.datetime.now(dt.timezone.utc).isoformat(timespec='seconds'),
                buckets=out, symbols=snapshot)


def _short_leg_tail(score, raw, mask, segment, h):
    """Raw forward returns of bottom-quintile names on the rebalance schedule."""
    positions = np.flatnonzero(np.asarray(segment, dtype=bool))[::h]
    v, r, m = score.to_numpy()[positions], raw.to_numpy()[positions], mask.to_numpy()[positions]
    picks = []
    for j in range(len(positions)):
        ok = m[j] & np.isfinite(v[j]) & np.isfinite(r[j])
        if ok.sum() < 15:
            continue
        order = np.argsort(np.where(ok, v[j], np.inf), kind='stable')[:ok.sum() // 5 + (ok.sum() % 5 > 0)]
        picks += r[j, order].tolist()
    p = np.asarray(picks)
    if not len(p):
        return None
    losses = np.clip(p, 0, None)
    return dict(n=len(p), mean=float(p.mean()), p_gt_30=float((p > 0.3).mean()),
                p_gt_50=float((p > 0.5).mean()), p99=float(np.quantile(p, 0.99)),
                loss_share_from_gt_30=float(losses[p > 0.3].sum() / losses.sum()) if losses.sum() else 0.0)


def rescore(panels, zoo, ens, label, costs_grid=(0.0008, 0.0020)):
    cfg = dict(config.MARKETS['crypto'])
    out = dict(universe=label, horizons={})
    accepted = [f for f in zoo['factors'] if f['status'] == 'accepted']
    cache = {}
    for h in cfg['horizons']:
        splits = make_splits(panels.mask.index, h, panels.windows)
        excess, raw = forward_target(panels, h)
        K = zoo['horizons'][str(h)]['trials']
        rows = []
        for f in (x for x in accepted if x['horizon'] == h):
            try:
                values = cache.get(f['expr'])
                if values is None:
                    values = cache[f['expr']] = evaluate_expr(f['expr'], panels.fields)
            except ExprError as exc:
                rows.append(dict(id=f['id'], error=str(exc)[:80]))
                continue
            m = evaluate.evaluate_factor(values, excess, raw, panels.mask, splits, h,
                                         dict(cfg, direction=f['direction']))
            if m is None:
                rows.append(dict(id=f['id'], error='coverage'))
                continue
            rows.append(dict(id=f['id'], train_ic=m['train']['ic'], train_t=m['train']['t'],
                             val_ic=m['val']['ic'], val_t=m['val']['t'],
                             val_ls=m['val']['ls_ret'], train_ls=m['train']['ls_ret'],
                             status=evaluate.classify(m['train'], m['val'], K, evaluate.t_required(K))))
        ids = (ens['horizons'].get(str(h)) or {}).get('factor_ids', [])
        by_id = {f['id']: f for f in accepted}
        members = [by_id[i] for i in ids if i in by_id]
        ens_row = None
        try:
            score = ensemble_scores(members, panels, values_cache={f['id']: cache[f['expr']] for f in members
                                                                     if f['expr'] in cache})
        except (KeyError, ExprError):
            score = None
        if score is not None:
            ens_row = {}
            for c in costs_grid:
                m = evaluate.evaluate_factor(score, excess, raw, panels.mask, splits, h,
                                             dict(cfg, direction=1, one_way_cost=c))
                if m:
                    ens_row[f'cost_{c}'] = dict(train=m['train'], val=m['val'])
            ens_row['short_leg_val'] = _short_leg_tail(score.where(panels.mask), raw, panels.mask,
                                                       splits['val'], h)
        ok = [r for r in rows if 'error' not in r]
        out['horizons'][str(h)] = dict(
            ranges=splits['ranges'], names_per_day=float(panels.mask.loc[splits['train']].sum(axis=1).mean()),
            factors=rows, n=len(ok),
            still_accepted=sum(r['status'] == 'accepted' for r in ok),
            median_train_ic=float(np.median([r['train_ic'] for r in ok])) if ok else None,
            median_train_t=float(np.median([r['train_t'] for r in ok])) if ok else None,
            median_val_ic=float(np.median([r['val_ic'] for r in ok])) if ok else None,
            median_val_t=float(np.median([r['val_t'] for r in ok])) if ok else None,
            ensemble=ens_row)
    return out


def main(argv=None):
    p = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    p.add_argument('--full', default=str(FULL))
    p.add_argument('--base', default=str(BASE))
    p.add_argument('--sections', nargs='*', default=['census', 'breadth', 'costs', 'book', 'rescore'])
    a = p.parse_args(argv)
    full_dir, base_dir = Path(a.full), Path(a.base)
    manifest = json.loads((full_dir / 'manifest.json').read_text())
    status = _status(manifest)
    zoo = json.loads((base_dir / 'zoo.json').read_text())
    ens = json.loads((base_dir / 'ensemble.json').read_text())
    base_symbols = {q.stem for q in (base_dir / 'daily').glob('*.json')}
    base = _panels(base_dir, 60)
    end = base.mask.index[-1]
    try:      # a partial --sections run updates the previous result instead of dropping sections
        result = json.loads((full_dir / 'study' / 'result.json').read_text())
    except (OSError, ValueError):
        result = {}
    result.update(generated_at=dt.datetime.now(dt.timezone.utc).isoformat(timespec='seconds'),
                  end=end.strftime('%Y-%m-%d'), zoo_split=zoo['split'])
    full_wide = _truncate(_panels(full_dir, None), end)
    monthly = pd.date_range('2020-01-01', end, freq='MS')
    split = make_splits(base.mask.index, 1, base.windows)
    window_days = base.mask.index[(split['train'] | split['val']).to_numpy()]
    sample = pd.DatetimeIndex([d for d in window_days if d.day in (1, 15)])
    if 'census' in a.sections:
        result['census'] = census(full_wide, status, base_symbols, monthly)
    if 'costs' in a.sections:
        result['costs'] = costs(full_wide, status, window_days[::7])
    if 'book' in a.sections:
        result['book'] = book(_panels(full_dir, None), status)
    configs = [('base60', base)] + [(f'full{n}', _truncate(_panels(full_dir, n), end)) for n in UNIVERSES]
    live_now = [s for s, v in status.items() if v == 'TRADING']
    configs.append(('full100_survivors', _truncate(_panels(full_dir, 100, live_now), end)))
    if 'breadth' in a.sections:
        result['breadth'] = {label: breadth(pn, sample) for label, pn in configs}
    if 'rescore' in a.sections:
        result['rescore'] = {}
        for label, pn in configs:
            print(f'rescore {label}', file=sys.stderr)
            result['rescore'][label] = rescore(pn, zoo, ens, label)
    write_symbol(full_dir / 'study' / 'result.json', result)
    print(f'wrote {full_dir / "study" / "result.json"}')
    return 0


if __name__ == '__main__':
    sys.exit(main())
