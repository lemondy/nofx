"""Frozen, offline DVOL sizing S1. Run once after synthetic verification.

python -m scripts.backtest.dvol_sizing_study run --help
"""
from __future__ import annotations

import argparse
from bisect import insort
from datetime import datetime, timezone
import json
import math
from pathlib import Path
import statistics

import numpy as np

from . import evolution_ledger
from .crypto_carry_study import (
    file_metadata, sha256, validate_rows, write_json, timestamp_ms,
)
from .dvol_vol_forecast_study import (
    parkinson_variance, build_samples, model_features, fit_ols, monthly_forecasts,
    load_manifest, validate_dvol_rows, validate_data, positive,
    DAY, TRAIN_START, OOS_START, OOS_END, VALID_START, VALID_END,
)

REPO = Path(__file__).resolve().parents[2]
PREREG = REPO / 'docs/research/2026-11-10-dvol-sizing-s1-prereg.md'
PREREG_SHA256 = '1092269aa7c2a0375856011e7e27813ba01d18b42e60cd6fdb122181844fc1f4'
LEDGER = REPO / 'data/evolution/ledger.jsonl'
SYMBOLS = {coin: coin + 'USDT' for coin in ('BTC', 'ETH', 'SOL', 'BNB', 'XRP', 'DOGE')}
ALT4 = ('SOL', 'BNB', 'XRP', 'DOGE')
MODELS = ('U', 'T', 'B', 'C')
INPUT_SHA256 = {
    'data/dvol/BTC.json': 'dec07cd3535185e56e2328ec49612b3dda72f93b61dbe21d97156a77c16841a3',
    'data/dvol/ETH.json': 'ef316d22eccf958197bf3bb2611b5c3aa5419cc70d37d7cbab93b076b6af9ce4',
    'data/carry/perp_1d/BTCUSDT.json': 'ec73b3b7e0da2ed62e8c54b4fd775d9eb6229cf2e69af2654c4e90bca85af92d',
    'data/carry/perp_1d/ETHUSDT.json': '12c0b553981c14acba33a04dd6873687bb3d6b5b06238b30f95afce32d2ecd2f',
    'data/carry/perp_1d/SOLUSDT.json': '6808342daa4bd07f7bb9e01c5226c3c7d0559e9a043d15e8d88fd88a4fcdaf89',
    'data/carry/perp_1d/BNBUSDT.json': 'e4392ea6a6843b4329136f9906089e611154ebdee812b8a72a25e39509bf2ed5',
    'data/carry/perp_1d/XRPUSDT.json': 'cad59628ae9970055db20bc8a0fa35de5391b9f9570f8c6dd0631e1c9c030db8',
    'data/carry/perp_1d/DOGEUSDT.json': '4134ff56338e0ea4d5c8d6c7945191a46c3dd26e059057b4b6f035f62324fb19',
    'data/llm_trim_offline/export_v1/trades.jsonl': '8abfa110e9ae7e84981150572ea1954178f263f9ba1a0690163d305157382121',
}
NOTES = [
    '全部日期按日线 openTime 的 UTC 日标记，日 t 的缩放于 t+1 日 00:00 可得。',
    'B/C 每月首日 m 重估，训练逐日复用 build_samples，仅纳入 TRAIN_START≤t 且 t+7日<m。'
    'OLS 保留原函数的最小范数解及秩，残差自由度固定为 n−p，p 含截距。n≤p 时跳过该月。',
    'σ_ref 用有序列表增量插入，从 TRAIN_START 起的 build_samples 有效样本 √rv30，'
    '取截至 t（含）的准确中位数。有效性口径含原样本的目标窗口要求。',
    '完整目标样本延伸到输入数据能支持的最后一天。其后的无目标特征只读 t 及之前的日线，'
    '按所在月份同一截止重估 B/C，不把无目标行加入训练或 σ_ref 样本；尾端 σ_ref 因而维持最后值。'
    '无目标预测仅用于交易叠加，主评估仍用原有效样本且 t<OOS_END。',
    '四模型共用评估日期，ALT4 取四币交集。ALT4 的 k 分布是每日四币平均 k 的分布，'
    '换手则为四币各自 |Δk| 的平均，保留币间相反调整。k 分位数采用 NumPy 的线性插值。',
    '换手只计相邻有效评估日期间的 Δk，不计初始建仓或期末平仓；成本为换手×0.0005，仅描述。',
    'ES 取绝对归一化日收益最大的 ceil(0.025n) 项。周块按有效日索引每七项切分，丢弃尾块。'
    'V* 用 U 全部完整周块的 statistics.median，每个载体单独计算。',
    '配对差为前者损失减后者损失，T−B、T−U 为描述比较。普通 t 使用 ddof=1，'
    'sd=0 时 t=0，并在配对统计 notes 标记；不足两块时 t 缺失。奇数块前半取 floor(n/2)。',
    'V*=0 或缺失时周损失与配对统计缺失，不满足 R1；V_b=0 且 V*>0 时损失取极限 0。'
    '空样本的均值、ES、分位数缺失，不满足判定。',
    '方向夏普用样本标准差 ddof=1，少于两日或 sd=0 时缺失。最大回撤从权益 1 起按 x̃ 复利，'
    '做空用 −x̃；若复利权益非正仍按公式记录，回撤可超过 100%。',
    '交易 R 使用实际价差和初始止损距离，不额外扣费。LONG/SHORT 忽略大小写。'
    '取 floor(entry_time/日)−1日的 BTC k，缺少该日 B/C 时剔除，不回退到更早预测。'
    '无效价格/止损优先计数，再检查方向、时间和预测；每笔只计一个剔除原因。',
    '真实数据只允许按冻结规格运行一次。合成单元测试不属于历史尝试，未调参；账本可选，非 invalid 要求 K=3。',
]
LIMITATIONS = [
    'BTC/ETH 评估区间曾用于 H1；山寨币借用 BTC DVOL，ALT4 代表性有限。',
    '日线每日重设与线上执行不同，评估期缺少完整 2022 熊市；平均敞口归一化只用于评估。',
    '日收益只按预注册公式计算风险统计，交易叠加只缩放已实现 R，约六周样本仅作描述。',
]


def verify_prereg(path=None):
    raw = Path(PREREG if path is None else path).read_bytes()
    if sha256(raw) != PREREG_SHA256:
        raise ValueError('Preregistration sha256 mismatch; refusing run/output')
    return raw


def input_paths(dvol_dir, carry_dir, trades):
    paths = {f'data/dvol/{coin}.json': Path(dvol_dir) / f'{coin}.json' for coin in ('BTC', 'ETH')}
    paths.update({f'data/carry/perp_1d/{symbol}.json': Path(carry_dir) / f'perp_1d/{symbol}.json'
                  for symbol in SYMBOLS.values()})
    paths['data/llm_trim_offline/export_v1/trades.jsonl'] = Path(trades)
    return paths


def verify_input_hashes(paths):
    hashes = {name: sha256(path.read_bytes()) for name, path in paths.items()}
    for name, expected in INPUT_SHA256.items():
        if hashes.get(name) != expected:
            raise ValueError(f'{name} frozen sha256 mismatch; refusing run')
    return hashes


def load_inputs(dvol_dir, carry_dir):
    """Reuse manifest schema and row validators, extending the currency mapping."""
    dvol_manifest, dvol_hash = load_manifest(dvol_dir)
    carry_manifest, carry_hash = load_manifest(carry_dir)
    data = dict(dvol={}, perp={})
    for kind, base, manifest, names in (
        ('dvol', dvol_dir, dvol_manifest, {c: f'{c}.json' for c in ('BTC', 'ETH')}),
        ('perp', carry_dir, carry_manifest, {c: f'perp_1d/{s}.json' for c, s in SYMBOLS.items()}),
    ):
        for coin, name in names.items():
            path = Path(base) / name
            rows = json.loads(path.read_bytes())
            if kind == 'dvol':
                validate_dvol_rows(rows)
            else:
                validate_rows(rows, 'perp_1d')
            actual = file_metadata(path, rows)
            recorded = manifest['files'].get(name, {})
            if not isinstance(recorded, dict) or any(recorded.get(k) != v for k, v in actual.items()):
                raise ValueError(f'{name} manifest 哈希、行数或区间不一致')
            data[kind][coin] = rows
    return data, {'data/dvol/manifest.json': dvol_hash, 'data/carry/manifest.json': carry_hash}


def targetless_features(dvol_rows, perp_rows, start=TRAIN_START, end=None):
    """HAR/IV feature rows with no target requirement and no prices after t."""
    iv = {r[0]: r[4] for r in dvol_rows}
    variances = {r[0]: parkinson_variance(r[2], r[3]) for r in perp_rows}
    if end is None:
        end = min(max(iv, default=start - DAY), max(variances, default=start - DAY)) + DAY
    rows = []
    for t in range(start, end, DAY):
        window = [variances.get(t - i * DAY) for i in range(30)]
        if all(positive(v) for v in window) and positive(iv.get(t)):
            rows.append(dict(time_ms=t, rv1=window[0], rv7=statistics.mean(window[:7]),
                             rv30=statistics.mean(window), iv=(iv[t] / 100) ** 2))
    return rows


def calibrated_fit(training, model):
    features = np.asarray([model_features(s, model) for s in training])
    targets = np.asarray([s['log_y'] for s in training])
    fit = fit_ols(features, targets)
    p = features.shape[1]
    if len(training) <= p:
        raise ValueError('残差校准需要 n>p')
    residuals = targets - features @ np.asarray(fit['coefficients'])
    return dict(fit, p=p, residual_df=len(training) - p,
                s2=float(np.dot(residuals, residuals)) / (len(training) - p))


def calibrated_monthly_forecasts(samples, feature_rows=None, start=OOS_START, end=VALID_END):
    """B/C loop identical to monthly_forecasts' training dates and dot products."""
    samples = sorted(samples, key=lambda s: s['time_ms'])
    rows = samples if feature_rows is None else sorted(feature_rows, key=lambda s: s['time_ms'])
    forecasts, fits, skipped = [], [], []
    previous_month, current = None, None
    for row in rows:
        t = row['time_ms']
        if not start <= t < end:
            continue
        month = int(datetime.fromtimestamp(t / 1000, timezone.utc).replace(day=1).timestamp() * 1000)
        if month != previous_month:
            previous_month, current = month, None
            training = [s for s in samples if TRAIN_START <= s['time_ms'] and s['time_ms'] + 7 * DAY < month]
            # C has five parameters including its intercept.
            if len(training) > 5:
                current = dict(month_ms=month, n=len(training), train_start_ms=training[0]['time_ms'],
                               train_last_ms=training[-1]['time_ms'],
                               max_target_day_ms=training[-1]['time_ms'] + 7 * DAY,
                               target_available_at_ms=training[-1]['time_ms'] + 8 * DAY,
                               models={m: calibrated_fit(training, m) for m in ('B', 'C')})
                fits.append(current)
        if current is None:
            skipped.append(dict(time_ms=t, reason='insufficient_residual_degrees_of_freedom'))
            continue
        models = {}
        for model in ('B', 'C'):
            fit = current['models'][model]
            prediction = float(np.dot(model_features(row, model), fit['coefficients']))
            f0 = math.exp(prediction)
            f = math.exp(prediction + fit['s2'] / 2)
            models[model] = dict(log_prediction=prediction, F0=f0, F=f, sigma_hat=math.sqrt(f))
        forecasts.append(dict(time_ms=t, fit_month_ms=month, rv30=row['rv30'],
                              y=row.get('y'), models=models))
    return dict(forecasts=forecasts, fits=fits, skipped=skipped)


def expanding_references(samples, dates):
    samples = sorted((s for s in samples if s['time_ms'] >= TRAIN_START), key=lambda s: s['time_ms'])
    ordered, references, index = [], {}, 0
    for t in sorted(set(dates)):
        while index < len(samples) and samples[index]['time_ms'] <= t:
            insort(ordered, math.sqrt(samples[index]['rv30']))
            index += 1
        references[t] = statistics.median(ordered) if ordered else None
    return references


def sizing_rows(samples, forecasts, perp_rows):
    references = expanding_references(samples, [f['time_ms'] for f in forecasts])
    closes = {r[0]: r[4] for r in perp_rows}
    rows = []
    for forecast in forecasts:
        t = forecast['time_ms']
        ref = references[t]
        if not positive(ref):
            continue
        r = closes[t + DAY] / closes[t] - 1 if positive(closes.get(t)) and positive(closes.get(t + DAY)) else None
        models = dict(U=dict(sigma_hat=None), T=dict(sigma_hat=math.sqrt(forecast['rv30'])),
                      **forecast['models'])
        for model, values in models.items():
            k = 1. if model == 'U' else min(1., ref / values['sigma_hat'])
            # Copy to keep forecast outputs immutable for callers.
            models[model] = dict(values, k=k, x=k * r if r is not None else None)
        rows.append(dict(time_ms=t, sigma_ref=ref, return_next=r, y=forecast['y'],
                         fit_month_ms=forecast['fit_month_ms'], models=models))
    return rows


def evaluation_rows(rows, start=OOS_START, end=OOS_END):
    return [r for r in rows if start <= r['time_ms'] < end and positive(r['y'])
            and r['return_next'] is not None]


def alt4_rows(by_coin):
    indexed = {c: {r['time_ms']: r for r in by_coin[c]} for c in ALT4}
    dates = sorted(set.intersection(*(set(indexed[c]) for c in ALT4)))
    rows = []
    for t in dates:
        models = {}
        for model in MODELS:
            constituents = [indexed[c][t]['models'][model] for c in ALT4]
            models[model] = dict(k=statistics.mean(v['k'] for v in constituents),
                                 x=statistics.mean(v['x'] for v in constituents),
                                 constituent_k=[v['k'] for v in constituents])
        rows.append(dict(time_ms=t, models=models))
    return rows


def expected_shortfall(values):
    if not values:
        return None
    return statistics.mean(sorted(map(abs, values), reverse=True)[:math.ceil(.025 * len(values))])


def paired_test(differences):
    n = len(differences)
    middle = n // 2
    if not n or any(d is None for d in differences):
        return dict(n=n, differences=differences, mean=None, sd=None, t=None,
                    first_half_mean=None, second_half_mean=None, notes=['周损失缺失或无完整周块'])
    mean = statistics.mean(differences)
    sd = statistics.stdev(differences) if n >= 2 else None
    t = None
    if sd is not None:
        t = mean / (sd / math.sqrt(n)) if sd > 0 else 0.
    return dict(n=n, differences=differences, mean=mean, sd=sd, t=t,
                first_half_mean=statistics.mean(differences[:middle]) if middle else None,
                second_half_mean=statistics.mean(differences[middle:]),
                notes=['sd=0，按约定 t=0'] if sd == 0 else [])


def directional_metrics(values):
    sd = statistics.stdev(values) if len(values) >= 2 else None
    sharpe = statistics.mean(values) / sd * math.sqrt(365) if sd is not None and sd > 0 else None
    equity, peak, drawdown = 1., 1., 0.
    for x in values:
        equity *= 1 + x
        peak = max(peak, equity)
        drawdown = max(drawdown, 1 - equity / peak)
    return dict(sharpe=sharpe, max_drawdown=drawdown if values else None, notes=[])


def carrier_statistics(rows):
    """Common dates and common U threshold, including descriptive T comparisons."""
    rows = sorted(rows, key=lambda r: r['time_ms'])
    n_blocks = len(rows) // 7
    models = {}
    for model in MODELS:
        k = [r['models'][model]['k'] for r in rows]
        exposure = statistics.mean(k) if k else None
        normalized = [r['models'][model]['x'] / exposure for r in rows]
        components = [r['models'][model].get('constituent_k', [r['models'][model]['k']]) for r in rows]
        turnover = math.fsum(statistics.mean(abs(b - a) for a, b in zip(left, right))
                             for left, right in zip(components, components[1:]))
        variances = [365 / 7 * math.fsum(x * x for x in normalized[i * 7:(i + 1) * 7])
                     for i in range(n_blocks)]
        quantiles = np.quantile(k, [0, .1, .25, .5, .75, .9, 1]).tolist() if k else [None] * 7
        models[model] = dict(mean_exposure=exposure,
                             k_quantiles=dict(zip(('0', '0.1', '0.25', '0.5', '0.75', '0.9', '1'), quantiles)),
                             scaled_fraction=sum(v < 1 for v in k) / len(k) if k else None,
                             turnover=turnover, cost=turnover * .0005, es=expected_shortfall(normalized),
                             normalized_returns=normalized, weekly_variances=variances,
                             long=directional_metrics(normalized), short=directional_metrics([-x for x in normalized]))
    v_star = statistics.median(models['U']['weekly_variances']) if n_blocks else None
    for values in models.values():
        losses = [max(0., math.log(v / v_star)) ** 2 if v > 0 else 0.
                  for v in values['weekly_variances']] if positive(v_star) else [None] * n_blocks
        values.update(weekly_losses=losses,
                      mean_weekly_loss=statistics.mean(losses) if losses and positive(v_star) else None)
    comparisons = {}
    for left, right in (('B', 'C'), ('T', 'B'), ('T', 'U')):
        differences = [a - b if a is not None and b is not None else None
                       for a, b in zip(models[left]['weekly_losses'], models[right]['weekly_losses'])]
        comparisons[f'{left}_minus_{right}'] = dict(paired_test(differences),
                                                    es_left=models[left]['es'], es_right=models[right]['es'])
    return dict(n=len(rows), dates_ms=[r['time_ms'] for r in rows], models=models,
                weekly=dict(n_blocks=n_blocks, discarded_days=len(rows) % 7, v_star=v_star,
                            comparisons=comparisons))


def qlike_statistics(rows):
    calibrated, uncalibrated = {}, {}
    for model in ('B', 'C'):
        for field, output in (('F', calibrated), ('F0', uncalibrated)):
            # Log form avoids cancellation in Y/F - log(Y/F) - 1.
            errors = [math.log(r['y']) - math.log(r['models'][model][field]) for r in rows]
            output[model] = statistics.mean(math.expm1(e) - e for e in errors) if errors else None
    return dict(n=len(rows), **calibrated, uncalibrated=uncalibrated)


def decision_conditions(stats):
    def lower(a, b):
        return a is not None and b is not None and math.isfinite(a) and math.isfinite(b) and a < b

    carriers = {}
    for carrier in ('BTC', 'ETH', 'ALT4'):
        values = stats.get('carriers', {}).get(carrier, {})
        paired = values.get('weekly', {}).get('comparisons', {}).get('B_minus_C', {})
        models = values.get('models', {})
        t = paired.get('t')
        halves = [paired.get('first_half_mean'), paired.get('second_half_mean')]
        es = {m: models.get(m, {}).get('es') for m in ('U', 'B', 'C')}
        carriers[carrier] = dict(
            R1=dict(t=t, first_half_mean=halves[0], second_half_mean=halves[1],
                    passed=t is not None and math.isfinite(t) and t >= 2 and all(positive(x) for x in halves)),
            R2=dict(es_c=es['C'], es_b=es['B'], es_u=es['U'],
                    passed=lower(es['C'], es['B']) and lower(es['C'], es['U'])))
    qlike = stats.get('qlike', {})
    better = {c: lower(qlike.get(c, {}).get('C'), qlike.get(c, {}).get('B')) for c in SYMBOLS}
    return dict(carriers=carriers, L=dict(main=better['BTC'] and better['ETH'],
                                         alt=sum(better[c] for c in ALT4) >= 3,
                                         alt_better_count=sum(better[c] for c in ALT4), by_symbol=better))


def decide_verdict(stats, valid=True):
    if not valid:
        return 'invalid'
    conditions = decision_conditions(stats)
    good = {c: v['R1']['passed'] and v['R2']['passed'] for c, v in conditions['carriers'].items()}
    if conditions['L']['main'] and conditions['L']['alt'] and all(good.values()):
        return 'pass'
    if conditions['L']['main'] and good['BTC'] and good['ETH']:
        return 'weak'
    return 'fail'


def llm_overlay(trades, btc_rows):
    indexed = {r['time_ms']: r for r in btc_rows}
    retained, excluded = [], dict(invalid_prices_or_stop=0, invalid_side=0, invalid_entry_time=0,
                                  missing_btc_prediction=0)
    for trade in trades:
        prices = [trade.get(k) for k in ('entry_price', 'exit_price', 'initial_stop_loss')]
        if (not all(isinstance(p, (int, float)) and not isinstance(p, bool) and positive(p) for p in prices)
                or prices[0] == prices[2]):
            excluded['invalid_prices_or_stop'] += 1
            continue
        side = trade.get('side')
        if not isinstance(side, str) or side.upper() not in ('LONG', 'SHORT'):
            excluded['invalid_side'] += 1
            continue
        try:
            entry_time = timestamp_ms(trade.get('entry_time'))
            if entry_time <= 0:
                raise ValueError('entry_time 必须为正')
        except (ValueError, TypeError, OverflowError):
            excluded['invalid_entry_time'] += 1
            continue
        day = entry_time // DAY * DAY - DAY
        forecast = indexed.get(day)
        if forecast is None or any(m not in forecast['models'] for m in ('B', 'C')):
            excluded['missing_btc_prediction'] += 1
            continue
        sign = 1 if side.upper() == 'LONG' else -1
        r = sign * (prices[1] - prices[0]) / abs(prices[0] - prices[2])
        k = {m: forecast['models'][m]['k'] for m in ('U', 'B', 'C')}
        retained.append(dict(id=trade.get('id'), entry_time=entry_time, k_date_ms=day, R=r, k=k))
    models = {}
    for model in ('U', 'B', 'C'):
        mean_k = statistics.mean(r['k'][model] for r in retained) if retained else None
        sum_r = math.fsum(r['R'] * r['k'][model] for r in retained)
        models[model] = dict(mean_k=mean_k, sum_R=sum_r, mean_R=sum_r / len(retained) if retained else None,
                             normalized_sum_R=sum_r / mean_k if mean_k else None,
                             normalized_mean_R=sum_r / mean_k / len(retained) if mean_k else None)
    return dict(total=len(trades), retained=len(retained), excluded=excluded,
                excluded_total=sum(excluded.values()), models=models, trades=retained)


def study(dvol_dir, carry_dir, trades):
    verify_prereg()
    try:
        hashes = verify_input_hashes(input_paths(dvol_dir, carry_dir, trades))
        result = dict(prereg_sha256=PREREG_SHA256, input_hashes=hashes, verdict='invalid',
                      validation=dict(valid=False, reasons=[], by_symbol={}), statistics={}, symbols={},
                      trials=dict(historical_runs_this_invocation=0, parameter_variants=1),
                             notes=NOTES, limitations=LIMITATIONS)
        try:
            data, manifest_hashes = load_inputs(dvol_dir, carry_dir)
            hashes.update(manifest_hashes)
            for coin in SYMBOLS:
                dvol = data['dvol']['ETH' if coin == 'ETH' else 'BTC']
                checked = validate_data(dict(dvol=dvol, perp_1d=data['perp'][coin]), VALID_START, VALID_END)
                result['validation']['by_symbol'][coin] = checked
                result['validation']['reasons'].extend(f'{coin} {r}' for r in checked['reasons'])
            trade_rows = [json.loads(line) for line in Path(trades).read_bytes().splitlines() if line.strip()]
            if any(not isinstance(t, dict) for t in trade_rows):
                raise ValueError('交易 JSONL 每行必须为对象')
        except (OSError, ValueError) as exc:
            result['validation']['reasons'].append(str(exc))
        result['validation']['valid'] = not result['validation']['reasons']
        if result['validation']['valid']:
            evaluated = {}
            for coin in SYMBOLS:
                dvol = data['dvol']['ETH' if coin == 'ETH' else 'BTC']
                perp = data['perp'][coin]
                end = min(dvol[-1][0], perp[-1][0]) + DAY
                prepared = build_samples(dvol, perp, TRAIN_START, end)
                samples = prepared['samples']
                last_sample = max((s['time_ms'] for s in samples), default=TRAIN_START - DAY)
                tail = targetless_features(dvol, perp, last_sample + DAY, end)
                forecast = calibrated_monthly_forecasts(samples, samples + tail, OOS_START, end)
                daily = sizing_rows(samples, forecast['forecasts'], perp)
                evaluated[coin] = evaluation_rows(daily, OOS_START, OOS_END)
                result['symbols'][coin] = dict(dvol_source='ETH' if coin == 'ETH' else 'BTC',
                                               samples=prepared, fits=forecast['fits'],
                                               skipped_forecasts=forecast['skipped'], daily=daily)
            alt = alt4_rows(evaluated)
            result['alt4_daily'] = alt
            stats = dict(carriers={c: carrier_statistics(rows) for c, rows in
                                   (('BTC', evaluated['BTC']), ('ETH', evaluated['ETH']), ('ALT4', alt))},
                         qlike={c: qlike_statistics(evaluated[c]) for c in SYMBOLS})
            stats['conditions'] = decision_conditions(stats)
            result['statistics'] = stats
            result['btc_worst_five'] = [dict(time_ms=r['time_ms'], return_date_ms=r['time_ms'] + DAY,
                                             return_next=r['return_next'],
                                             k={m: r['models'][m]['k'] for m in MODELS})
                                        for r in sorted(evaluated['BTC'], key=lambda r: r['return_next'])[:5]]
            result['llm_overlay'] = llm_overlay(trade_rows, result['symbols']['BTC']['daily'])
            result['verdict'] = decide_verdict(stats, True)
            result['trials']['historical_runs_this_invocation'] = 1
        return result
    finally:
        verify_prereg()


def report(result):
    lines = ['# DVOL 仓位缩放 S1', '', f'判定 `{result["verdict"]}`。', '',
             f'预注册 SHA256 `{result["prereg_sha256"]}`。', '', '## 输入与校验', '']
    lines.extend(f'- `{name}`，SHA256 `{value}`。' for name, value in result['input_hashes'].items())
    lines += ['', '```json', json.dumps(result['validation'], ensure_ascii=False, indent=2), '```', '']
    if result['statistics']:
        lines += ['## 主判定与描述统计', '',
                  '| 载体 | 模型 | 平均敞口 | ES | 周损失均值 | 换手 | 成本 |',
                  '|---|---|---|---|---|---|---|']
        for carrier, stats in result['statistics']['carriers'].items():
            for model, values in stats['models'].items():
                fields = [values[k] for k in ('mean_exposure', 'es', 'mean_weekly_loss', 'turnover', 'cost')]
                lines.append('| ' + ' | '.join(map(str, [carrier, model, *fields])) + ' |')
        # Include every descriptive field in the human-readable report as well as JSON.
        descriptive = {}
        daily_fields = ('normalized_returns', 'weekly_variances', 'weekly_losses')
        for carrier, stats in result['statistics']['carriers'].items():
            models = {model: {k: v for k, v in values.items() if k not in daily_fields}
                      for model, values in stats['models'].items()}
            descriptive[carrier] = dict(weekly=stats['weekly'], models=models)
        for title, value in (
            ('R1、R2 与校准 L', result['statistics']['conditions']),
            ('校准及未校准 QLIKE', result['statistics']['qlike']),
            ('分位数、方向、周块及 T 对照', descriptive),
            ('BTC 最大五次日跌幅与前日 k', result['btc_worst_five']),
            ('LLM 交易叠加（仅描述）', {k: v for k, v in result['llm_overlay'].items() if k != 'trades'}),
        ):
            lines += ['', '## ' + title, '', '```json',
                      json.dumps(evolution_ledger.json_safe(value), ensure_ascii=False, indent=2, allow_nan=False), '```']
    lines += ['', '## 补充约定', ''] + ['- ' + n for n in result['notes']]
    lines += ['', '## 局限', ''] + ['- ' + n for n in result['limitations']]
    lines += ['', '逐日 k、σ̂、x、完整周块、月度系数和 s²、交易明细见 results.json。', '']
    return '\n'.join(lines)


def write_ledger(result, path):
    def build(k, previous):
        if result['verdict'] != 'invalid' and k != 3:
            raise ValueError(f'实际账本 K={k} 与预注册 K=3 不一致，拒绝追加')
        conditions = decision_conditions(result['statistics'])
        return dict(track='crypto_llm', stage='S1', change_type='C', idea_id='dvol-sizing-scaler-v1',
                    baseline_version='no-market-vol-scaler', verdict=result['verdict'], preregistered_K=3,
                    prereg_sha256=PREREG_SHA256, input_hashes=result['input_hashes'],
                    carriers=conditions['carriers'], L=conditions['L'],
                    qlike=result['statistics'].get('qlike', {}), trials=result['trials'])
    return evolution_ledger.append_event(path, 'crypto_llm', build)


def validate_output(out, dvol_dir, carry_dir, trades, ledger=None):
    roots = [Path(p).resolve() for p in (dvol_dir, carry_dir, Path(trades).parent)]
    sources = list(input_paths(dvol_dir, carry_dir, trades).values()) + [
        PREREG, Path(dvol_dir) / 'manifest.json', Path(carry_dir) / 'manifest.json']
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
    run.add_argument('--dvol-dir', type=Path, default=REPO / 'data/dvol')
    run.add_argument('--carry-dir', type=Path, default=REPO / 'data/carry')
    run.add_argument('--trades', type=Path, default=REPO / 'data/llm_trim_offline/export_v1/trades.jsonl')
    run.add_argument('--out', type=Path, required=True)
    run.add_argument('--write-ledger', type=Path, nargs='?', const=LEDGER)
    args = parser.parse_args(argv)
    try:
        validate_output(args.out, args.dvol_dir, args.carry_dir, args.trades, args.write_ledger)
        result = study(args.dvol_dir, args.carry_dir, args.trades)
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
