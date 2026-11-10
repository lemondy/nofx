"""Frozen DVOL variance-forecast S1. Only ``fetch`` uses the network.

python -m scripts.backtest.dvol_vol_forecast_study --help
Forecast diagnostics only. No orders, ledger writes or K accumulation.
"""
from __future__ import annotations

import argparse
from bisect import bisect_right
from collections import Counter
from datetime import datetime, timezone
import json
import math
from pathlib import Path
import statistics
import time
from urllib.parse import urlencode

import numpy as np

from . import market_cache
from .crypto_carry_study import (
    DAY, file_metadata, sha256, timestamp_ms, validate_rows as validate_carry_rows,
    write_json,
)
from .evolution_ledger import json_safe

REPO = Path(__file__).resolve().parents[2]
PREREG = REPO / 'docs/research/2026-11-10-dvol-vol-forecast-s1-prereg.md'
PREREG_SHA256 = 'cc09fb3b49f9cdd78213caa006462cb7c412b011eb4e36438bf18d62ce5b6ffa'
SYMBOLS = {'BTC': 'BTCUSDT', 'ETH': 'ETHUSDT'}
FETCH_START = timestamp_ms('2021-03-24')
VALID_START = timestamp_ms('2021-03-25')
VALID_END = timestamp_ms('2026-11-08')  # Exclusive, includes November 7.
TRAIN_START = timestamp_ms('2021-04-24')
OOS_START = timestamp_ms('2023-01-01')
OOS_END = timestamp_ms('2026-11-01')  # Exclusive, includes October 31.
MODELS = ('B', 'C', 'IV_only', 'HAR_log_spread')
NOTES = [
    '按 openTime 的 UTC 日期对齐，DVOL 当日 close 与永续当日 close 同在次日 00:00 可得。',
    '每月首个日历日 m 重估，只用 t+7<m，即 t≤m−8 天的训练样本。'
    '最后可用目标的价格日期为 m−1，收盘可得时刻至多为 m，不使用 openTime≥m 的价格。'
    '该月首日缺失也维持这个截止，整月共用系数和训练目标均值。',
    '主模型与两个敏感性模型共用完整有效样本，训练日期从 2021-04-24 起扩展。'
    '窗口按日历日构造，不填补缺日。任一 Parkinson 分量、IV 或目标非正即剔除，'
    '有符号或为零的日收益仍按定义参与目标平方和。每个剔除日按首个原因计一次。',
    'R² 的每个预测使用当月训练窗的 ln Y 均值，两半各自使用对应逐日分母。'
    '两半按共同有效预测日数切分，奇数时前半取 floor(n/2)。',
    '不重叠损失样本以首个有效预测日为锚点，每七个日历日取一次，缺日跳过且不移动锚点。'
    '普通 t 使用样本标准差 ddof=1。少于两项时 t 缺失，零标准差时取 0 或带符号无穷。'
    '分母为零时 R² 缺失，缺失统计量不满足 pass 或 weak。',
    'QLIKE 为逐日损失均值，F=exp(对数预测)，不作偏差修正，差值定义为候选减基线。',
    '第 6 节描述项使用共同有效样本外预测日。VRP 年度分布按预测日年份划分。'
    '五分位用线性插值的 20/40/60/80% 分位点，相等值归上组，可出现空组。',
    '未来七日收益为 ln(C[t+7]/C[t])。最大回撤为 C[t..t+7] 相对此前收盘峰值的最大跌幅，'
    '以正比例表示，包含起始收盘。杠杆效应另要求 t−1 的两类 close 都有效。',
    'HAR 加 ln IV−ln RV30 保留原 HAR 的 ln RV30，故与 C 的列空间相同，'
    '敏感性结果仅为重新参数化。两个敏感性模型只作描述。',
    'fetch 按最多 900 个 UTC 日的半开时间窗请求，API 的 end_timestamp 取右端点减 1 毫秒。'
    '只请求已结束的日线。遇到非空 continuation 则二分时间窗，直至单日，'
    '不解释 continuation 的方向或游标语义。单日窗口至多有一根 1D 日线。'
    '响应按时间排序，按时间戳合并且旧行优先，前缀、内部缺口和尾部都补齐。',
    'fetch 所有 HTTP 请求之间至少等待 0.5 秒，含跨币种和细分窗口请求。'
    '已有未结束日线会报错，以保持旧行保留与只收录完整日线的约束。',
    'OLS 使用 numpy.linalg.lstsq，保留最小二乘的最小范数解并记录秩。'
    '无可用训练样本时跳过该月并记录原因。数据校验失败输出 invalid，不计算预测。',
]
LIMITATIONS = [
    '只有 BTC、ETH，约五年半历史，包含多个市场周期，不能外推到其他标的。',
    'DVOL 的 30 日期限与未来 7 日目标不匹配，Deribit 方法的历史修订无法核实。',
    '日线实现方差噪声较大，没有使用日内数据。描述项不参与判定。',
    '这是风险与仓位层的预测检验，通过后仍需另行预注册仓位缩放 S1。',
]


def verify_prereg(path=None):
    raw = Path(PREREG if path is None else path).read_bytes()
    if sha256(raw) != PREREG_SHA256:
        raise ValueError('Preregistration sha256 mismatch; refusing run/output')
    return raw


def validate_dvol_rows(rows):
    if not isinstance(rows, list):
        raise ValueError('DVOL 必须为行数组')
    previous = -1
    for row in rows:
        if not isinstance(row, list) or len(row) != 5:
            raise ValueError('DVOL 行必须有 5 列')
        t = row[0]
        if (not isinstance(t, int) or isinstance(t, bool) or t <= previous or t % DAY
                or not all(isinstance(v, (int, float)) and not isinstance(v, bool)
                           and math.isfinite(v) for v in row[1:])):
            raise ValueError('DVOL 时间重复、乱序、非 UTC 日或数值无效')
        previous = t


def missing_ranges(cached, start, end):
    """Contiguous missing UTC dates in [start, end), including both edges."""
    begin = None
    for t in range(start, end, DAY):
        if t not in cached and begin is None:
            begin = t
        elif t in cached and begin is not None:
            yield begin, t
            begin = None
    if begin is not None:
        yield begin, end


def fetch_data(data_dir, sleep=.5, *, http=None, sleeper=time.sleep, now_ms=None):
    """Incremental fetch with injectable HTTP/clock and no continuation semantics.

    Half-open windows prevent endpoint overlap, even for inclusive API endpoints.
    A continuation signals possible truncation. Subdivide rather than assuming
    that its integer points forwards or backwards. At resolution=1D a one-day
    window cannot be truncated to fewer than one available daily observation.
    Historical rows win ties. Each successful page is persisted before retrying
    smaller windows, so interrupted downloads retain their completed work.
    """
    if not math.isfinite(sleep) or sleep < .5:
        raise ValueError('--sleep 必须至少为 0.5 秒')
    http = market_cache.http_json if http is None else http
    now = int(time.time() * 1000) if now_ms is None else now_ms
    end = now // DAY * DAY
    base = Path(data_dir)
    manifest_path = base / 'manifest.json'
    manifest = json.loads(manifest_path.read_bytes()) if manifest_path.exists() else dict(version=1, files={})
    if not isinstance(manifest, dict) or not isinstance(manifest.get('files'), dict):
        raise ValueError('manifest 必须包含 files 对象')
    requested = False

    for currency in SYMBOLS:
        name = f'{currency}.json'
        path = base / name
        rows = json.loads(path.read_bytes()) if path.exists() else []
        validate_dvol_rows(rows)
        if any(row[0] + DAY > now for row in rows):
            raise ValueError(f'{name} 已有未结束日线')
        cached = {row[0]: row for row in rows}

        def persist():
            merged = [cached[t] for t in sorted(cached)]
            write_json(path, merged)
            manifest['files'][name] = dict(file_metadata(path, merged), fetched_until_ms=now)
            write_json(manifest_path, manifest)

        def download(left, right):
            nonlocal requested
            if all(t in cached for t in range(left, right, DAY)):
                return
            if requested:
                sleeper(sleep)
            requested = True
            query = urlencode(dict(currency=currency, start_timestamp=left,
                                   end_timestamp=right - 1, resolution='1D'))
            response = http('https://www.deribit.com/api/v2/public/get_volatility_index_data?' + query,
                            proxy=market_cache.DEFAULT_PROXY)
            if (not isinstance(response, dict) or not isinstance(response.get('result'), dict)
                    or not isinstance(response['result'].get('data'), list)):
                raise ValueError(f'{name} 下载响应无效')
            page = response['result']['data']
            # Validate structure before sorting potentially malformed rows.
            for row in page:
                validate_dvol_rows([row])
            page = sorted(page, key=lambda row: row[0])
            validate_dvol_rows(page)
            continuation = response['result'].get('continuation')
            if continuation is not None and (not isinstance(continuation, int) or isinstance(continuation, bool)):
                raise ValueError('DVOL continuation 必须为整数或 null')
            for row in page:
                if left <= row[0] < right and row[0] + DAY <= now:
                    cached.setdefault(row[0], row)
            persist()
            if continuation is not None and right - left > DAY:
                middle = left + ((right - left) // DAY // 2) * DAY
                download(left, middle)
                download(middle, right)

        for start, stop in list(missing_ranges(cached, FETCH_START, max(FETCH_START, end))):
            for left in range(start, stop, 900 * DAY):
                download(left, min(left + 900 * DAY, stop))
        persist()
    return manifest


def load_manifest(base):
    raw = (Path(base) / 'manifest.json').read_bytes()
    manifest = json.loads(raw)
    if not isinstance(manifest, dict) or not isinstance(manifest.get('files'), dict):
        raise ValueError('manifest 必须包含 files 对象')
    return manifest, sha256(raw)


def load_symbol(dvol_dir, carry_dir, currency, dvol_manifest, carry_manifest):
    data, hashes = {}, {}
    for kind, base, name, manifest in (
        ('dvol', dvol_dir, f'{currency}.json', dvol_manifest),
        ('perp_1d', carry_dir, f'perp_1d/{SYMBOLS[currency]}.json', carry_manifest),
    ):
        path = Path(base) / name
        rows = json.loads(path.read_bytes())
        if kind == 'dvol':
            validate_dvol_rows(rows)
        else:
            validate_carry_rows(rows, 'perp_1d')
        actual = file_metadata(path, rows)
        recorded = manifest['files'].get(name, {})
        if not isinstance(recorded, dict) or any(recorded.get(k) != v for k, v in actual.items()):
            raise ValueError(f'{kind}/{name} manifest 哈希、行数或区间不一致')
        data[kind] = rows
        namespace = 'dvol' if kind == 'dvol' else 'carry'
        hashes[f'{namespace}/{name}'] = actual['sha256']
    return data, hashes


def validate_data(data, start=VALID_START, end=VALID_END):
    """Section 1 coverage uses all UTC dates, with November 7 included."""
    if end <= start or start % DAY or end % DAY:
        raise ValueError('校验区间必须为非空 UTC 整日区间')
    reasons, details = [], {}
    expected = (end - start) // DAY
    for kind in ('dvol', 'perp_1d'):
        indexed = {r[0]: r for r in data[kind]}
        missing = sum(t not in indexed for t in range(start, end, DAY))
        details[kind] = dict(expected=expected, missing=missing, missing_fraction=missing / expected)
        if missing / expected > .01:
            reasons.append(f'{kind} 日线缺失超过 1%')
    outside = [r[0] for r in data['dvol'] if not math.isfinite(r[4]) or not 5 < r[4] < 400]
    details['dvol']['out_of_range_dates_ms'] = outside
    if outside:
        reasons.append('DVOL close 不在 (5, 400) 内')
    return dict(valid=not reasons, reasons=reasons, details=details)


def positive(value):
    return value is not None and math.isfinite(value) and value > 0


def parkinson_variance(high, low):
    if not positive(high) or not positive(low) or high < low:
        return None
    return (math.log(high) - math.log(low)) ** 2 / (4 * math.log(2)) * 365


def future_outcomes(closes, t):
    prices = [closes.get(t + i * DAY) for i in range(8)]
    if not all(positive(price) for price in prices):
        return None
    returns = [math.log(b) - math.log(a) for a, b in zip(prices, prices[1:])]
    target = (365 / 7) * math.fsum(r * r for r in returns)
    peak, drawdown = prices[0], 0.
    for price in prices[1:]:
        peak = max(peak, price)
        drawdown = max(drawdown, 1 - price / peak)
    return dict(y=target, return_7d=math.log(prices[-1]) - math.log(prices[0]),
                max_drawdown_7d=drawdown)


def build_samples(dvol, perp, start=TRAIN_START, end=OOS_END):
    """Exact calendar-day HAR windows and future returns t+1 through t+7."""
    index = {row[0]: row[4] for row in dvol}
    closes = {row[0]: row[4] for row in perp}
    variances = {row[0]: parkinson_variance(row[2], row[3]) for row in perp}
    samples, excluded = [], []
    for t in range(start, end, DAY):
        window = [variances.get(t - i * DAY) for i in range(30)]
        iv_close = index.get(t)
        outcomes = future_outcomes(closes, t)
        reason = None
        if any(value is None for value in window):
            reason = 'missing_rv_window'
        elif not all(positive(value) for value in window):
            reason = 'nonpositive_rv_component'
        elif iv_close is None:
            reason = 'missing_iv'
        elif not positive(iv_close):
            reason = 'nonpositive_iv'
        elif outcomes is None:
            reason = 'missing_target_window'
        elif not positive(outcomes['y']):
            reason = 'nonpositive_target'
        if reason is not None:
            excluded.append(dict(time_ms=t, reason=reason))
            continue
        samples.append(dict(time_ms=t, rv1=window[0], rv7=statistics.mean(window[:7]),
                            rv30=statistics.mean(window), iv=(iv_close / 100) ** 2,
                            log_y=math.log(outcomes['y']), **outcomes))
    return dict(samples=samples, candidate_days=(end - start) // DAY, valid_days=len(samples),
                excluded_days=excluded, exclusions=dict(Counter(row['reason'] for row in excluded)))


def model_features(row, model):
    har = [1., math.log(row['rv1']), math.log(row['rv7']), math.log(row['rv30'])]
    log_iv = math.log(row['iv'])
    if model == 'B':
        return har
    if model == 'C':
        return [*har, log_iv]
    if model == 'IV_only':
        return [1., log_iv]
    if model == 'HAR_log_spread':
        return [*har, log_iv - har[-1]]
    raise ValueError(f'未知模型 {model}')


def fit_ols(features, targets):
    x, y = np.asarray(features, dtype=float), np.asarray(targets, dtype=float)
    if x.ndim != 2 or y.ndim != 1 or len(y) == 0 or len(x) != len(y):
        raise ValueError('OLS 需要非空且行数一致的二维特征与一维目标')
    if not np.isfinite(x).all() or not np.isfinite(y).all():
        raise ValueError('OLS 输入必须有限')
    coefficients, _, rank, singular = np.linalg.lstsq(x, y, rcond=None)
    return dict(coefficients=coefficients.tolist(), rank=int(rank), n=len(y), singular_values=singular.tolist())


def monthly_forecasts(samples, start=OOS_START, end=OOS_END):
    samples = sorted(samples, key=lambda row: row['time_ms'])
    forecasts, fits, skipped = [], [], []
    previous_month, current = None, None
    for row in samples:
        t = row['time_ms']
        if not start <= t < end:
            continue
        month = int(datetime.fromtimestamp(t / 1000, timezone.utc).replace(day=1).timestamp() * 1000)
        if month != previous_month:
            previous_month = month
            # m is UTC midnight before day m's prices exist. Label t uses closes
            # through date t+7 and becomes available at t+8 midnight. Therefore
            # t+7 < m (t <= m-8), even if the month's first valid day is later.
            training = [s for s in samples if TRAIN_START <= s['time_ms'] and s['time_ms'] + 7 * DAY < month]
            if training:
                targets = [s['log_y'] for s in training]
                current = dict(month_ms=month, n=len(training), train_start_ms=training[0]['time_ms'],
                               train_last_ms=training[-1]['time_ms'],
                               max_target_day_ms=training[-1]['time_ms'] + 7 * DAY,
                               target_available_at_ms=training[-1]['time_ms'] + 8 * DAY,
                               train_mean_log_y=statistics.mean(targets),
                               models={model: fit_ols([model_features(s, model) for s in training], targets)
                                       for model in MODELS})
                fits.append(current)
            else:
                current = None
        if current is None:
            skipped.append(dict(time_ms=t, reason='no_training_samples'))
            continue
        predictions = {model: float(np.dot(model_features(row, model), current['models'][model]['coefficients']))
                       for model in MODELS}
        forecasts.append(dict(time_ms=t, fit_month_ms=month, log_y=row['log_y'], y=row['y'],
                              train_mean_log_y=current['train_mean_log_y'], predictions=predictions))
    return dict(forecasts=forecasts, fits=fits, skipped=skipped)


def forecast_metrics(rows, baseline='B', candidate='C'):
    denominator = math.fsum((r['log_y'] - r['train_mean_log_y']) ** 2 for r in rows)
    values = {}
    for label, model in (('b', baseline), ('c', candidate)):
        errors = np.asarray([r['log_y'] - r['predictions'][model] for r in rows])
        sse = float(np.dot(errors, errors))
        # Y/F = exp(ln Y - forecast), with no variance-bias correction.
        with np.errstate(over='ignore'):
            qlike = float(np.mean(np.expm1(errors) - errors)) if len(rows) else None
        values.update({f'sse_{label}': sse, f'r2_{label}': 1 - sse / denominator if denominator > 0 else None,
                       f'qlike_{label}': qlike})
    return dict(n=len(rows), denominator=denominator, **values,
                delta_r2=values['r2_c'] - values['r2_b'] if denominator > 0 else None,
                delta_qlike=values['qlike_c'] - values['qlike_b'] if rows else None)


def nonoverlapping_loss_test(rows):
    ordered = sorted(rows, key=lambda row: row['time_ms'])
    selected = [r for r in ordered if (r['time_ms'] - ordered[0]['time_ms']) % (7 * DAY) == 0]
    differences = [(r['log_y'] - r['predictions']['B']) ** 2 -
                   (r['log_y'] - r['predictions']['C']) ** 2 for r in selected]
    mean = statistics.mean(differences) if differences else None
    sd = statistics.stdev(differences) if len(differences) >= 2 else None
    t = None
    if sd is not None:
        t = mean / (sd / math.sqrt(len(differences))) if sd > 0 else (0. if mean == 0 else math.copysign(math.inf, mean))
    return dict(n=len(differences), dates_ms=[r['time_ms'] for r in selected],
                differences=differences, mean=mean, sd=sd, t=t)


def split_halves(rows):
    middle = len(rows) // 2
    return rows[:middle], rows[middle:]


def evaluate_forecasts(rows):
    first, second = split_halves(rows)
    return dict(**forecast_metrics(rows), nonoverlap=nonoverlapping_loss_test(rows),
                first_half=forecast_metrics(first), second_half=forecast_metrics(second))


def decide_verdict(stats, valid=True):
    if not valid:
        return 'invalid'

    def at_least(value, threshold):
        return value is not None and value >= threshold

    def above_zero(value):
        return value is not None and value > 0

    both = [stats.get(currency, {}) for currency in SYMBOLS]
    if all(at_least(s.get('delta_r2'), .02) and at_least(s.get('nonoverlap', {}).get('t'), 2.)
           and above_zero(s.get('first_half', {}).get('delta_r2'))
           and above_zero(s.get('second_half', {}).get('delta_r2')) for s in both):
        return 'pass'
    if all(above_zero(s.get('delta_r2')) and above_zero(s.get('nonoverlap', {}).get('t')) for s in both):
        return 'weak'
    return 'fail'


def distribution(values):
    if not values:
        return dict(n=0)
    quantiles = np.quantile(values, [.05, .25, .5, .75, .95]).tolist()
    return dict(n=len(values), mean=statistics.mean(values), median=quantiles[2],
                positive_fraction=sum(v > 0 for v in values) / len(values),
                min=min(values), p05=quantiles[0], p25=quantiles[1], p75=quantiles[3],
                p95=quantiles[4], max=max(values))


def quintile_outcomes(samples, values):
    cuts = np.quantile(values, [.2, .4, .6, .8]).tolist() if values else []
    groups = [[] for _ in range(5)]
    for row, value in zip(samples, values):
        groups[bisect_right(cuts, value)].append(row)
    return dict(cutpoints=cuts, groups=[dict(quintile=i + 1, n=len(group),
        return_7d=distribution([r['return_7d'] for r in group]),
        max_drawdown_7d=distribution([r['max_drawdown_7d'] for r in group])) for i, group in enumerate(groups)])


def descriptive_statistics(samples, forecasts, dvol, perp):
    effective = {r['time_ms'] for r in forecasts}
    samples = [s for s in samples if s['time_ms'] in effective]
    vrp = [s['iv'] - s['y'] for s in samples]
    yearly = {}
    for row, value in zip(samples, vrp):
        year = str(datetime.fromtimestamp(row['time_ms'] / 1000, timezone.utc).year)
        yearly.setdefault(year, []).append(value)
    iv_closes = {r[0]: r[4] for r in dvol}
    closes = {r[0]: r[4] for r in perp}
    pairs = []
    for row in samples:
        t = row['time_ms']
        values = [iv_closes.get(t), iv_closes.get(t - DAY), closes.get(t), closes.get(t - DAY)]
        if all(positive(value) for value in values):
            pairs.append([math.log(values[0]) - math.log(values[1]), math.log(values[2]) - math.log(values[3])])
    correlation = None
    if len(pairs) >= 2:
        changes, returns = np.asarray(pairs).T
        if np.std(changes) > 0 and np.std(returns) > 0:
            correlation = float(np.corrcoef(changes, returns)[0, 1])
    return dict(n=len(samples), vrp=distribution(vrp), vrp_yearly={y: distribution(v) for y, v in yearly.items()},
                quintiles=dict(vrp=quintile_outcomes(samples, vrp),
                               iv_minus_rv30=quintile_outcomes(samples, [s['iv'] - s['rv30'] for s in samples])),
                leverage_effect=dict(n=len(pairs), correlation=correlation),
                sensitivity={model: forecast_metrics(forecasts, candidate=model) for model in MODELS[2:]})


def study(dvol_dir, carry_dir):
    before = verify_prereg()
    result = dict(version=1, engine='numpy.linalg.lstsq', prereg_sha256=PREREG_SHA256,
                  counts_toward_k=False, trials=0, inputs={}, validation={}, invalid_reasons=[],
                  verdict='invalid', statistics={}, symbols={}, descriptions={}, notes=NOTES, limitations=LIMITATIONS)
    loaded, manifests = {}, {}
    for name, base in (('dvol', dvol_dir), ('carry', carry_dir)):
        try:
            manifests[name], digest = load_manifest(base)
            result['inputs'][name + '/manifest_sha256'] = digest
        except (OSError, ValueError, TypeError) as exc:
            result['invalid_reasons'].append(f'{name} manifest 无效 {exc}')
    if not result['invalid_reasons']:
        for currency in SYMBOLS:
            try:
                data, hashes = load_symbol(dvol_dir, carry_dir, currency, manifests['dvol'], manifests['carry'])
                result['inputs'].update(hashes)
                loaded[currency] = data
                validation = validate_data(data)
                result['validation'][currency] = validation
                result['invalid_reasons'].extend(f'{currency} {r}' for r in validation['reasons'])
            except (OSError, ValueError, TypeError, KeyError, IndexError) as exc:
                result['invalid_reasons'].append(f'{currency} 输入无效 {exc}')
    if not result['invalid_reasons']:
        result['trials'] = 1  # One frozen specification, both coins; no ledger event.
        for currency, data in loaded.items():
            prepared = build_samples(data['dvol'], data['perp_1d'])
            prediction = monthly_forecasts(prepared['samples'])
            result['statistics'][currency] = evaluate_forecasts(prediction['forecasts'])
            result['symbols'][currency] = dict(prepared, **prediction)
            result['descriptions'][currency] = descriptive_statistics(
                prepared['samples'], prediction['forecasts'], data['dvol'], data['perp_1d'])
    result['verdict'] = decide_verdict(result['statistics'], valid=not result['invalid_reasons'])
    if verify_prereg() != before:
        raise ValueError('Preregistration changed during study; refusing output')
    return result


def report(result):
    lines = ['# DVOL 波动预测离线 S1', '', f'判定 `{result["verdict"]}`。BTC、ETH 分别估计。', '',
             f'预注册 SHA256 `{PREREG_SHA256}`。主检验尝试次数 {result["trials"]}，不计 K。', '',
             '训练从 2021-04-24 起，样本外为 2023-01-01 至 2026-10-31，UTC 日期。', '',
             'B 为 HAR，C 为 HAR 加 ln IV。ΔR² 为 C−B，ΔQLIKE 为 C−B。']
    if result['invalid_reasons']:
        lines += ['', '## 无效原因', ''] + ['- ' + r for r in result['invalid_reasons']]
    exclusions = {symbol: {key: value for key, value in data.items()
                          if key in ('candidate_days', 'valid_days', 'exclusions', 'skipped')}
                  for symbol, data in result['symbols'].items()}
    for title, value in (('输入校验', result['validation']), ('主统计与两半', result['statistics']),
                         ('剔除与估计缺失', exclusions), ('仅描述', result['descriptions']), ('冻结输入', result['inputs'])):
        lines += ['', '## ' + title, '', '```json', json.dumps(json_safe(value), ensure_ascii=False, indent=2), '```']
    action = {'pass': '增量筛选通过。另写预注册检验波动预测仓位缩放的 S1。',
              'weak': '不接入，保留弱证据结果。', 'fail': '不接入，保留失败结果。',
              'invalid': '数据校验未通过，证据不足。'}[result['verdict']]
    lines += ['', '## 后续', '', action, '', '## 补充约定', ''] + ['- ' + note for note in NOTES]
    lines += ['', '## 局限', ''] + ['- ' + note for note in LIMITATIONS]
    lines += ['', '逐日变量、剔除日、预测、月度系数及训练截止见 results.json。', '']
    return '\n'.join(lines)


def validate_output(out, dvol_dir, carry_dir):
    roots = [Path(dvol_dir).resolve(), Path(carry_dir).resolve()]
    destinations = [(Path(out) / name).resolve() for name in ('results.json', 'results.json.tmp', 'report.md')]
    inputs = [PREREG.resolve(), roots[0] / 'manifest.json', roots[1] / 'manifest.json']
    inputs += [roots[0] / f'{currency}.json' for currency in SYMBOLS]
    inputs += [roots[1] / f'perp_1d/{symbol}.json' for symbol in SYMBOLS.values()]
    if (len(set(destinations)) != len(destinations)
            or any(p == PREREG.resolve() or any(p.is_relative_to(root) or root.is_relative_to(p) for root in roots)
                   or any(source.exists() and p.exists() and p.samefile(source) for source in inputs)
                   for p in destinations)):
        raise ValueError('输出不可覆盖输入或彼此覆盖')


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    sub = parser.add_subparsers(dest='command', required=True)
    fetch = sub.add_parser('fetch')
    fetch.add_argument('--data-dir', type=Path, default=REPO / 'data/dvol')
    fetch.add_argument('--sleep', type=float, default=.5)
    run = sub.add_parser('run')
    run.add_argument('--dvol-dir', type=Path, default=REPO / 'data/dvol')
    run.add_argument('--carry-dir', type=Path, default=REPO / 'data/carry')
    run.add_argument('--out', type=Path, required=True)
    args = parser.parse_args(argv)
    try:
        if args.command == 'fetch':
            fetch_data(args.data_dir, args.sleep)
        else:
            validate_output(args.out, args.dvol_dir, args.carry_dir)
            result = study(args.dvol_dir, args.carry_dir)
            write_json(args.out / 'results.json', result)
            (args.out / 'report.md').write_text(report(result), encoding='utf-8')
            print(f'VERDICT {result["verdict"]}')
            return 1 if result['verdict'] == 'invalid' else 0
    except (OSError, ValueError, np.linalg.LinAlgError) as exc:
        parser.error(str(exc))
    return 0


if __name__ == '__main__':
    raise SystemExit(main())
