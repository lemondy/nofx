"""Fixed v1 target rules and an offline, descriptive frozen-ensemble study.

Crypto targets consume the core live_scores snapshot. US targets reuse the S1
momentum selection on local daily rows and the frozen equity universe. Research
uses causal ensemble scores rounded exactly as in the crypto snapshot.
No data fetching, factor selection, or writes to inputs occur here.
"""
from __future__ import annotations

import argparse
from collections import Counter
import datetime as dt
import hashlib
import json
from pathlib import Path

import numpy as np
import pandas as pd

from . import panel, store
from .data_fetch import ROOT


REPO_ROOT = Path(__file__).resolve().parents[2]
US_UNIVERSE = Path(__file__).with_name('us_momentum_universe.json')
DEFAULT_PERP_MAP = REPO_ROOT / 'data/cache/usstock_symbols.json'
US_HORIZON = 28
# Daily data may bridge weekends/holidays for four calendar days. The 28-day
# rebalance horizon does not extend freshness; Go also applies max_stale_days.
US_MAX_STALE_DAYS = 4
US_VOLSCALE_START_AS_OF = '2026-12-01'
US_VOLSCALE_FIELDS = ('k', 'sigma_hat', 'sigma_ref', 'sigma_observations', 'k_date',
                      'start_as_of', 'base_rule')


def _rule(market):
    if market not in ('crypto', 'us'):
        raise ValueError('market must be crypto or us')
    rule = dict(name='ls_invvol_protected_v1' if market == 'crypto' else 'mom_ls_invvol_v1',
                quantile=0.2, long_gross=0.5, short_gross=0.5,
                max_name_weight=0.1, vol_window=30, long_only=False)
    if market == 'crypto':
        rule.update(short_exclude_ret7_quantile=0.9, min_listing_days=90)
    return rule


def _utc(now):
    value = pd.Timestamp(now if now is not None else dt.datetime.now(dt.timezone.utc))
    return value.tz_localize('UTC') if value.tzinfo is None else value.tz_convert('UTC')


def _scores_entry(market, doc):
    horizons = (doc or {}).get('horizons') or {}
    choices = (3,) if market == 'crypto' else (5, 10, 20)
    for h in choices:
        entry = horizons.get(str(h)) or horizons.get(h) or {}
        if entry.get('scores'):
            return h, entry
    return (3 if market == 'crypto' else None), {}


def _capped_invvol(vol, gross, cap):
    """Proportional water filling; an exhausted sleeve stays below gross."""
    weights = pd.Series(0., index=vol.index)
    # Scaling before division avoids overflowing inverse tiny volatilities.
    inverse = vol.min() / vol
    remaining = gross
    active = list(vol.index)
    while active and remaining > 1e-14:
        proposed = inverse.loc[active] / inverse.loc[active].sum() * remaining
        capped = proposed.index[proposed >= cap]
        if not len(capped):
            weights.loc[active] = proposed
            break
        weights.loc[capped] = cap
        remaining -= len(capped) * cap
        active = [s for s in active if s not in capped]
    return weights


def _load_perp_map(path):
    path = Path(path) if path is not None else DEFAULT_PERP_MAP
    if not path.is_absolute():
        path = REPO_ROOT / path
    try:
        doc = json.loads(path.read_text(encoding='utf-8'))
        if not isinstance(doc, dict) or doc.get('version') != 2 or not isinstance(doc.get('symbols'), list):
            return None
        return {row['underlying']: row.get('perp_symbol') for row in doc['symbols']
                if isinstance(row['underlying'], str)}
    except (OSError, ValueError, TypeError, KeyError):
        return None


def _load_us_features(root):
    # Import lazily: S1 itself imports _capped_invvol from this module.
    from scripts.backtest.fq_us_momentum_study import build_features, valid_rows

    universe = json.loads(US_UNIVERSE.read_text(encoding='utf-8'))['symbols']
    frames = {}
    for symbol in universe + ['SPY']:
        path = Path(root) / 'us/daily' / f'{symbol}.json'
        try:
            rows = json.loads(path.read_text(encoding='utf-8'))
        except (OSError, ValueError):
            rows = []
        # Same row parsing, validity check and SPY calendar as S1.load_snapshot.
        frame = pd.DataFrame([r[:6] for r in rows],
                             columns=['date', 'open', 'high', 'low', 'close', 'volume'])
        frame.index = pd.DatetimeIndex(pd.to_datetime(frame.pop('date')))
        if not frame.index.is_monotonic_increasing or frame.index.has_duplicates:
            raise ValueError(f'{symbol}: dates must be unique and ascending')
        frame = frame.astype(float)
        frames[symbol] = frame.where(valid_rows(frame), np.nan)
    calendar = frames['SPY'].dropna().index
    return build_features(frames, universe, calendar) if len(calendar) else None


def _build_us_targets(features, perps, *, now=None):
    from scripts.backtest.fq_us_momentum_study import decision_at

    generated = _utc(now)
    date = features['close'].index[-1] if features is not None else None
    result = dict(version=1, market='us', status='insufficient_universe',
                  reason='missing_spy_calendar', generated_at=generated.isoformat(),
                  as_of=str(date.date()) if date is not None else None,
                  run_id=None, horizon=US_HORIZON, rule=_rule('us'), factor_ids=[],
                  universe_size=0, weights={}, excluded={}, pct={})
    if perps is None:
        result.update(reason='perp_map_unavailable')
        return result
    if date is None:
        return result
    if (generated.normalize() - _utc(date).normalize()).days > US_MAX_STALE_DAYS:
        result.update(status='stale_scores', reason='as_of_older_than_4_days')
        return result
    decision = decision_at(features, date)
    result['universe_size'] = decision['eligible_count']
    signals = features['signal'].loc[date, decision['eligible']]
    ranks = signals.rank(pct=True).dropna()
    result['pct'] = {s + 'BUSDT': round(float(p), 6) for s, p in ranks.items()}
    if decision['eligible_count'] < 15:
        result.update(reason='fewer_than_15_eligible_symbols')
        return result
    longs, shorts = list(decision['long']), list(decision['short'])
    for symbol in shorts:
        perp = perps.get(symbol)
        if not isinstance(perp, str) or not perp.strip():
            result['excluded'][symbol + 'BUSDT'] = 'no_perp'
    shorts = [s for s in shorts if s + 'BUSDT' not in result['excluded']]
    if not shorts:
        result.update(reason='empty_short_leg_after_exclusions')
        return result
    # The perp map lists every tradable bStock pair. A long candidate missing
    # from it cannot be bought (e.g. STX on 2026-11-05), so it is excluded and
    # the long sleeve re-weighted to its full gross, mirroring the no_perp rule.
    for symbol in longs:
        if symbol not in perps:
            result['excluded'][symbol + 'BUSDT'] = 'not_listed'
    longs = [s for s in longs if s + 'BUSDT' not in result['excluded']]
    if not longs:
        result.update(reason='empty_long_leg_after_exclusions')
        return result
    vol = pd.Series(decision['volatility'], dtype=float)
    rule = result['rule']
    weights = pd.concat([
        _capped_invvol(vol.loc[longs], rule['long_gross'], rule['max_name_weight']),
        -_capped_invvol(vol.loc[shorts], rule['short_gross'], rule['max_name_weight'])])
    result['weights'] = {s + 'BUSDT': round(float(w), 6) for s, w in weights.items() if round(float(w), 6)}
    result.update(status='ok', reason=None)
    return result


def _us_volscale_signal(features):
    from scripts.backtest.fq_us_momentum_study import PARAMS, decision_at
    from scripts.backtest.fq_us_volscale_study import unscaled_returns, volatility_signal

    result = dict(k=None, sigma_hat=None, sigma_ref=None, sigma_observations=0)
    if features is None or features['close'].empty:
        return result
    calendar = features['close'].index
    counts = features['eligible'].sum(axis=1)
    possible = counts.index[counts.ge(PARAMS['min_eligible'])]
    if possible.empty:
        return result
    start = calendar.get_loc(possible[0])
    # Match S1's first eligible date and 20-session decision clock, including
    # its warmup k=1 values. Unlike the study, retain the incomplete last period:
    # the live rebalance is at as_of, so u_d and k must include as_of's close,
    # rather than use k from the most recent historical schedule date.
    dates = calendar[start::PARAMS['rebalance_trading_days']]
    decisions = [decision_at(features, date) for date in dates]
    close = features['close'].ffill().loc[calendar[start:]]
    signal = volatility_signal(unscaled_returns(close, decisions)).iloc[-1]
    for key in ('k', 'sigma_hat', 'sigma_ref'):
        result[key] = float(signal[key]) if np.isfinite(signal[key]) else None
    result['sigma_observations'] = int(signal['sigma_observations'])
    return result


def _build_us_volscale_targets(features, perps, *, now=None):
    result = _build_us_targets(features, perps, now=now)
    base_weights = result['weights']
    result.update(_us_volscale_signal(features), base_rule=result['rule']['name'],
                  start_as_of=US_VOLSCALE_START_AS_OF, k_date=result['as_of'], weights={})
    result['rule'] = dict(result['rule'], name='mom_ls_invvol_volscale_v1')
    if result['as_of'] is not None and result['as_of'] < US_VOLSCALE_START_AS_OF:
        result.update(status='not_started', reason='before_start_as_of')
    elif result['status'] != 'ok':
        pass  # Preserve the base status and reason, with empty weights.
    elif any(result[key] is None for key in ('k', 'sigma_hat', 'sigma_ref')):
        # S1 reports k=1 during warmup; expose it for historical parity and
        # monitoring, but never let that placeholder authorize live targets.
        result.update(status='insufficient_universe', reason='volscale_signal_unavailable')
    else:
        result['weights'] = {s: round(w * result['k'], 6) for s, w in base_weights.items()}
    return result


def build_targets(market, live_scores_doc=None, panels=None, *, horizon=None, now=None,
                  root=ROOT, perp_map=None, rule='base') -> dict:
    """Build schema v1 from local US rows or supplied crypto snapshot/panels.

    ``horizon`` is an optional consistency assertion, not a rule override.
    Ties use the original panel column order. Listing age counts finite positive
    observed closes through as_of, rather than elapsed time since first quote.

    US ignores live_scores_doc and miner panels: its S1 features come directly
    from <root>/us/daily and the repository's frozen universe.
    """
    if rule not in ('base', 'volscale'):
        raise ValueError('rule must be base or volscale')
    if rule == 'volscale' and market != 'us':
        raise ValueError('volscale rule is US-only')
    if market == 'us':
        if horizon is not None and horizon != US_HORIZON:
            raise ValueError(f'horizon must match the fixed live selection ({US_HORIZON})')
        builder = _build_us_volscale_targets if rule == 'volscale' else _build_us_targets
        return builder(_load_us_features(root), _load_perp_map(perp_map), now=now)
    rule = _rule(market)
    generated = _utc(now)
    doc = live_scores_doc or {}
    h, entry = _scores_entry(market, doc)
    if horizon is not None and horizon != h:
        raise ValueError(f'horizon must match the fixed live selection ({h})')
    result = dict(version=1, market=market, status='no_validated_factors',
                  reason='no_core_scores', generated_at=generated.isoformat(),
                  as_of=doc.get('as_of'), run_id=doc.get('run_id'), horizon=h,
                  rule=rule, factor_ids=list(entry.get('factor_ids') or []),
                  universe_size=0, weights={}, excluded={}, pct={})
    if not entry:
        return result
    as_of = pd.Timestamp(doc.get('as_of'))
    if pd.isna(as_of):
        result.update(status='stale_scores', reason='missing_as_of')
        return result
    as_of = _utc(as_of).normalize()
    if (generated.normalize() - as_of).days > h + 2:
        result.update(status='stale_scores', reason='as_of_older_than_horizon_plus_2_days')
        return result
    date = as_of.tz_localize(None)
    if panels is None or date not in panels.mask.index or date not in panels.fields['close'].index:
        result.update(status='insufficient_universe', reason='as_of_missing_from_panels')
        return result
    # Slice FIRST: even history counts and return thresholds cannot see a later row.
    close = panels.fields['close'].loc[:date].reindex(columns=panels.mask.columns)
    close = close.where(np.isfinite(close) & close.gt(0))
    returns = close.tail(rule['vol_window'] + 1).pct_change(fill_method=None)
    vol = returns.rolling(rule['vol_window'], min_periods=rule['vol_window']).std(ddof=1).iloc[-1]
    scores = pd.Series({s: v.get('z', np.nan) for s, v in entry['scores'].items()}, dtype=float)
    scores = scores.reindex(close.columns)
    eligible = panels.mask.loc[date].fillna(False) & np.isfinite(scores) & np.isfinite(vol) & vol.gt(0)
    scores = scores[eligible]
    result['universe_size'] = len(scores)
    key = (lambda symbol: str(symbol) + 'BUSDT') if market == 'us' else str
    ranks = scores.rank(pct=True)
    for symbol in scores.index:
        pct = entry['scores'][symbol].get('pct', ranks[symbol])
        if pct is not None and np.isfinite(pct):
            result['pct'][key(symbol)] = round(float(pct), 6)
    if len(scores) < 15:
        result.update(status='insufficient_universe', reason='fewer_than_15_eligible_symbols')
        return result
    ordered = scores.sort_values(kind='stable')
    count = max(1, int(np.floor(len(ordered) * rule['quantile'])))
    longs, shorts = ordered.index[-count:], ordered.index[:count]
    if market == 'crypto':
        ret7 = close.loc[date] / close.reindex([date - dt.timedelta(days=7)]).iloc[0] - 1.
        ret7 = ret7.reindex(scores.index).where(lambda r: np.isfinite(r))
        threshold = ret7.quantile(rule['short_exclude_ret7_quantile'])
        history = close.notna().sum()
        for symbol in shorts:
            reasons = []
            if np.isfinite(ret7[symbol]) and ret7[symbol] >= threshold:
                reasons.append('short_ret7_top_decile')
            if history[symbol] < rule['min_listing_days']:
                reasons.append('short_listing_lt_90d')
            if reasons:
                result['excluded'][key(symbol)] = ','.join(reasons)
        shorts = shorts[~shorts.isin(result['excluded'])]
        if not len(shorts):
            result.update(status='insufficient_universe', reason='empty_short_leg_after_exclusions')
            return result
    weights = _capped_invvol(vol.loc[longs], rule['long_gross'], rule['max_name_weight'])
    if market == 'crypto':
        weights = pd.concat([weights, -_capped_invvol(vol.loc[shorts], rule['short_gross'],
                                                     rule['max_name_weight'])])
    result['weights'] = {key(s): round(float(w), 6) for s, w in weights.items() if round(float(w), 6)}
    result.update(status='ok', reason=None)
    return result


def _load_targets(market, root, *, panels=None, now=None, perp_map=None, rule='base'):
    if market == 'us':
        return build_targets(market, root=root, perp_map=perp_map, now=now, rule=rule)
    doc = store.read_json(Path(root) / market / 'live_scores.json')
    if panels is None and _scores_entry(market, doc)[1]:
        panels = panel.load_panels(market, root)
    return build_targets(market, doc, panels, now=now, rule=rule)


def write_targets(market, root, *, panels=None, now=None, perp_map=None, rule='base') -> Path:
    """Read local inputs and atomically replace the selected rule's target file."""
    result = _load_targets(market, root, panels=panels, now=now, perp_map=perp_map, rule=rule)
    filename = 'quant_targets_volscale.json' if rule == 'volscale' else 'quant_targets.json'
    path = Path(root) / market / filename
    # store uses a same-directory mkstemp, fsync, os.replace and finally cleanup.
    store.write_json(path, result)
    return path


def _load_frozen(root):
    base = Path(root) / 'crypto'
    raw = {name: (base / name).read_bytes() for name in ('ensemble.json', 'zoo.json')}
    ensemble_doc, zoo_doc = (json.loads(raw[name]) for name in ('ensemble.json', 'zoo.json'))
    ids = (ensemble_doc.get('horizons', {}).get('3') or {}).get('factor_ids') or []
    if not ids:
        raise ValueError('frozen crypto h=3 ensemble has no factors')
    by_id = {f['id']: f for f in zoo_doc['factors'] if f['horizon'] == 3}
    factors = [{k: by_id[i][k] for k in ('id', 'horizon', 'expr', 'direction')} for i in ids]
    frozen = {name: dict(sha256=hashlib.sha256(value).hexdigest(),
                         generated_at=json.loads(value).get('generated_at')) for name, value in raw.items()}
    for name in ('evaluate.py', 'expr.py'):
        frozen[name] = dict(sha256=hashlib.sha256(Path(__file__).with_name(name).read_bytes()).hexdigest())
    return factors, frozen


def _historical_targets(panels, scores, factors, dates):
    """Daily target stairs; non-ok snapshots retain the previous target."""
    protected = pd.DataFrame(np.nan, index=dates, columns=scores.columns)
    naive = protected.copy()
    signals = []
    for date in dates[::3]:
        row = scores.loc[date].where(panels.mask.loc[date])
        row = row[np.isfinite(row)]
        pct = row.rank(pct=True)
        entry = dict(factor_ids=[f['id'] for f in factors], scores={
            str(s): dict(z=round(float(z), 4), pct=round(float(pct[s]), 4)) for s, z in row.items()})
        snapshot = dict(as_of=date.strftime('%Y-%m-%d'), run_id='frozen-h3-study', horizons={'3': entry})
        target = build_targets('crypto', snapshot, panels, now=date + dt.timedelta(days=1))
        if target['status'] == 'ok':
            protected.loc[date] = pd.Series(target['weights']).reindex(scores.columns, fill_value=0.)
        # Comparator is the plain core-mask finite-score Q5-Q1: no vol gate,
        # short exclusions or cap. Both sleeves have the same 0.5 gross budget.
        ordered = pd.Series({s: v['z'] for s, v in entry['scores'].items()}, dtype=float).sort_values(kind='stable')
        naive.loc[date] = 0.
        if len(ordered) >= 2:
            count = max(1, int(np.floor(len(ordered) * 0.2)))
            naive.loc[date, ordered.index[-count:]] = 0.5 / count
            naive.loc[date, ordered.index[:count]] = -0.5 / count
        signals.append({k: target[k] for k in ('as_of', 'status', 'universe_size', 'weights', 'excluded')})
    return protected.ffill().fillna(0.), naive.ffill().fillna(0.), signals


def _frames_rates(panels, targets):
    # Drop never-held feeds only. This changes neither scoring nor selection.
    symbols = targets.columns[targets.ne(0).any()]
    if not len(symbols):
        symbols = targets.columns[:1]
    frames, rates = {}, {}
    for symbol in symbols:
        frame = pd.DataFrame({name: panels.fields[name][symbol] for name in
                              ('open', 'high', 'low', 'close', 'volume')}).loc[targets.index].dropna()
        if frame.empty:
            raise ValueError(f'no execution bars for {symbol}')
        frames[symbol] = frame
        funding = panels.fields.get('funding')
        rate = funding[symbol].reindex(targets.index).dropna() if funding is not None else pd.Series(dtype=float)
        rates[symbol] = {d.date(): float(v) for d, v in rate.items() if np.isfinite(v)}
    return frames, rates, targets.loc[:, symbols]


def backtest(root):
    """Descriptive only: freeze current members/directions, never tune them."""
    from .ensemble import ensemble_scores
    from scripts.backtest.quantjourney import CAPITAL, RebalancePolicy, run_weights

    factors, frozen = _load_frozen(root)
    panels = panel.load_panels('crypto', root)
    # The DSL operators are trailing/pointwise/cross-sectional only. Computing
    # the causal time series once is identical to scoring each prefix; the
    # builder still slices prices at each signal's as_of before every statistic.
    scores = ensemble_scores(factors, panels)
    result = dict(version=1, market='crypto', horizon=3, rule=_rule('crypto'),
                  generated_at=_utc(None).isoformat(), inputs=frozen, factors=factors,
                  panel=panels.meta, initial_capital=CAPITAL,
                  commission=0.0005, slippage=0.0003, intervals={})
    end = panels.mask.index[-1]
    for name, start, stop in (('val', '2025-05-04', '2026-04-30'),
                              ('forward', '2026-05-04', end)):
        dates = panels.mask.index[(panels.mask.index >= pd.Timestamp(start)) &
                                  (panels.mask.index <= pd.Timestamp(stop))]
        if len(dates) < 2:
            raise ValueError(f'not enough data for {name}')
        protected, naive, signals = _historical_targets(panels, scores, factors, dates)
        interval = dict(start=dates[0].strftime('%Y-%m-%d'), end=dates[-1].strftime('%Y-%m-%d'),
                        selection_bias=name == 'val', signals=signals, strategies={})
        exclusions = Counter(reason for signal in signals for reasons in signal['excluded'].values()
                             for reason in reasons.split(','))
        for strategy, targets in (('ls_invvol_protected_v1', protected), ('naive_equal_q5_q1', naive)):
            frames, rates, targets = _frames_rates(panels, targets)
            run = run_weights(frames, targets, market='crypto', commission=0.0005,
                              slippage=0.0003, funding=rates,
                              policy=RebalancePolicy(frequency=None, rebalance_on_signal_change=True))
            # Use simple daily-return Sharpe, matching the descriptive study's
            # convention; retain the engine's log-return Sharpe for auditing.
            metrics = run['metrics']
            metrics['sharpe_log'] = metrics['sharpe']
            metrics['sharpe'] = metrics['sharpe_simple_rf']
            metrics['short_excluded_count'] = (sum(len(s['excluded']) for s in signals)
                                                if strategy == 'ls_invvol_protected_v1' else 0)
            metrics['short_excluded_by_reason'] = dict(exclusions) if strategy == 'ls_invvol_protected_v1' else {}
            interval['strategies'][strategy] = run
        result['intervals'][name] = interval
    return result


def _report(result):
    lines = ['# 目标权重规则描述性回测', '',
             '仅离线读取本地数据；冻结当前 crypto h=3 ensemble 的成分、表达式及方向，无重选、调参或通过门槛。', '',
             '**val 有选择偏差；forward 是当前成分的历史重放，已被研究读取，也不是当时实际部署的独立前向记录。模拟盘才是本规则的前向检验。**', '',
             '## 两区间对照', '',
             '| 区间 | 规则 | 总收益 | 年化 | Sharpe | 最大回撤 | 累计换手 | 手续费 | 资金费净付出 | 空头排除次数 |',
             '| --- | --- | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: |']
    def fmt(value):
        return 'NA' if value is None else f'{value:.4f}'
    for name, interval in result['intervals'].items():
        for strategy, run in interval['strategies'].items():
            m = run['metrics']
            values = [name, strategy, fmt(m['return_pct']) + '%', fmt(m['cagr_pct']) + '%',
                      fmt(m['sharpe']), fmt(m['maxdd_pct']) + '%', fmt(m['turnover']),
                      fmt(m['commission_paid']), fmt(m['funding_paid']), str(m['short_excluded_count'])]
            lines.append('| ' + ' | '.join(values) + ' |')
    lines += ['', '## 口径与限制', '',
              '- 初始资金 1000，两个区间各自从空仓开始。区间首日收盘产生第一信号，次日开盘执行；每 3 个日历日重建目标，最后持有至区间末日收盘，不伪造平仓。',
              '- 信号来自 ensemble.ensemble_scores 的因果序列，z/pct 与 live_scores 一样分别四舍五入至 4 位；核心流动性池内按分数稳定排序，同分按面板列顺序。所有风险统计仅用信号日及之前数据。',
              '- 保护规则与 build_targets 共用代码；有效历史为有限正收盘价的观测天数。空头剔除不递补；不足 15 个合格标的或空头为空时不调仓。',
              '- 目标在两个信号之间保持阶梯不变；仅目标变化时调仓，没有每日漂移再平衡。实际股数持有不变，敞口会漂移；非 ok 信号保留上次目标。',
              '- 朴素对照：核心 mask 内有限分数的前后 20%，两腿各 0.5 等权；不作波动率门槛、空头排除和单币上限。',
              '- 累计换手为每笔成交绝对名义/成交前权益之和（双向口径）。手续费与资金费均为初始资金 1000 下的货币金额；资金费净付出为负代表收入。滑点已含净值，未计为手续费。',
              '- 年化=(末值/1000)^(365/日数)−1；Sharpe=简单日收益均值/样本标准差×√365（包含首个零收益日）；最大回撤包含初始资金。JSON 同时保存引擎对数收益 Sharpe。',
              '- 引擎按隔夜实际持仓×当日开盘×日均 funding×3 结算。缺失 funding 按引擎规则计零，下方报告覆盖；未复原逐次结算。',
              '- 缺价只前填估值，缺 open 不交易并保留原数量；没有自动延后重试。未模拟强平、灾难止损、最小订单、冲击或容量。当前存活币宇宙有幸存者偏差；固定成分在历史上尚未全部入选。',
              '- 单币上限 0.1 优先于腿预算；一腿候选全部触顶时接受较低敞口，因此剔除后并不保证精确净中性。', '',
              '## 冻结输入', '']
    for name, item in result['inputs'].items():
        lines.append(f'- {name}: sha256 `{item["sha256"]}`；generated_at={item.get("generated_at") or "原文件无此字段"}')
    lines += ['', '成分、表达式、方向、全部信号和原生净值保存于同名 JSON。', '', '## 区间审计', '']
    for name, interval in result['intervals'].items():
        lines.append(f'- {name}: {interval["start"]}～{interval["end"]}；信号 {len(interval["signals"])} 次；'
                     f'状态 {dict(Counter(s["status"] for s in interval["signals"]))}。')
        for strategy, run in interval['strategies'].items():
            m = run['metrics']
            lines.append(f'  - {strategy}: funding 覆盖 {m["funding_covered"]}/{m["funding_observations"]} 个持仓币日；'
                         f'排除原因计数 {m["short_excluded_by_reason"]}（同一币同日可有两种原因，总次数只计一次）。')
    return '\n'.join(lines) + '\n'


def _summary(doc):
    result = {k: doc[k] for k in ('market', 'status', 'reason', 'as_of', 'horizon', 'run_id',
                                  'universe_size', 'factor_ids', 'weights', 'excluded')}
    result.update(rule=doc['rule']['name'],
                  long_gross=round(sum(w for w in doc['weights'].values() if w > 0), 6),
                  short_gross=round(-sum(w for w in doc['weights'].values() if w < 0), 6))
    if doc['rule']['name'] == 'mom_ls_invvol_volscale_v1':
        result.update({key: doc[key] for key in US_VOLSCALE_FIELDS})
    return result


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--market', choices=('crypto', 'us'), default='crypto')
    parser.add_argument('--rule', choices=('base', 'volscale'), default='base',
                        help='volscale is the US momentum volatility-scaling variant')
    parser.add_argument('--root', type=Path, default=ROOT)
    parser.add_argument('--perp-map', type=Path, default=DEFAULT_PERP_MAP,
                        help='US version 2 perp map; relative paths resolve from the repo root')
    parser.add_argument('--write', action='store_true')
    parser.add_argument('--print', action='store_true', dest='print_summary')
    parser.add_argument('--backtest', action='store_true')
    parser.add_argument('--out', type=Path, default=Path('.quant_targets_out/backtest.md'))
    parser.add_argument('--json', type=Path, dest='json_path', default=Path('.quant_targets_out/backtest.json'))
    args = parser.parse_args(argv)
    if args.rule == 'volscale' and (args.market != 'us' or args.backtest):
        parser.error('--rule volscale requires --market us and cannot be combined with --backtest')
    if args.backtest:
        if args.market != 'crypto' or args.write:
            parser.error('--backtest is crypto-only and cannot be combined with --write')
        result = backtest(args.root)
        store.write_json(args.json_path, result)
        args.out.parent.mkdir(parents=True, exist_ok=True)
        args.out.write_text(_report(result), encoding='utf-8')
        print(f'Backtest reports: {args.out}, {args.json_path}')
    elif args.write:
        path = write_targets(args.market, args.root, perp_map=args.perp_map, rule=args.rule)
        if args.print_summary:
            print(json.dumps(_summary(store.read_json(path)), ensure_ascii=False, indent=2))
        else:
            print(path)
    else:
        print(json.dumps(_summary(_load_targets(args.market, args.root, perp_map=args.perp_map,
                                              rule=args.rule)),
                         ensure_ascii=False, indent=2))


if __name__ == '__main__':
    main()
