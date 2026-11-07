"""Frozen fq_us multi-feature ridge/RFF S1, with read-only local inputs."""
from __future__ import annotations

import argparse
from dataclasses import dataclass
from datetime import datetime, timezone
import hashlib
import json
import math
from pathlib import Path
import sys
import time

import numpy as np
import pandas as pd

from . import ml_multifeature_study as core
from . import us_broad_classic_study as broad
from .expr import evaluate_expr
from scripts.backtest import evolution_ledger
from scripts.backtest import fq_us_momentum_study as momentum
from scripts.backtest.incremental_filter import z_threshold

PREREG = 'docs/research/2026-11-07-fq-us-ml-multifeature-prereg.md'
REPO = Path(__file__).resolve().parents[2]
FEATURE_HASH = '987f53daa50c6f828f568b2794ef8a6470608a4a4ed084f406870d7233bf9e4c'
FEATURES = tuple((n, e) for n, e in core.FEATURES
                 if not n.startswith(('fund_', 'prem_')) and n not in ('beta_60', 'ivol_30'))
CUSTOM = ('beta_60_spy', 'ivol_30_spy', 'mom12_1', 'mom6_1', 'high52')
H, STRIDE, EMBARGO, MIN_U, MIN_T = 20, 5, 22, 30, 15
TRAIN_START, TRAIN_END, REFIT_END = '2012-11-01', '2021-12-31', '2024-12-31'
DEVIATIONS = [
    '宽池 guard_inputs 不能跳过已漂移的 live bstock_daily；仅复用其哈希函数与三个 U 冻结值，T 另用 momentum.guard_hashes，未读取 live 数据。',
    '不修改共享模块：rank_map 外层先按 U=30/T=15 屏蔽不足日期；标签按有限标签至少30只屏蔽，仍调用原 rank_map(label=True)。',
    '抽样按完整 SPY 日历：从首个可用且标签至少30只的训练日起每5个交易日取点；后续不可用日期跳过，不重置相位。标签终点先按完整日历 t+20 清洗。',
    '非有限特征比例按训练信息清洗后的全部 U 训练可用格计算，包含未被每5日抽样选中的日期，不按标签有限性筛格。',
    '日差采用周期 (起点,终点]，与 B 收盘成交扣费后的周期边界权益比一致，边界日仅归前一周期；最后不足20日不纳入。',
    '严格按周期起点归段；主回测沿用 B 全历史日程及账户状态，2022年前周期不判定。冻结模型在训练期内的回放含样本内信息，历史袖权益漂移会传递到判定段。',
    'IC 的 t 复用 ML 核心 ic_summary 的普通日均标准误，标签20日重叠，因此仅描述，不作显著性判定；训练IC涵盖清洗后全部训练日，非仅抽样日。',
    '只做多描述性变体分别运行 R 和 RF（与 B 的 L 配对）；重训 R′/RF′仅从首个前向周期起点开始，两腿各50000，比较同日前向重启的 B。',
    '隔离账本 K 由 append_event 按实际行数计算，不伪造历史行；判定使用预注册 K=6/7，另存 preregistered_K。',
    '研究日志由并行主代理维护，本任务只新增研究脚本和合成测试，报告与隔离账本写入指定输出目录。',
]


@dataclass
class Pool:
    fields: dict
    mask: pd.DataFrame
    spy: pd.Series


def guard_features(features=None):
    rows = FEATURES if features is None else features
    digest = hashlib.sha256(''.join(f'{n}\t{e}\n' for n, e in rows).encode()).hexdigest()
    if digest != FEATURE_HASH:
        raise ValueError('feature hash mismatch; refusing to run')
    return digest


def guard_inputs(broad_root, snapshot, allow_input_drift=False):
    root = Path(broad_root)
    paths = dict(broad_constituents=[root / 'constituents_20261105.json'],
                 broad_daily=(root / 'daily').glob('*.json'), spy=[root / 'benchmark/SPY.json'])
    hashes = {k: broad.concatenated_sha256(v) for k, v in paths.items()}
    drift = {k: dict(expected=broad.EXPECTED_HASHES[k], actual=v)
             for k, v in hashes.items() if v != broad.EXPECTED_HASHES[k]}
    if drift and not allow_input_drift:
        raise ValueError(f'frozen U input hash mismatch: {drift}')
    t_hashes, t_drift = momentum.guard_hashes(snapshot, allow_input_drift)
    hashes.update({'T.' + k: v for k, v in t_hashes.items()})
    drift.update({'T.' + k: v for k, v in t_drift.items()})
    return hashes, drift


def read_full(path):
    raw = pd.DataFrame(json.loads(Path(path).read_text()),
                       columns=['date', 'open', 'high', 'low', 'close', 'volume', 'typical'])
    raw.index = pd.DatetimeIndex(pd.to_datetime(raw.pop('date')))
    if raw.empty or raw.index.has_duplicates or not raw.index.is_monotonic_increasing:
        raise ValueError(f'invalid daily dates: {path}')
    raw = raw.astype(float)
    # Reuse the broad reader and B's stricter bar validation.
    cv = broad.read_daily(path)
    raw[['close', 'volume']] = cv[['close', 'volume']]
    return raw.where(momentum.valid_rows(raw), np.nan)


def make_pool(frames, names, calendar, spy, mask):
    fields = {key: pd.DataFrame({s: frames[s][source] for s in names}).reindex(calendar)
              for key, source in [('open', 'open'), ('high', 'high'), ('low', 'low'),
                                  ('close', 'close'), ('volume', 'volume'), ('vwap', 'typical')]}
    fields['ret'] = fields['close'].pct_change(fill_method=None)
    return Pool(fields, mask, spy.reindex(calendar))


def load_pools(broad_root, snapshot, frozen=True):
    root, snapshot = Path(broad_root), Path(snapshot)
    members = {s.replace('.', '-') for s in json.loads(
        (root / 'constituents_20261105.json').read_text())['sp500']}
    uframes = {p.stem: read_full(p) for p in sorted((root / 'daily').glob('*.json'))}
    if not set(uframes) <= members or (frozen and len(uframes) != 498):
        raise ValueError('U composition disagrees with frozen constituents')
    spy = read_full(root / 'benchmark/SPY.json').close.dropna()
    unames = sorted(uframes)
    ufeatures = momentum.build_features(uframes, unames, spy.index)
    umask = broad.eligibility_mask(ufeatures['close'], pd.DataFrame(
        {s: uframes[s].volume for s in unames}).reindex(spy.index))
    pd.testing.assert_frame_equal(umask, ufeatures['eligible'], check_exact=True)
    u = make_pool(uframes, unames, spy.index, spy, umask)
    names, perps, frames, calendar, quality = momentum.load_snapshot(snapshot)
    features = momentum.build_features(frames, names, calendar)
    full_t = {}
    for s in names:
        full_t[s] = read_full(snapshot / 'daily' / f'{s}.json')
        for column in ('open', 'high', 'low', 'close', 'volume'):
            pd.testing.assert_series_equal(full_t[s][column], frames[s][column], check_names=False)
    t = make_pool(full_t, names, calendar, frames['SPY'].close, features['eligible'])
    return u, t, features, frames, calendar, dict(U=len(unames), T=len(names),
        missing_constituents=sorted(members - uframes.keys()),
        unmapped=[s for s in names if not perps[s]], data_quality=quality)


def custom_features(close, spy):
    returns = close.pct_change(fill_method=None)
    spy_returns = spy.reindex(close.index).pct_change(fill_method=None)
    covariance = returns.rolling(60, min_periods=60).cov(spy_returns, ddof=1)
    variance = spy_returns.rolling(60, min_periods=60).var(ddof=1)
    return dict(beta_60_spy=covariance.div(variance.where(variance.gt(0)), axis=0),
                ivol_30_spy=returns.sub(spy_returns, axis=0).rolling(30, min_periods=30).std(ddof=1),
                mom12_1=close.shift(21) / close.shift(252) - 1,
                mom6_1=close.shift(21) / close.shift(126) - 1,
                high52=close / close.rolling(252, min_periods=252).max())


def rank_map(values, mask, *, label=False, min_names=MIN_U):
    count = (mask & np.isfinite(values)).sum(axis=1) if label else mask.sum(axis=1)
    use = mask & count.ge(min_names).to_numpy()[:, None]
    return core.rank_map(values, use, label=label)


def train_window(calendar, end=TRAIN_END):
    positions = np.arange(len(calendar))
    end_position = calendar.searchsorted(pd.Timestamp(end), side='right') - 1
    return np.asarray((calendar >= TRAIN_START) & (positions + H <= end_position))


def sampled_dates(calendar, mask, labels, end=TRAIN_END):
    eligible = mask.sum(axis=1).ge(MIN_U).to_numpy()
    labels_ok = (mask & np.isfinite(labels)).sum(axis=1).ge(MIN_U).to_numpy()
    usable = train_window(calendar, end) & eligible & labels_ok
    available = np.flatnonzero(usable)
    chosen = np.zeros(len(calendar), bool)
    if len(available):
        chosen[np.arange(available[0], len(calendar), STRIDE)] = True
    return chosen & usable


def feature_panels(pool, min_names, progress=lambda _: None, inspect_train=False):
    guard_features()
    names = [n for n, _ in FEATURES] + list(CUSTOM)
    cube = np.empty((*pool.mask.shape, len(names)))
    shares, styles = {}, {}
    use = pool.mask.to_numpy() & train_window(pool.mask.index)[:, None]
    use &= pool.mask.sum(axis=1).ge(MIN_U).to_numpy()[:, None]
    denominator = int(use.sum())
    custom = custom_features(pool.fields['close'], pool.spy)
    for j, name in enumerate(names):
        progress(f'特征 {j + 1}/35：{name}')
        value = evaluate_expr(FEATURES[j][1], pool.fields) if j < len(FEATURES) else custom[name]
        if inspect_train:
            if denominator == 0:
                raise ValueError('empty U train-eligible cells')
            shares[name] = float((use & ~np.isfinite(value.to_numpy())).sum() / denominator)
        cube[:, :, j] = rank_map(value, pool.mask, min_names=min_names).to_numpy()
        if name in ('mom12_1', 'vol_30', 'beta_60_spy', 'dvol_30'):
            styles[name] = value
    return cube, names, shares, styles


def training_sample(cube, pool, end=TRAIN_END):
    raw = pool.fields['close'].shift(-H) / pool.fields['close'] - 1
    labels = rank_map(raw, pool.mask, label=True)
    dates = sampled_dates(pool.mask.index, pool.mask, labels, end)
    rows, stocks = np.nonzero(pool.mask.to_numpy() & np.isfinite(labels.to_numpy()) & dates[:, None])
    return cube[rows, stocks], labels.to_numpy()[rows, stocks], rows


def cv_indices(sample_positions):
    """Embargo uses full-calendar positions, never sampled-date distances."""
    positions = np.asarray(sample_positions)
    if len(positions) < core.FOLDS or np.any(np.diff(positions) <= 0):
        raise ValueError('at least five distinct ascending sampled dates required')
    folds = []
    for evaluation in np.array_split(np.arange(len(positions)), core.FOLDS):
        train = np.flatnonzero((positions < positions[evaluation[0]] - EMBARGO) |
                               (positions > positions[evaluation[-1]] + EMBARGO))
        if not len(train):
            raise ValueError('empty US CV training fold')
        folds.append((train, evaluation))
    return folds


def fit_pair(x, y, dates, omega=None, progress=print, *, noise_labels=None):
    """US folds around the shared ML sufficient-statistic and scoring core."""
    unique, codes = np.unique(dates, return_inverse=True)
    noise = core.permute_labels(y, dates) if noise_labels is None else noise_labels
    targets = np.column_stack((y, noise))
    progress(f'全训练 Gram：N={len(x)}，P={x.shape[1] if omega is None else 2 * omega.shape[1]}')
    full_g, full_rhs = core.gram(x, targets, omega)
    scale = float(np.trace(full_g) / len(x) / len(full_g))
    lambdas = np.asarray(core.MULTIPLIERS) * scale
    daily = [[[] for _ in lambdas] for _ in range(2)]
    records = []
    for k, (train, evaluation) in enumerate(cv_indices(unique), 1):
        tr, ev = np.isin(codes, train), np.isin(codes, evaluation)
        excluded = ~tr & ~ev
        progress(f'CV {k}/5：训练 {tr.sum()} 行，评估 {ev.sum()} 行，22交易日隔离 {excluded.sum()} 行')
        held_g, held_rhs = core.gram(x[ev], targets[ev], omega)
        embargo_g, embargo_rhs = core.gram(x[excluded], targets[excluded], omega)
        g, rhs = full_g - held_g - embargo_g, full_rhs - held_rhs - embargo_rhs
        del held_g, embargo_g
        betas = core.spectral_ridge(g, rhs, int(tr.sum()), lambdas)
        del g
        predictions = core.predict(x[ev], betas, omega)
        for a in range(2):
            for j in range(len(lambdas)):
                daily[a][j].append(core.daily_ic_rows(predictions[:, j, a], targets[ev, a], dates[ev]))
        records.append(dict(train_positions=unique[train].tolist(), eval_positions=unique[evaluation].tolist(),
                            embargo_positions=np.unique(dates[excluded]).tolist(),
                            train_rows=int(tr.sum()), eval_rows=int(ev.sum())))
        progress(f'CV {k}/5 完成')
    cvs = [[dict(multiplier=m, **{'lambda': float(lam)}, ic=float(pd.concat(daily[a][j]).mean()),
                 n_days=len(pd.concat(daily[a][j]))) for j, (m, lam) in enumerate(zip(core.MULTIPLIERS, lambdas))]
           for a in range(2)]
    chosen = [core.choose_lambda(cv) for cv in cvs]
    fits = core.spectral_ridge(full_g, full_rhs, len(x), [c['lambda'] for c in chosen])
    return [dict(beta=fits[:, a, a].tolist(), chosen_lambda=chosen[a]['lambda'],
                 scale=scale, cv=cvs[a], cv_folds=records, n_train=len(x), n_train_dates=len(unique))
            for a in range(2)]


def score_panels(cube, mask, betas, omega=None, min_names=MIN_T):
    valid = mask.to_numpy() & mask.sum(axis=1).ge(min_names).to_numpy()[:, None]
    rows, stocks = np.nonzero(valid)
    values = core.predict(cube[rows, stocks], np.column_stack(betas), omega)
    scores = []
    for j in range(values.shape[1]):
        result = np.full(mask.shape, np.nan)
        result[rows, stocks] = values[:, j]
        scores.append(pd.DataFrame(result, index=mask.index, columns=mask.columns))
    return scores


def signal_decisions(features, dates, signal):
    replacement = dict(features, signal=signal)
    return [momentum.decision_at(replacement, date) for date in dates]


def run_arm(frames, sessions, decisions, progress, name, long_only=False):
    progress(f'{name}：Backtrader 多头袖')
    long = momentum.run_sleeve(frames, sessions, {r['date']: r['L' if long_only else 'long'] for r in decisions},
                               cash=100_000. if long_only else 50_000., commission=momentum.PARAMS['long_cost_per_fill'])
    if long_only:
        return dict(equity_curve=long['equity_curve'], fills=long['fills'], legs=dict(long=long),
                    period_table=momentum.period_table(long['equity_curve'], decisions))
    progress(f'{name}：Backtrader 空头袖')
    short = momentum.run_sleeve(frames, sessions, {r['date']: r['short'] for r in decisions},
                                cash=50_000., commission=momentum.PARAMS['short_cost_per_fill'], short=True)
    curve = [dict(date=a['date'], equity=a['equity'] + b['equity'])
             for a, b in zip(long['equity_curve'], short['equity_curve'], strict=True)]
    return dict(equity_curve=curve, fills=long['fills'] + short['fills'], legs=dict(long=long, short=short),
                period_table=momentum.period_table(curve, decisions))


def segment_of(start):
    date = pd.Timestamp(start)
    return 'excluded' if date < pd.Timestamp('2022-01-01') else 'val' if date < pd.Timestamp('2025-01-01') else 'forward'


def period_days(calendar, periods):
    result = pd.DataFrame(index=calendar, columns=['period', 'segment'])
    for p in periods:
        use = (calendar > pd.Timestamp(p['start'])) & (calendar <= pd.Timestamp(p['end']))
        result.loc[use, ['period', 'segment']] = [p['k'], segment_of(p['start'])]
    return result.dropna()


def curve_returns(curve):
    series = pd.Series({pd.Timestamp(r['date']): r['equity'] for r in curve}).sort_index()
    return series.pct_change(fill_method=None)


def cluster_statistics(delta, clusters):
    frame = pd.DataFrame(dict(delta=delta, cluster=clusters)).dropna()
    if frame.empty:
        return dict(n_days=0, n_periods=0, mean_delta=None, se=None, z=None)
    if not np.isfinite(frame.delta).all():
        raise ValueError('non-finite daily paired return')
    mean = float(frame.delta.mean())
    sums = (frame.delta - mean).groupby(frame.cluster).sum()
    se = float(np.sqrt(np.square(sums).sum()) / len(frame))
    z = mean / se if se else math.copysign(math.inf, mean) if mean else None
    return dict(n_days=len(frame), n_periods=len(sums), mean_delta=mean, se=se, z=z)


def paired_statistics(arm, baseline, membership):
    delta = curve_returns(arm) - curve_returns(baseline)
    use = membership.loc[membership.segment.isin(['val', 'forward'])]
    stats = cluster_statistics(delta.reindex(use.index), use.period)
    stats['segment_delta'] = {s: float(delta.reindex(use.index[use.segment == s]).mean())
                              if (use.segment == s).any() else None for s in ('val', 'forward')}
    return stats


def verdict(n, shares, bad_prediction, z, z_noise, val_delta, forward_delta, z_k):
    if n < 30 or any(v > .5 for v in shares.values()) or bad_prediction:
        return 'invalid'
    if (z is not None and z_noise is not None and val_delta is not None and forward_delta is not None
            and val_delta > 0 and forward_delta > 0 and z_noise >= 1.):
        if z >= z_k:
            return 'strong'
        if z >= 1.:
            return 'screen_pass'
    return 'fail'


def panel_ic(score, label, mask, visible):
    rows, stocks = np.nonzero(mask.to_numpy() & np.asarray(visible)[:, None])
    daily = core.daily_ic_rows(score.to_numpy()[rows, stocks], label.to_numpy()[rows, stocks], rows)
    return dict(**core.ic_summary(daily), daily={str(score.index[int(d)].date()): float(v) for d, v in daily.items()})


def segment_metrics(run, membership):
    result = {}
    for segment in ('val', 'forward'):
        dates = membership.index[membership.segment == segment]
        if dates.empty:
            result[segment] = None
            continue
        def metrics(curve, fills):
            first = pd.Timestamp(dates[0])
            before = [r for r in curve if pd.Timestamp(r['date']) < first]
            start_equity = before[-1]['equity']
            chosen = [r for r in curve if pd.Timestamp(r['date']) in dates]
            selected_fills = [r for r in fills if pd.Timestamp(r['date']) in dates]
            m = momentum.path_metrics(chosen, start_equity, selected_fills)
            ret = curve_returns(curve).reindex(dates)
            sd = float(ret.std(ddof=1))
            m['sharpe'] = float(ret.mean() / sd * np.sqrt(252)) if sd else None
            return m
        result[segment] = metrics(run['equity_curve'], run['fills'])
        result[segment]['legs'] = {leg: metrics(r['equity_curve'], r['fills']) for leg, r in run['legs'].items()}
    return result


def append_ledger(path, result):
    events = []
    for arm, k, idea in [('R', 6, 'fq-us-ml-ridge-v1'), ('RF', 7, 'fq-us-ml-rff-v1')]:
        def build(_k, _previous):
            return dict(result['paired'][arm], stage='S1', change_type='E', idea_id=idea,
                        baseline_version='fq_us:mom_ls_invvol_v1', preregistered_K=k,
                        chosen_lambda=result['models'][arm]['chosen_lambda'],
                        input_hashes=result['hashes'], feature_hash=FEATURE_HASH, prereg_path=PREREG,
                        deviations=result['deviations'])
        events.append(evolution_ledger.append_event(path, 'fq_us', build))
    return events


def validate_outputs(broad_root, snapshot, out, ledger):
    roots = [Path(p).resolve() for p in (broad_root, snapshot)]
    out = Path(out).resolve()
    paths = [out, *[(out / n).resolve() for n in ('report.json', 'report.md', 'attempts.jsonl', 'daily_returns.csv')]]
    if ledger is not None:
        ledger = Path(ledger).resolve()
        if ledger in paths:
            raise ValueError('ledger must not overwrite a study output')
        paths.append(ledger)
    if any(path.is_relative_to(root) for path in paths for root in roots):
        raise ValueError('output must not be inside read-only input directories')
    return out


def run_study(broad_root, snapshot, out, allow_input_drift=False, progress=print):
    hashes, drift = guard_inputs(broad_root, snapshot, allow_input_drift)
    guard_features()
    u, t, bfeatures, frames, calendar, composition = load_pools(broad_root, snapshot, frozen=not drift)
    progress(f'输入哈希通过：U={u.mask.shape}；T={t.mask.shape}')
    ucube, names, shares, _ = feature_panels(u, MIN_U, progress, inspect_train=True)
    tcube, _, _, styles = feature_panels(t, MIN_T, progress)
    x, y, dates = training_sample(ucube, u)
    progress(f'训练样本冻结：{len(x)} 行 / {len(np.unique(dates))} 日期；最后信号日 {u.mask.index[dates[-1]].date()}')
    omega = core.random_weights(p=35)
    noise_labels = core.permute_labels(y, dates)
    models = {}
    for arm, noise, weights in [('R', 'N_R', None), ('RF', 'N_RF', omega)]:
        progress(f'{arm}/{noise}：拟合与选 λ')
        fitted = fit_pair(x, y, dates, weights, progress, noise_labels=noise_labels)
        models.update({arm: fitted[0], noise: fitted[1]})
    del x, y, noise_labels
    scores, ic = {}, {}
    for arm, noise, weights in [('R', 'N_R', None), ('RF', 'N_RF', omega)]:
        progress(f'{arm}/{noise}：U/T 逐日预测与 IC（描述）')
        for pool_name, pool, cube, minimum in [('U', u, ucube, MIN_U), ('T', t, tcube, MIN_T)]:
            predicted = score_panels(cube, pool.mask, [models[a]['beta'] for a in (arm, noise)], weights, minimum)
            expected = pool.mask & pool.mask.sum(axis=1).ge(minimum).to_numpy()[:, None]
            label = rank_map(pool.fields['close'].shift(-H) / pool.fields['close'] - 1,
                             pool.mask, label=True, min_names=minimum)
            for a, score in zip((arm, noise), predicted):
                models[a].setdefault('nonfinite_predictions', {})[pool_name] = int((expected & ~np.isfinite(score)).sum().sum())
                ic.setdefault(a, {})[pool_name] = {}
                for segment in (('train', 'val', 'forward') if pool_name == 'U' else ('val', 'forward')):
                    visible = (train_window(pool.mask.index) if segment == 'train' else
                               (pool.mask.index >= '2022-01-01') & (pool.mask.index <= '2024-12-31') if segment == 'val' else
                               pool.mask.index >= '2025-01-01')
                    ic[a][pool_name][segment] = panel_ic(score, label, expected, visible)
                if pool_name == 'T':
                    scores[a] = score
    progress('B：调用未修改的 momentum.run_study（含 L 与两个原基准）')
    reference = momentum.run_study(snapshot, allow_input_drift)
    sessions, bdecisions = momentum.rebalance_schedule(bfeatures, calendar)
    stock_frames = {s: frames[s] for s in t.mask.columns}
    baseline = dict(equity_curve=reference['equity_curve'], fills=reference['legs']['long']['fills'] + reference['legs']['short']['fills'],
                    legs=reference['legs'], period_table=reference['period_table'])
    progress('B：独立复用原决策/两袖路径，核对完整周期表逐项相等')
    repeated_b = run_arm(stock_frames, sessions, bdecisions, progress, 'B复现')
    assert repeated_b['period_table'] == reference['period_table'], 'B period table differs from run_study'
    assert repeated_b['equity_curve'] == reference['equity_curve'], 'B equity differs from run_study'
    del repeated_b
    membership = period_days(sessions, baseline['period_table'])
    results, decisions = dict(B=baseline), dict(B=bdecisions)
    for a in ('R', 'RF', 'N_R', 'N_RF'):
        if any(models[a]['nonfinite_predictions'].values()):
            # Keep the invalid audit and use cash on affected dates for descriptive execution.
            clean = scores[a].copy()
            bad_dates = (t.mask & ~np.isfinite(clean)).any(axis=1)
            safe_features = dict(bfeatures, eligible=bfeatures['eligible'] & ~bad_dates.to_numpy()[:, None])
        else:
            clean, safe_features = scores[a], bfeatures
        decisions[a] = signal_decisions(safe_features, [pd.Timestamp(d['date']) for d in bdecisions], clean)
        results[a] = run_arm(stock_frames, sessions, decisions[a], progress, a)
    paired = {}
    for a, noise, k in [('R', 'N_R', 6), ('RF', 'N_RF', 7)]:
        p = paired_statistics(results[a]['equity_curve'], baseline['equity_curve'], membership)
        p['noise'] = paired_statistics(results[a]['equity_curve'], results[noise]['equity_curve'], membership)
        p.update(z_noise=p['noise']['z'], z_K=z_threshold(k), preregistered_K=k)
        p['verdict'] = verdict(p['n_periods'], shares, any(models[a]['nonfinite_predictions'].values()),
                               p['z'], p['z_noise'], p['segment_delta']['val'], p['segment_delta']['forward'], p['z_K'])
        paired[a] = p
    descriptive = {a: segment_metrics(run, membership) for a, run in results.items()}
    similarities = {}
    for a, score in scores.items():
        similarities[a] = {}
        for segment in ('val', 'forward'):
            visible = t.mask.index.isin(membership.index[membership.segment == segment])
            correlations = {style: panel_ic(score, value, t.mask, visible) for style, value in styles.items()}
            overlaps = {leg: [] for leg in ('long', 'short')}
            for d, b in zip(decisions[a][:-1], bdecisions[:-1], strict=True):
                if segment_of(b['date']) == segment:
                    for leg in overlaps:
                        if b[leg]:
                            overlaps[leg].append(len(set(d[leg]) & set(b[leg])) / len(b[leg]))
            similarities[a][segment] = dict(correlations=correlations, basket_overlap={
                leg: float(np.mean(values)) if values else None for leg, values in overlaps.items()})
    long_only = {}
    for a in ('R', 'RF'):
        run = run_arm(stock_frames, sessions, decisions[a], progress, a + '/L', long_only=True)
        long_only[a] = dict(paired=paired_statistics(run['equity_curve'], reference['L']['equity_curve'], membership),
                            metrics=segment_metrics(run, membership), backtest=run)
    long_only['B'] = dict(metrics=segment_metrics(dict(equity_curve=reference['L']['equity_curve'],
        fills=reference['L']['fills'], legs=dict(long=reference['L'])), membership))
    progress('R′/RF′：训练加验证期清洗、固定原 λ 重训；仅回测前向完整周期')
    xx, yy, dd = training_sample(ucube, u, REFIT_END)
    forward_decisions = [d for d in bdecisions if segment_of(d['date']) == 'forward']
    if len(forward_decisions) < 2:
        raise ValueError('not enough complete forward periods for descriptive refits')
    forward_sessions = sessions[sessions >= pd.Timestamp(forward_decisions[0]['date'])]
    restarted_b = run_arm(stock_frames, forward_sessions, forward_decisions, progress, '前向重启B')
    forward_membership = period_days(forward_sessions, restarted_b['period_table'])
    refits = {}
    for a, weights in [('R', None), ('RF', omega)]:
        progress(a + '′：累计 Gram，固定 λ 求解')
        g, rhs = core.gram(xx, yy[:, None], weights)
        beta = core.spectral_ridge(g, rhs, len(xx), [models[a]['chosen_lambda']])[:, 0, 0]
        del g, rhs
        visible_mask = t.mask & (t.mask.index >= forward_sessions[0])[:, None]
        score = score_panels(tcube, visible_mask, [beta], weights)[0]
        dec = signal_decisions(bfeatures, [pd.Timestamp(d['date']) for d in forward_decisions], score)
        run = run_arm(stock_frames, forward_sessions, dec, progress, a + '′')
        refits[a + '_prime'] = dict(chosen_lambda=models[a]['chosen_lambda'], beta=beta.tolist(),
            n_train=len(xx), n_train_dates=len(np.unique(dd)), last_signal_date=str(u.mask.index[dd[-1]].date()),
            nonfinite_predictions=int((visible_mask & ~np.isfinite(score)).sum().sum()),
            paired=paired_statistics(run['equity_curve'], restarted_b['equity_curve'], forward_membership),
            metrics=segment_metrics(run, forward_membership), backtest=run)
    if guard_inputs(broad_root, snapshot, allow_input_drift)[0] != hashes:
        raise ValueError('inputs changed during study')
    daily = pd.DataFrame({a: curve_returns(r['equity_curve']) for a, r in results.items()}).join(membership)
    daily.to_csv(Path(out) / 'daily_returns.csv', index_label='date')
    coefficients = sorted([dict(feature=n, beta=float(b)) for n, b in zip(names, models['R']['beta'], strict=True)],
                          key=lambda r: -abs(r['beta']))
    deviations = DEVIATIONS + ['B 原执行口径：' + d for d in reference['deviations']]
    if drift:
        deviations.append('显式允许输入漂移；本次不再构成冻结输入复现。')
    return dict(prereg_path=PREREG, prereg_sha256=hashlib.sha256((REPO / PREREG).read_bytes()).hexdigest(),
        hashes=hashes, feature_hash=FEATURE_HASH, input_drift=drift, composition=composition,
        parameters=dict(P=35, Q=core.Q, horizon=H, stride=STRIDE, embargo_trading_days=EMBARGO,
                        train_start=TRAIN_START, train_end=TRAIN_END, refit_end=REFIT_END, execution=momentum.PARAMS),
        feature_names=names, nonfinite_shares=shares, models=models, paired=paired, ic=ic,
        segments=descriptive, similarities=similarities, coefficients=coefficients,
        long_only=long_only, refits=refits, refit_baseline=restarted_b,
        backtests=results, decisions=decisions, baseline_L=reference['L'],
        reproduction=dict(period_table_exact_match=True, daily_equity_exact_match=True),
        deviations=deviations, disclosure='共跑了 R、RF、N_R、N_RF、R′、RF′ 六组，外加只做多变体（R/L、RF/L）；K只计算R和RF。')


def number(value, digits=6):
    return 'NA' if value is None else f'{value:.{digits}f}'


def render_report(r):
    lines = ['# fq_us 多特征机器学习 S1 冻结研究', '', r['disclosure'], '',
             '数据已被既有研究看过，两池均有幸存者偏差；本研究不构成样本外证据，只筛查相对 B 的增量。',
             f'启动 {r["attempt"]} 次；耗时 {r["elapsed_seconds"]:.1f} 秒；B 完整周期表及每日权益严格复现通过。', '',
             '| 臂 | 判定 | z | z_noise | z_K | 验证日均Δ | 前向日均Δ | λ | 周期数 |',
             '|---|---|---:|---:|---:|---:|---:|---:|---:|']
    for a, p in r['paired'].items():
        lines.append('| ' + ' | '.join([a, p['verdict'], number(p['z']), number(p['z_noise']), number(p['z_K']),
            number(p['segment_delta']['val'], 9), number(p['segment_delta']['forward'], 9),
            number(r['models'][a]['chosen_lambda'], 12), str(p['n_periods'])]) + ' |')
    lines += ['', '## IC（描述；普通 t 未校正20日标签重叠）', '',
              '| 臂 | U训练 IC/t/n | U验证 IC/t/n | U前向 IC/t/n | T验证 IC/t/n | T前向 IC/t/n |',
              '|---|---|---|---|---|---|']
    for a, pools in r['ic'].items():
        cells = [pools[p][s] for p, s in [('U', 'train'), ('U', 'val'), ('U', 'forward'), ('T', 'val'), ('T', 'forward')]]
        lines.append('| ' + a + ' | ' + ' | '.join(f'{number(c["mean"])} / {number(c["t"], 3)} / {c["n"]}' for c in cells) + ' |')
    lines += ['', '## λ 交叉验证表（训练期固定尺度，噪声独立选 λ）', '',
              '| 臂 | 倍数 | λ | CV 日均IC | 日数 |', '|---|---:|---:|---:|---:|']
    for a, m in r['models'].items():
        for c in m['cv']:
            lines.append(f'| {a} | {c["multiplier"]} | {number(c["lambda"], 12)} | {number(c["ic"])} | {c["n_days"]} |')
    lines += ['', '## 分段回测（描述）', '', '| 臂 | 段 | 净收益 | 夏普 | 最大回撤 | 双边换手 | 多头收益 | 空头收益 |',
              '|---|---|---:|---:|---:|---:|---:|---:|']
    for a, segments in r['segments'].items():
        for s, m in segments.items():
            if m:
                lines.append('| ' + ' | '.join([a, s, number(m['total_return']), number(m['sharpe']),
                    number(m['max_drawdown']), number(m['turnover']), number(m['legs']['long']['total_return']),
                    number(m['legs']['short']['total_return'])]) + ' |')
    lines += ['', '## 与动量及风格的相似度（描述）', '',
              '| 臂 | 段 | mom12_1 | vol_30 | beta_60_spy | dvol_30 | 多头重合 | 空头重合 |',
              '|---|---|---:|---:|---:|---:|---:|---:|']
    for a, segments in r['similarities'].items():
        for s, m in segments.items():
            lines.append('| ' + ' | '.join([a, s] + [number(m['correlations'][key]['mean'])
                for key in ('mom12_1', 'vol_30', 'beta_60_spy', 'dvol_30')] +
                [number(m['basket_overlap'][key]) for key in ('long', 'short')]) + ' |')
    lines += ['', '## 只做多与重训（描述，不参与判定）', '']
    for a in ('R', 'RF'):
        p = r['long_only'][a]['paired']
        lines.append(f'- {a}/L − B/L：z={number(p["z"])}；两段日均Δ={p["segment_delta"]}。')
    for a, refit in r['refits'].items():
        p = refit['paired']
        lines.append(f'- {a}：固定λ={number(refit["chosen_lambda"],12)}；前向 z={number(p["z"])}；日均Δ={number(p["segment_delta"]["forward"],9)}；净收益={number(refit["metrics"]["forward"]["total_return"])}。')
    lines += ['', '## R 的35个系数（按绝对值排序）', '', '| 特征 | β |', '|---|---:|']
    lines += [f'| {c["feature"]} | {number(c["beta"],9)} |' for c in r['coefficients']]
    lines += ['', '## 输入、审计与偏离', '', f'- 特征哈希：`{FEATURE_HASH}`。',
              f'- 预注册：`{PREREG}`。', f'- 最大特征非有限比例：{max(r["nonfinite_shares"].values()):.6f}。',
              '- 完整逐日IC、CV隔离位置、模型系数、原生权益/成交、周期表与目标权重见 report.json；配对日收益见 daily_returns.csv。']
    lines += [f'- {k}：`{v}`。' for k, v in r['hashes'].items()]
    lines += ['- ' + d for d in r['deviations']]
    if any(p['verdict'] in ('strong', 'screen_pass') for p in r['paired'].values()):
        lines += ['', '建议：仅考虑另行预注册在线配对，等待现有动量S2结果并经批准；不变更正式交易。']
    else:
        lines += ['', '建议：本次未获得值得接入的增量证据；若两臂均 fail，按预注册关闭美股多特征机器学习方向。']
    return '\n'.join(lines) + '\n'


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--broad-root', type=Path, required=True)
    parser.add_argument('--snapshot', type=Path, required=True)
    parser.add_argument('--out', type=Path, required=True)
    group = parser.add_mutually_exclusive_group()
    group.add_argument('--ledger', type=Path)
    group.add_argument('--no-ledger', action='store_true')
    parser.add_argument('--allow-input-drift', action='store_true')
    args = parser.parse_args(argv)
    ledger = None if args.no_ledger else args.ledger or args.out / 'ledger.jsonl'
    out = validate_outputs(args.broad_root, args.snapshot, args.out, ledger)
    guard_features()
    guard_inputs(args.broad_root, args.snapshot, args.allow_input_drift)
    out.mkdir(parents=True, exist_ok=True)
    if (out / 'report.json').exists():
        raise ValueError('completed report already exists; refusing an unregistered repeat')
    attempts = out / 'attempts.jsonl'
    attempt = len(attempts.read_text().splitlines()) + 1 if attempts.exists() else 1
    with attempts.open('a') as stream:
        stream.write(json.dumps(dict(attempt=attempt, started_at=datetime.now(timezone.utc).isoformat())) + '\n')
    started = time.monotonic()
    result = run_study(args.broad_root, args.snapshot, out, args.allow_input_drift,
                       progress=lambda m: print(m, flush=True))
    result.update(attempt=attempt, elapsed_seconds=time.monotonic() - started,
                  command='python -m scripts.factor_mining.us_ml_multifeature_study ' + ' '.join(sys.argv[1:] if argv is None else argv))
    if attempt > 1:
        result['deviations'].append(f'共启动{attempt}次，前次崩溃后仅修实现，参数未变；详见attempts.jsonl。')
    result = core.study.json_safe(result)
    if ledger:
        result['ledger'] = dict(path=str(ledger.resolve()), rows=append_ledger(ledger, result))
    (out / 'report.json').write_text(json.dumps(result, ensure_ascii=False, indent=2, allow_nan=False) + '\n')
    (out / 'report.md').write_text(render_report(result), encoding='utf-8')
    for a, p in result['paired'].items():
        print(f'{a}：{p["verdict"]}；z={p["z"]}；z_noise={p["z_noise"]}；z_K={p["z_K"]}', flush=True)
    print(f'报告：{out / "report.md"}', flush=True)
    return 0


if __name__ == '__main__':
    raise SystemExit(main())
