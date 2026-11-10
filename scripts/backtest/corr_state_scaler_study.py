"""Frozen offline correlation-state sizing S1, with synthetic-only verification.

python -m scripts.backtest.corr_state_scaler_study run --help
"""
from __future__ import annotations

import argparse
from bisect import insort
from datetime import datetime, timedelta, timezone
import hashlib
import json
import math
from pathlib import Path
import statistics

import numpy as np
import pandas as pd

from .crypto_carry_study import sha256, write_json, timestamp_ms
from .dvol_sizing_study import (
    expected_shortfall, paired_test, directional_metrics,
    carrier_statistics, qlike_statistics, llm_overlay,
)
from .dvol_vol_forecast_study import fit_ols, positive, DAY
from .evolution_ledger import append_event
from scripts.factor_mining.panel import _read_symbol
from scripts.factor_mining.data_fetch import CRYPTO_EXCLUDED

REPO = Path(__file__).resolve().parents[2]
PREREG = REPO / 'docs/research/2026-11-10-corr-state-scaler-s1-prereg.md'
PREREG_SHA256 = '5c5f274ee5e77c55a02c87eeb8e780c251070e5ac19b3950a9f94fea53e4b516'
LEDGER = REPO / 'data/evolution/ledger.jsonl'
DATA_PREFIX = 'data/factor_mining/crypto_full/'
TRADE_KEY = 'data/llm_trim_offline/export_v1/trades.jsonl'
INPUT_SHA256 = {
    DATA_PREFIX + 'manifest.json': '3a34c083f5d66d823df63b0a24ab58b4afbdb454cb8b8f0b271bdf892abf8a37',
    DATA_PREFIX + 'validation.json': '5c0fdc770abff256f2ccce92e8ba25a5a84bdf596abd99ed9db6ebce6dc1e2a5',
    DATA_PREFIX + 'daily/*.json': 'cdbfc3912e60d9252318fb2d927cc6b59ed49af794052564ff2dd000fa7df244',
    TRADE_KEY: '8abfa110e9ae7e84981150572ea1954178f263f9ba1a0690163d305157382121',
}
DAILY_FILE_COUNT = 685
EXCLUDED = {'BTCUSDT', 'ETHUSDT', 'USDCUSDT', 'PAXGUSDT', 'XAUTUSDT'} | CRYPTO_EXCLUDED
TRAIN_START = timestamp_ms('2021-01-01')
OOS_START = timestamp_ms('2023-01-01')
OOS_END = timestamp_ms('2026-10-26')  # Inclusive.
VALID_START = timestamp_ms('2020-11-01')
VALID_END = timestamp_ms('2026-11-02')  # Inclusive.
CARRIERS = {'TOP10': 10, 'TOP20': 20, 'TOP50': 50}
MODELS = ('U', 'T', 'B', 'C')
QUANTILES = (0, .05, .25, .5, .75, .95, 1)
NOTES = [
    '日线日期及月界均为 UTC，评估起止日包含两端。缺日不填充，窗口按真实日历日取值。',
    '相关性可用成员门槛为 min(15, ⌈0.75N⌉)，按预注册第 11 节运行前勘误：TOP20、TOP50 为 15，TOP10 为 8。',
    '每个剔除阶段按首个原因计一次，相关性不可用成员另累计计数。成员日缺价比例仅用'
    '2021-01-01 至 2026-10-26 的持仓日 t，检查 t+1 收盘，分母是剔除前成员日数。',
    'OLS 复用最小范数解，保留秩，校准自由度固定 n−p，p 包含截距。每个模型要求 n>p，'
    '共同预测要求 B/C 均能拟合。训练仅含有正目标的样本行，且 t+7日严格小于月首。',
    '扩展中位数包含截至 t 的全部有效特征行，不要求目标可用。尾端无目标行继续更新参考值并预测。',
    '分布分位数使用 NumPy 线性插值。按年份分别报告全部特征行与评估日。'
    '五分位并列边界用 searchsorted(side=right) 归入较高组，保留五组，空组统计缺失。',
    'Pearson 相关在不足两点或任一变量零方差时缺失。N_eff 分母为零或负时记缺失并计数。',
    'ES、周块、换手、5bp 成本、方向统计和 QLIKE 完全复用 DVOL S1，空样本不满足判定。'
    '周块按有效日期序列每七行切分，换手不含初始建仓与期末平仓。',
    'LLM 叠加复用原函数，以 TOP20 的开仓前一个 UTC 日预测代替 BTC 预测，'
    '沿用 missing_btc_prediction 剔除字段名，其含义在本研究为缺 TOP20 预测。',
    '真实研究按冻结规格只跑一次，合成测试不算历史尝试。非 invalid 的账本实际 K 必须为 4。',
]
LIMITATIONS = [
    '等权每日重设组合只是 LLM 持仓代理，实际持仓数量、币种及方向均不同。',
    '评估期已用于 DVOL 两次 S1，缺少完整 2022 熊市，日线无法刻画盘中风险。',
    '下架缺价成员直接剔除，可能低估损失。约六周 LLM 交易叠加仅作方向性描述。',
]


def verify_prereg(path=None):
    raw = Path(PREREG if path is None else path).read_bytes()
    if sha256(raw) != PREREG_SHA256:
        raise ValueError('Preregistration sha256 mismatch; refusing run/output')
    return raw


def verify_input_hashes(data_dir, trades):
    daily = sorted((Path(data_dir) / 'daily').glob('*.json'))
    if len(daily) != DAILY_FILE_COUNT:
        raise ValueError(f'daily 文件数必须恰为 685，实际 {len(daily)}，拒绝运行')
    hashes = {DATA_PREFIX + name: sha256((Path(data_dir) / name).read_bytes())
              for name in ('manifest.json', 'validation.json')}
    # Hash concatenated raw bytes in filename order without holding the panel in memory.
    digest = hashlib.sha256()
    for path in daily:
        with path.open('rb') as stream:
            for chunk in iter(lambda: stream.read(1024 * 1024), b''):
                digest.update(chunk)
    hashes[DATA_PREFIX + 'daily/*.json'] = digest.hexdigest()
    hashes[TRADE_KEY] = sha256(Path(trades).read_bytes())
    for name, expected in INPUT_SHA256.items():
        if hashes.get(name) != expected:
            raise ValueError(f'{name} frozen sha256 mismatch; refusing run')
    return hashes


def load_inputs(data_dir):
    """Validate every file through the shared reader before excluding symbols."""
    symbols = {p.stem: _read_symbol(p) for p in sorted((Path(data_dir) / 'daily').glob('*.json'))}
    symbols = {s: frame for s, frame in symbols.items() if s not in EXCLUDED and not frame.empty}
    if not symbols:
        raise ValueError('no usable non-excluded daily data')
    calendar = pd.DatetimeIndex(sorted(set().union(*(f.index for f in symbols.values()))), name='date')
    return {name: pd.DataFrame({s: f[name].reindex(calendar) for s, f in symbols.items()},
                               index=calendar, dtype=float)
            for name in ('close', 'volume', 'vwap')}


def membership_mask(fields, n):
    columns = sorted(set(fields['close'].columns) - EXCLUDED)
    close = fields['close'][columns]
    observed = close.notna()
    eligible = observed & observed.cumsum().ge(60)
    amount = (fields['volume'][columns] * fields['vwap'][columns]).rolling(20, min_periods=20).mean()
    ranks = amount.where(eligible).rank(axis=1, ascending=False, method='first')
    return (eligible & ranks.le(n)).astype(bool)


def basket_returns(close, mask):
    """Return on d uses only S[d-1], retaining missing-close diagnostics."""
    returns, daily = {}, []
    for t, member_row in mask.iterrows():
        time_ms = int(t.value // 1_000_000)
        if time_ms < TRAIN_START:
            continue
        members = list(mask.columns[member_row.to_numpy()])
        current = close.loc[t, members]
        next_date = t + timedelta(days=1)
        following = close.reindex(index=[next_date], columns=members).iloc[0]
        usable = following.notna()
        values = following[usable] / current[usable] - 1
        value = float(values.mean()) if len(values) else None
        returns[time_ms + DAY] = value
        daily.append(dict(member_time_ms=time_ms, time_ms=time_ms + DAY, members=members,
                          member_count=len(members), missing_next_close=int((~usable).sum()),
                          missing_symbols=list(usable.index[~usable]), value=value))
    return dict(returns=returns, daily=daily)


def validate_calendar(calendar):
    required = pd.date_range(pd.Timestamp(VALID_START, unit='ms'), pd.Timestamp(VALID_END, unit='ms'))
    missing = required.difference(calendar)
    return dict(valid=len(missing) == 0, missing_days=len(missing),
                first_missing_days=[d.strftime('%Y-%m-%d') for d in missing[:20]])


def validate_carrier(mask, basket, n):
    required = pd.date_range(pd.Timestamp(TRAIN_START, unit='ms'), pd.Timestamp(OOS_END, unit='ms'))
    counts = mask.sum(axis=1).reindex(required, fill_value=0)
    bad = counts[counts.ne(n)]
    days = [r for r in basket['daily'] if TRAIN_START <= r['member_time_ms'] <= OOS_END]
    total = sum(r['member_count'] for r in days)
    missing = sum(r['missing_next_close'] for r in days)
    ratio = missing / total if total else None
    return dict(valid=len(bad) == 0 and ratio is not None and ratio <= .005,
                expected_members=n, bad_membership_days=len(bad),
                first_bad_membership_days=[dict(date=d.strftime('%Y-%m-%d'), count=int(c))
                                           for d, c in bad.iloc[:20].items()],
                total_member_days=total, missing_next_close=missing, missing_next_close_fraction=ratio)


def correlation_state(close, members, t):
    dates = pd.date_range(t - timedelta(days=30), t)
    prices = close.reindex(index=dates, columns=members)
    complete = prices.notna().all(axis=0)
    log_returns = np.diff(np.log(prices.loc[:, complete].to_numpy()), axis=0)
    usable = np.var(log_returns, axis=0) > 0
    log_returns = log_returns[:, usable]
    n = log_returns.shape[1]
    rho = None
    if n >= 2:
        matrix = np.corrcoef(log_returns, rowvar=False)
        rho = float(matrix[np.triu_indices(n, k=1)].mean())
    d = (1 + (n - 1) * rho) / n if rho is not None else None
    return dict(rho=rho, n_t=n, D=d, unavailable_members=len(members) - n)


def min_correlation_members(n):
    """Prereg section 11 erratum: min(15, ceil(0.75N))."""
    return min(15, math.ceil(.75 * n))


def build_features(close, mask, returns, min_members=15):
    rows, excluded = [], dict(missing_rv_window=0, insufficient_correlation_members=0,
                              nonpositive_rv1=0, nonpositive_rv7=0, nonpositive_rv30=0,
                              nonpositive_D=0, unavailable_correlation_members=0)
    for t, member_row in mask.iterrows():
        time_ms = int(t.value // 1_000_000)
        if time_ms < TRAIN_START:
            continue
        window = [returns.get(time_ms - i * DAY) for i in range(30)]
        if any(r is None or not math.isfinite(r) for r in window):
            excluded['missing_rv_window'] += 1
            continue
        variances = [365 * r * r for r in window]
        rv = dict(rv1=variances[0], rv7=statistics.mean(variances[:7]), rv30=statistics.mean(variances))
        members = list(mask.columns[member_row.to_numpy()])
        state = correlation_state(close, members, t)
        excluded['unavailable_correlation_members'] += state['unavailable_members']
        if state['n_t'] < min_members:
            excluded['insufficient_correlation_members'] += 1
            continue
        invalid = next((key for key, value in dict(**rv, D=state['D']).items() if not positive(value)), None)
        if invalid is not None:
            excluded['nonpositive_' + invalid] += 1
            continue
        rows.append(dict(time_ms=time_ms, **rv, **state))
    return dict(rows=rows, excluded=excluded)


def future_outcomes(returns, t):
    values = [returns.get(t + i * DAY) for i in range(1, 8)]
    if any(r is None or not math.isfinite(r) for r in values):
        return None
    y = 365 / 7 * math.fsum(r * r for r in values)
    equity, peak, drawdown = 1., 1., 0.
    for r in values:
        equity *= 1 + r
        peak = max(peak, equity)
        drawdown = max(drawdown, 1 - equity / peak)
    return dict(y=y, return_7d=equity - 1, max_drawdown_7d=drawdown)


def build_samples(feature_rows, returns):
    rows, excluded = [], dict(missing_target_window=0, nonpositive_y=0)
    for row in feature_rows:
        outcomes = future_outcomes(returns, row['time_ms'])
        if outcomes is None:
            excluded['missing_target_window'] += 1
        elif not positive(outcomes['y']):
            excluded['nonpositive_y'] += 1
        else:
            rows.append(dict(row, **outcomes, log_y=math.log(outcomes['y'])))
    return dict(rows=rows, excluded=excluded)


def model_features(row, model):
    features = [1., math.log(row['rv1']), math.log(row['rv7']), math.log(row['rv30'])]
    if model == 'C':
        features.append(math.log(row['D']))
    elif model != 'B':
        raise ValueError('model must be B or C')
    return features


def calibrated_fit(training, model):
    features = np.asarray([model_features(s, model) for s in training])
    p = 5 if model == 'C' else 4
    if len(training) <= p:
        raise ValueError('残差校准需要 n>p')
    targets = np.asarray([math.log(s['y']) for s in training])
    fit = fit_ols(features, targets)
    residuals = targets - features @ np.asarray(fit['coefficients'])
    return dict(fit, p=p, residual_df=len(training) - p,
                s2=float(np.dot(residuals, residuals)) / (len(training) - p))


def calibrated_monthly_forecasts(samples, feature_rows, start=OOS_START, end=None):
    samples = sorted(samples, key=lambda s: s['time_ms'])
    targets = {s['time_ms']: s['y'] for s in samples}
    forecasts, fits, skipped = [], [], []
    previous_month, current = None, None
    for row in sorted(feature_rows, key=lambda s: s['time_ms']):
        t = row['time_ms']
        if t < start or (end is not None and t > end):
            continue
        month = int(datetime.fromtimestamp(t / 1000, timezone.utc).replace(day=1).timestamp() * 1000)
        if month != previous_month:
            previous_month = month
            training = [s for s in samples if TRAIN_START <= s['time_ms'] and s['time_ms'] + 7 * DAY < month]
            models = {m: calibrated_fit(training, m) for m, p in (('B', 4), ('C', 5)) if len(training) > p}
            current = dict(month_ms=month, n=len(training),
                           train_start_ms=training[0]['time_ms'] if training else None,
                           train_last_ms=training[-1]['time_ms'] if training else None,
                           max_target_day_ms=training[-1]['time_ms'] + 7 * DAY if training else None,
                           target_available_at_ms=training[-1]['time_ms'] + 8 * DAY if training else None,
                           models=models)
            fits.append(current)
        if len(current['models']) != 2:
            skipped.append(dict(time_ms=t, reason='insufficient_residual_degrees_of_freedom'))
            continue
        models = {}
        for model, fit in current['models'].items():
            prediction = float(np.dot(model_features(row, model), fit['coefficients']))
            f0, f = math.exp(prediction), math.exp(prediction + fit['s2'] / 2)
            if not positive(f0) or not positive(f):
                raise ValueError('预测方差必须有限且为正')
            models[model] = dict(log_prediction=prediction, F0=f0, F=f, sigma_hat=math.sqrt(f))
        forecasts.append(dict(row, y=targets.get(t), fit_month_ms=month, models=models))
    return dict(forecasts=forecasts, fits=fits, skipped=skipped)


def expanding_references(feature_rows, dates):
    rows = sorted((r for r in feature_rows if r['time_ms'] >= TRAIN_START), key=lambda r: r['time_ms'])
    ordered, references, index = [], {}, 0
    for t in sorted(set(dates)):
        while index < len(rows) and rows[index]['time_ms'] <= t:
            insort(ordered, math.sqrt(rows[index]['rv30']))
            index += 1
        references[t] = statistics.median(ordered) if ordered else None
    return references


def sizing_rows(feature_rows, forecasts, returns):
    references = expanding_references(feature_rows, [r['time_ms'] for r in forecasts])
    rows = []
    for forecast in forecasts:
        t = forecast['time_ms']
        ref = references[t]
        if not positive(ref):
            continue
        r = returns.get(t + DAY)
        models = dict(U=dict(sigma_hat=None), T=dict(sigma_hat=math.sqrt(forecast['rv30'])),
                      **forecast['models'])
        for model, values in models.items():
            k = 1. if model == 'U' else min(1., ref / values['sigma_hat'])
            models[model] = dict(values, k=k, x=k * r if r is not None else None)
        rows.append(dict(forecast, sigma_ref=ref, return_next=r, models=models))
    return rows


def evaluation_rows(rows, start=OOS_START, end=OOS_END):
    return [r for r in rows if start <= r['time_ms'] <= end and positive(r.get('y'))
            and r.get('return_next') is not None and math.isfinite(r['return_next'])
            and all(m in r['models'] and positive(r['models'][m].get('F')) for m in ('B', 'C'))]


def decision_conditions(stats):
    def finite(x):
        return x is not None and math.isfinite(x)

    def lower(a, b):
        return finite(a) and finite(b) and a < b

    values = {}
    for name in CARRIERS:
        carrier = stats.get('carriers', {}).get(name, {})
        paired = carrier.get('weekly', {}).get('comparisons', {}).get('B_minus_C', {})
        models = carrier.get('models', {})
        values[name] = dict(mean=paired.get('mean'), t=paired.get('t'),
                            first_half_mean=paired.get('first_half_mean'),
                            second_half_mean=paired.get('second_half_mean'),
                            **{f'es_{m.lower()}': models.get(m, {}).get('es') for m in ('U', 'B', 'C')})
    top = values['TOP20']
    r1 = {k: top[k] for k in ('t', 'first_half_mean', 'second_half_mean')}
    r1['passed'] = (finite(top['t']) and top['t'] >= 2.
                    and positive(top['first_half_mean']) and positive(top['second_half_mean']))
    r2 = {k: top[k] for k in ('es_c', 'es_b', 'es_u')}
    r2['passed'] = lower(top['es_c'], top['es_b']) and lower(top['es_c'], top['es_u'])
    robust = {name: dict(values[name], passed=positive(values[name]['mean'])
                        and lower(values[name]['es_c'], values[name]['es_b'])) for name in ('TOP10', 'TOP50')}
    return dict(R1=r1, R2=r2, R3=dict(carriers=robust, passed=all(v['passed'] for v in robust.values())))


def decide_verdict(stats, valid=True):
    if not valid:
        return 'invalid'
    conditions = decision_conditions(stats)
    if conditions['R1']['passed'] and conditions['R2']['passed']:
        return 'pass' if conditions['R3']['passed'] else 'weak'
    return 'fail'


def distribution(values):
    values = [v for v in values if v is not None and math.isfinite(v)]
    quantiles = np.quantile(values, QUANTILES).tolist() if values else [None] * len(QUANTILES)
    return dict(n=len(values), mean=statistics.mean(values) if values else None,
                quantiles=dict(zip(map(str, QUANTILES), quantiles)))


def pearson(left, right):
    if len(left) < 2 or np.var(left) == 0 or np.var(right) == 0:
        return None
    value = float(np.corrcoef(left, right)[0, 1])
    return value if math.isfinite(value) else None


def correlation_descriptives(rows):
    rho = [r['rho'] for r in rows]
    denominator = [1 + 4 * v for v in rho]
    effective = [5 / d for d in denominator if positive(d)]
    return dict(rho=distribution(rho), n_eff=distribution(effective),
                n_eff_undefined=len(rho) - len(effective),
                rho_rv30_pearson=pearson(rho, [r['rv30'] for r in rows]),
                log_D_log_rv30_pearson=pearson([math.log(r['D']) for r in rows],
                                              [math.log(r['rv30']) for r in rows]))


def rho_quintiles(rows, returns):
    boundaries = np.quantile([r['rho'] for r in rows], np.linspace(0, 1, 6)).tolist() if rows else [None] * 6
    groups = [[] for _ in range(5)]
    for row in rows:
        index = int(np.searchsorted(boundaries[1:-1], row['rho'], side='right'))
        outcomes = future_outcomes(returns, row['time_ms'])
        groups[index].append(dict(rho=row['rho'], **outcomes))
    return dict(boundaries=boundaries, groups=[dict(
        quintile=i + 1, days=len(group),
        **{key + '_mean': statistics.mean(r[key] for r in group) if group else None
           for key in ('rho', 'return_7d', 'max_drawdown_7d', 'y')}) for i, group in enumerate(groups)])


def descriptive_statistics(features, evaluated, returns):
    groups = {}
    for name, rows in (('all_features', features), ('evaluation', evaluated)):
        years = sorted({datetime.fromtimestamp(r['time_ms'] / 1000, timezone.utc).year for r in rows})
        groups[name] = dict(correlation_descriptives(rows), by_year={str(year): correlation_descriptives(
            [r for r in rows if datetime.fromtimestamp(r['time_ms'] / 1000, timezone.utc).year == year])
            for year in years})
    return dict(**groups, rho_quintiles=rho_quintiles(evaluated, returns))


def study(data_dir, trades):
    verify_prereg()
    try:
        hashes = verify_input_hashes(data_dir, trades)
        reasons = []
        try:
            fields = load_inputs(data_dir)
        except ValueError as exc:
            reasons.append(str(exc))
            fields = {name: pd.DataFrame(index=pd.DatetimeIndex([], name='date'), dtype=float)
                      for name in ('close', 'volume', 'vwap')}
        calendar = validate_calendar(fields['close'].index)
        validation = dict(valid=calendar['valid'] and not reasons, calendar=calendar, carriers={}, reasons=reasons)
        if not calendar['valid']:
            validation['reasons'].append('2020-11-01 至 2026-11-02 日历缺天')
        result = dict(prereg_sha256=PREREG_SHA256, input_hashes=hashes, validation=validation,
                      carriers={}, statistics=dict(carriers={}, qlike={}), notes=NOTES, limitations=LIMITATIONS,
                      trials=dict(historical_runs_this_invocation=1, parameter_variants=1))
        for name, n in CARRIERS.items():
            mask = membership_mask(fields, n)
            basket = basket_returns(fields['close'], mask)
            checked = validate_carrier(mask, basket, n)
            validation['carriers'][name] = checked
            if not checked['valid']:
                validation['valid'] = False
                validation['reasons'].append(name + ' 成员数或缺 t+1 收盘比例校验失败')
            features = build_features(fields['close'], mask, basket['returns'], min_correlation_members(n))
            samples = build_samples(features['rows'], basket['returns'])
            forecasts = calibrated_monthly_forecasts(samples['rows'], features['rows'], start=OOS_START)
            daily = sizing_rows(features['rows'], forecasts['forecasts'], basket['returns'])
            evaluated = evaluation_rows(daily, OOS_START, OOS_END)
            result['carriers'][name] = dict(
                basket_daily=basket['daily'], features=features['rows'], samples=samples['rows'], daily=daily,
                fits=forecasts['fits'], skipped_forecasts=forecasts['skipped'],
                excluded=dict(features=features['excluded'], samples=samples['excluded'],
                              missing_next_close=sum(r['missing_next_close'] for r in basket['daily'])),
                descriptions=descriptive_statistics(features['rows'], evaluated, basket['returns']))
            result['statistics']['carriers'][name] = carrier_statistics(evaluated)
            result['statistics']['qlike'][name] = qlike_statistics(evaluated)
            if name == 'TOP20':
                result['top20_worst_five'] = [dict(
                    time_ms=r['time_ms'], date=datetime.fromtimestamp(r['time_ms'] / 1000, timezone.utc).strftime('%Y-%m-%d'),
                    return_date_ms=r['time_ms'] + DAY, return_next=r['return_next'], rho=r['rho'],
                    k={m: r['models'][m]['k'] for m in MODELS})
                    for r in sorted(evaluated, key=lambda r: r['return_next'])[:5]]
        trade_rows = [json.loads(line) for line in Path(trades).read_bytes().splitlines() if line.strip()]
        if any(not isinstance(row, dict) for row in trade_rows):
            raise ValueError('交易 JSONL 每行必须为对象')
        result['llm_overlay'] = llm_overlay(trade_rows, result['carriers']['TOP20']['daily'])
        result['statistics']['conditions'] = decision_conditions(result['statistics'])
        result['verdict'] = decide_verdict(result['statistics'], validation['valid'])
        return result
    finally:
        verify_prereg()


def report(result):
    def fmt(value):
        return '缺失' if value is None else format(value, '.6g')

    lines = ['# 币种相关性状态缩放 S1', '', f'判定 `{result["verdict"]}`。', '',
             f'预注册 SHA256 `{result["prereg_sha256"]}`。', '', '## 输入与校验', '',
             '```json', json.dumps(dict(input_hashes=result['input_hashes'], validation=result['validation']),
                                  ensure_ascii=False, indent=2), '```', '', '## R1、R2、R3', '',
             '```json', json.dumps(result['statistics']['conditions'], ensure_ascii=False, indent=2), '```', '',
             '## 主要指标', '', '| 载体 | 天数 | 模型 | 平均敞口 | ES | 周损失均值 | 换手 | 5bp 成本 |',
             '|---|---|---|---|---|---|---|---|']
    for carrier, stats in result['statistics']['carriers'].items():
        for model, values in stats['models'].items():
            fields = [values[k] for k in ('mean_exposure', 'es', 'mean_weekly_loss', 'turnover', 'cost')]
            lines.append('| ' + ' | '.join(map(str, [carrier, stats['n'], model, *fields])) + ' |')
    lines += ['', '## 描述项要点', '',
              '| 载体 | 样本 | 天数 | ρ̄ 均值 | ρ̄ 中位数 | ρ̄ 5% / 95% | N_eff 中位数 | corr(ρ̄,RV30) | corr(ln D,ln RV30) |',
              '|---|---|---|---|---|---|---|---|---|']
    for carrier, data in result['carriers'].items():
        for label, group in data['descriptions'].items():
            if label == 'rho_quintiles':
                continue
            rho = group['rho']
            q = rho['quantiles']
            values = [carrier, label, str(rho['n']), fmt(rho['mean']), fmt(q['0.5']),
                      fmt(q['0.05']) + ' / ' + fmt(q['0.95']), fmt(group['n_eff']['quantiles']['0.5']),
                      fmt(group['rho_rv30_pearson']), fmt(group['log_D_log_rv30_pearson'])]
            lines.append('| ' + ' | '.join(values) + ' |')
    lines += ['', '### 按年 ρ̄ 分布', '', '| 载体 | 样本 | 年份 | 天数 | 均值 | 中位数 | 5% / 95% |',
              '|---|---|---|---|---|---|---|']
    for carrier, data in result['carriers'].items():
        for label in ('all_features', 'evaluation'):
            for year, group in data['descriptions'][label]['by_year'].items():
                rho = group['rho']
                q = rho['quantiles']
                values = [carrier, label, year, str(rho['n']), fmt(rho['mean']), fmt(q['0.5']),
                          fmt(q['0.05']) + ' / ' + fmt(q['0.95'])]
                lines.append('| ' + ' | '.join(values) + ' |')
    lines += ['', '### 校准与 T 对照', '', '| 载体 | B QLIKE 校准 / 未校准 | C QLIKE 校准 / 未校准 | T−B mean / t | T−U mean / t |',
              '|---|---|---|---|---|']
    for carrier, qlike in result['statistics']['qlike'].items():
        stats = result['statistics']['carriers'][carrier]
        comparisons = stats['weekly']['comparisons']
        values = [carrier] + [fmt(qlike[m]) + ' / ' + fmt(qlike['uncalibrated'][m]) for m in ('B', 'C')]
        values += [fmt(comparisons[c]['mean']) + ' / ' + fmt(comparisons[c]['t']) for c in ('T_minus_B', 'T_minus_U')]
        lines.append('| ' + ' | '.join(values) + ' |')
    lines += ['', '### 缩放与方向统计', '', '| 载体 | 模型 | k 中位数 | k<1 比例 | 多头夏普 / 回撤 | 空头夏普 / 回撤 |',
              '|---|---|---|---|---|---|']
    for carrier, stats in result['statistics']['carriers'].items():
        for model, values in stats['models'].items():
            cells = [carrier, model, fmt(values['k_quantiles']['0.5']), fmt(values['scaled_fraction'])]
            cells += [fmt(values[side]['sharpe']) + ' / ' + fmt(values[side]['max_drawdown']) for side in ('long', 'short')]
            lines.append('| ' + ' | '.join(cells) + ' |')
    lines += ['', '### ρ̄ 五分位与之后七日', '', '| 载体 | 组 | 天数 | ρ̄ 均值 | 复利收益均值 | 最大回撤均值 | Y 均值 |',
              '|---|---|---|---|---|---|---|']
    for carrier, data in result['carriers'].items():
        for group in data['descriptions']['rho_quintiles']['groups']:
            values = [carrier, str(group['quintile']), str(group['days'])]
            values += [fmt(group[k + '_mean']) for k in ('rho', 'return_7d', 'max_drawdown_7d', 'y')]
            lines.append('| ' + ' | '.join(values) + ' |')
    lines += ['', '## TOP20 最差五天', '', '| t 日期 | r(t+1) | ρ̄(t) | k U / T / B / C |', '|---|---|---|---|']
    for row in result['top20_worst_five']:
        lines.append('| ' + ' | '.join([row['date'], fmt(row['return_next']), fmt(row['rho']),
                                       ' / '.join(fmt(row['k'][m]) for m in MODELS)]) + ' |')
    lines += ['', '## LLM 交易叠加', '', '```json', json.dumps(
        {k: v for k, v in result['llm_overlay'].items() if k != 'trades'}, ensure_ascii=False, indent=2), '```', '',
        '## 剔除计数', '', '```json', json.dumps({c: d['excluded'] for c, d in result['carriers'].items()},
                                               ensure_ascii=False, indent=2), '```']
    lines += ['', '## 补充约定', ''] + ['- ' + note for note in result['notes']]
    lines += ['', '## 局限', ''] + ['- ' + note for note in result['limitations']]
    lines += ['', '逐日成员、ρ̄、n_t、D、k、σ̂、x、完整描述分布及月度系数和 s² 见 results.json。', '']
    return '\n'.join(lines)


def write_ledger(result, path):
    def build(k, previous):
        if result['verdict'] != 'invalid' and k != 4:
            raise ValueError(f'实际账本 K={k} 与预注册 K=4 不一致，拒绝追加')
        return dict(track='crypto_llm', stage='S1', change_type='C', idea_id='corr-state-scaler-v1',
                    baseline_version='har-basket-scaler', verdict=result['verdict'], preregistered_K=4,
                    prereg_sha256=PREREG_SHA256, input_hashes=result['input_hashes'],
                    **decision_conditions(result['statistics']))
    return append_event(path, 'crypto_llm', build)


def validate_output(out, data_dir, trades, ledger=None):
    roots = [Path(data_dir).resolve(), Path(trades).parent.resolve(), Path(trades).resolve().parent]
    sources = [Path(data_dir) / name for name in ('manifest.json', 'validation.json')]
    sources += list((Path(data_dir) / 'daily').glob('*.json')) + [Path(trades), PREREG]
    destinations = [Path(out) / name for name in ('results.json', 'results.json.tmp', 'report.md')]
    if ledger is not None:
        destinations.append(Path(ledger))
    resolved = [p.resolve() for p in destinations]
    if (len(set(resolved)) != len(resolved)
            or any(p.is_relative_to(root) or root.is_relative_to(p) for p in resolved for root in roots)
            or any(p == source.resolve() or (p.exists() and source.exists() and p.samefile(source))
                   for p in resolved for source in sources)
            or any(a.is_relative_to(b) or b.is_relative_to(a)
                   or (a.exists() and b.exists() and a.samefile(b))
                   for i, a in enumerate(resolved) for b in resolved[i + 1:])):
        raise ValueError('输出与账本不可覆盖输入、落在输入目录内或彼此覆盖')


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    sub = parser.add_subparsers(dest='command', required=True)
    run = sub.add_parser('run')
    run.add_argument('--data-dir', type=Path, default=REPO / 'data/factor_mining/crypto_full')
    run.add_argument('--trades', type=Path, default=REPO / TRADE_KEY)
    run.add_argument('--out', type=Path, required=True)
    run.add_argument('--write-ledger', type=Path, nargs='?', const=LEDGER)
    args = parser.parse_args(argv)
    try:
        validate_output(args.out, args.data_dir, args.trades, args.write_ledger)
        result = study(args.data_dir, args.trades)
        write_json(args.out / 'results.json', result)
        (args.out / 'report.md').write_text(report(result), encoding='utf-8')
        if args.write_ledger is not None:
            result['ledger'] = write_ledger(result, args.write_ledger)
            write_json(args.out / 'results.json', result)
        print(f'VERDICT {result["verdict"]}')
        return 1 if result['verdict'] == 'invalid' else 0
    except (OSError, ValueError, OverflowError, np.linalg.LinAlgError) as exc:
        parser.error(str(exc))


if __name__ == '__main__':
    raise SystemExit(main())
