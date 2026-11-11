"""Frozen Kronos entry-veto S1. Only fetch uses the network; run is CPU-only.

python -m scripts.backtest.kronos_entry_veto_study {fetch,run} --help
"""
from __future__ import annotations

import argparse
from datetime import datetime, timezone
import hashlib
import json
import math
import os
from pathlib import Path
import random
import re
import subprocess
import sys
import time

from . import evolution_ledger, incremental_filter, market_cache

REPO = Path(__file__).resolve().parents[2]
DATA = Path('/Users/zhangyun/workspace/nofx/data')
PREREG = REPO / 'docs/research/2026-11-11-kronos-entry-veto-s1-prereg.md'
PREREG_SHA256 = 'e53d8cd3a1c2d9f039be68064b1ec0f7152ca0fdce44c90b928a9fc572530031'
TRADES_SHA256 = '8abfa110e9ae7e84981150572ea1954178f263f9ba1a0690163d305157382121'
RESULTS_SHA256 = 'ddce670b658a5abbe40dc04dd51927a0244c55a7ddab8ac75e3e87fd8b14d0b5'
KRONOS_HEAD = '67b630e67f6a18c9e9be918d9b4337c960db1e9a'
WEIGHT_HASHES = {
    'small/model.safetensors': 'b082dfcbd8e8c142a725c8bbb99781802f38fec81210e13479effb32b3c3e020',
    'tok/model.safetensors': '59d85f6af76a2c3b8240ea06cb21db4213b4eeca053f246b23e29cf832fc6bee',
}
LEDGER = DATA / 'evolution/ledger.jsonl'
STEP = market_cache.KLINE_MS['5m']
N_TRADES = 439
OUTPUTS = ('forecasts.jsonl', 'results.json', 'trades.jsonl', 'mask.jsonl', 'report.md')


def file_sha256(path):
    digest = hashlib.sha256()
    with Path(path).open('rb') as source:
        for chunk in iter(lambda: source.read(1024 * 1024), b''):
            digest.update(chunk)
    return digest.hexdigest()


def json_bytes(value):
    return (json.dumps(evolution_ledger.json_safe(value), ensure_ascii=False,
                       sort_keys=True, allow_nan=False, separators=(',', ':')) + '\n').encode()


def write_json(path, value):
    path = Path(path)
    path.parent.mkdir(parents=True, exist_ok=True)
    temporary = path.with_name(path.name + '.tmp')
    temporary.write_bytes(json_bytes(value))
    temporary.replace(path)


def load_export(path):
    trades, _ = incremental_filter.read_jsonl(Path(path))
    ids = [str(t['id']) for t in trades]
    if len(set(ids)) != len(ids) or any(not re.fullmatch(r'\d+', key) for key in ids):
        raise ValueError('交易 id 必须为唯一的非负整数')
    for trade in trades:
        if type(trade['entry_time']) is not int or not re.fullmatch(r'[A-Z0-9]+', trade['symbol']):
            raise ValueError('entry_time 必须为整数毫秒，symbol 必须为大写字母数字')
    return trades


def kline_window(entry_ms, post=False):
    last = entry_ms // STEP * STEP - STEP
    return (last + STEP, last + 61 * STEP) if post else (last - 511 * STEP, last + STEP)


def kline_filename(trade):
    return f"{trade['symbol']}_{trade['entry_ms'] if 'entry_ms' in trade else trade['entry_time']}.json"


def window_rows(rows, start, end):
    by_time = {row[0]: row for row in rows if start <= row[0] < end}
    return [by_time[t] for t in sorted(by_time)]


def fetch_klines(trades_path, klines_dir, sleep=.3, *, fetch=None, sleeper=time.sleep):
    if not math.isfinite(sleep) or sleep < .3:
        raise ValueError('--sleep 必须至少为 0.3 秒')
    fetch = market_cache.fetch_binance_klines if fetch is None else fetch
    trades = load_export(trades_path)
    base = Path(klines_dir)
    manifest_path = base / 'manifest.json'
    manifest = json.loads(manifest_path.read_bytes()) if manifest_path.exists() else dict(version=1, files={})
    manifest['trades_sha256'] = file_sha256(trades_path)
    requested = False
    for trade in trades:
        for post in (False, True):
            name = ('post/' if post else '') + kline_filename(trade)
            path = base / name
            start, end = kline_window(trade['entry_time'], post)
            old_rows = json.loads(path.read_bytes()) if path.exists() else []
            rows = window_rows(old_rows, start, end)
            expected = list(range(start, end, STEP))
            if [r[0] for r in rows] != expected:
                if requested:
                    sleeper(sleep)
                downloaded = fetch(trade['symbol'], '5m', start, end)
                requested = True
                rows = window_rows(downloaded + rows, start, end)  # preserve cached overlaps
            if not path.exists() or old_rows != rows:
                write_json(path, rows)
            manifest['files'][name] = dict(sha256=file_sha256(path), rows=len(rows),
                requested_start_ms=start, requested_end_ms=end, complete=[r[0] for r in rows] == expected)
            write_json(manifest_path, manifest)
    return manifest


def guard_outputs(options):
    protected = [Path(p).resolve() for p in (options.trades, options.results, PREREG, __file__)]
    trees = [Path(p).resolve() for p in (options.klines_dir, options.kronos_repo, options.weights)]
    destinations = [(Path(options.out) / name).resolve() for name in OUTPUTS]
    destinations += [(Path(options.out) / (name + '.tmp')).resolve() for name in OUTPUTS]
    if options.write_ledger is not None:
        destinations.append(Path(options.write_ledger).resolve())
    if (len(set(destinations)) != len(destinations)
            or any(d.is_relative_to(p) or p.is_relative_to(d) for d in destinations for p in protected)
            or any(a != b and a.is_relative_to(b) for a in destinations for b in destinations)
            or any(d.is_relative_to(tree) or tree.is_relative_to(d) for d in destinations for tree in trees)):
        raise ValueError('输出与账本不可覆盖冻结输入或彼此覆盖')


def verify_inputs(options):
    hashes = {}
    for key, path, expected in (('prereg_sha256', PREREG, PREREG_SHA256),
                               ('trades_sha256', options.trades, TRADES_SHA256),
                               ('results_sha256', options.results, RESULTS_SHA256)):
        hashes[key] = file_sha256(path)
        if hashes[key] != expected:
            raise ValueError(f'{key} mismatch，拒绝运行')
    head = subprocess.check_output(['git', '-C', str(options.kronos_repo), 'rev-parse', 'HEAD'], text=True).strip()
    if head != KRONOS_HEAD:
        raise ValueError('Kronos HEAD mismatch，拒绝运行')
    hashes['kronos_head'] = head
    hashes['weights'] = {}
    for name, expected in WEIGHT_HASHES.items():
        actual = file_sha256(Path(options.weights) / name)
        if actual != expected:
            raise ValueError(f'{name} sha256 mismatch，拒绝运行')
        hashes['weights'][name] = actual
    hashes['configs'] = {name: file_sha256(Path(options.weights) / name / 'config.json') for name in ('small', 'tok')}
    hashes['study_sha256'] = file_sha256(__file__)
    return hashes


def load_trades(trades_path, results_path):
    exported = load_export(trades_path)
    results = json.loads(Path(results_path).read_bytes())['trades']
    by_id = {str(row['id']): row for row in results}
    if (len(exported) != N_TRADES or len(by_id) != len(results)
            or {str(t['id']) for t in exported} != by_id.keys()):
        raise ValueError(f'冻结交易必须为 {N_TRADES} 笔，且与 R 的 id 一一对应')
    trades = []
    for row in exported:
        key = str(row['id'])
        sign = {'LONG': 1, 'SHORT': -1}.get(row['side'])
        r, baseline = (float(by_id[key][arm]['r']) for arm in ('no_trim', 'baseline'))
        if sign is None or not all(math.isfinite(v) for v in (r, baseline)):
            raise ValueError('交易方向或 R 无效')
        entered = datetime.fromtimestamp(row['entry_time'] / 1000, timezone.utc)
        trades.append(dict(trade_id=key, symbol=row['symbol'], side=row['side'], side_sign=sign,
            entry_ms=row['entry_time'], entry_time=entered.isoformat(timespec='milliseconds').replace('+00:00', 'Z'),
            r=r, r_baseline=baseline))
    return sorted(trades, key=lambda t: (t['entry_time'], t['trade_id']))


def validate_context(rows, entry_ms):
    if not isinstance(rows, list) or len(rows) < 480:
        raise ValueError('上下文少于 480 根')
    rows = rows[-512:]
    previous = None
    for row in rows:
        if not isinstance(row, list) or len(row) != 7:
            raise ValueError('K 线必须有七个字段')
        opened = row[0]
        if type(opened) is not int or opened % STEP or (previous is not None and opened - previous != STEP):
            raise ValueError('K 线有缺口或时间未对齐')
        prices = [float(v) for v in row[1:5]]
        volumes = [float(v) for v in row[5:7]]
        if not all(math.isfinite(v) and v > 0 for v in prices):
            raise ValueError('OHLC 必须为有限正数')
        if not all(math.isfinite(v) and v >= 0 for v in volumes):
            raise ValueError('成交量或成交额无效')
        previous = opened
    if previous != kline_window(entry_ms)[1] - STEP:
        raise ValueError('最后一根 openTime 不匹配')
    return rows


def load_models(repo, weights):
    sys.path.insert(0, str(Path(repo).resolve()))
    from model import Kronos, KronosTokenizer
    import model.kronos as kronos_mod
    tok = KronosTokenizer.from_pretrained(str(Path(weights) / 'tok')).to('cpu').eval()
    model = Kronos.from_pretrained(str(Path(weights) / 'small')).to('cpu').eval()
    return tok, model, kronos_mod


class NumpyCapture:
    """Keep the paths immediately before Kronos's final sample-axis mean."""
    def __init__(self, numpy):
        self.numpy = numpy
        self.paths = None

    def __getattr__(self, name):
        return getattr(self.numpy, name)

    def mean(self, values, *args, **kwargs):
        self.paths = self.numpy.array(values, copy=True)
        return self.numpy.mean(values, *args, **kwargs)


def capture_inference(kronos_mod, *args, **kwargs):
    import numpy as np
    original = kronos_mod.np
    capture = NumpyCapture(np)
    try:
        kronos_mod.np = capture
        averaged = kronos_mod.auto_regressive_inference(*args, **kwargs)
    finally:
        kronos_mod.np = original
    assert capture.paths is not None, '没有捕获逐路径预测'
    assert np.array_equal(averaged, capture.paths.mean(axis=1)), '逐路径均值与 Kronos 返回值不一致'
    paths = capture.paths[0][:, -60:, :]
    assert paths.shape == (16, 60, 6), f'预测 shape 错误 {paths.shape}'
    return paths


def forecast_paths(bundle, context, opens, seed):
    import numpy as np
    import pandas as pd
    import torch
    tok, model, kronos_mod = bundle
    x = np.asarray(context, dtype=np.float32)
    mu, sd = x.mean(0), x.std(0)
    xn = np.clip((x - mu) / (sd + 1e-5), -5, 5)
    xs = kronos_mod.calc_time_stamps(pd.Series(pd.to_datetime(opens, unit='ms'))).values.astype(np.float32)
    future = [opens[-1] + k * STEP for k in range(1, 61)]
    ys = kronos_mod.calc_time_stamps(pd.Series(pd.to_datetime(future, unit='ms'))).values.astype(np.float32)
    torch.manual_seed(seed)
    with torch.no_grad():
        paths = capture_inference(kronos_mod, tok, model, torch.from_numpy(xn[None]),
            torch.from_numpy(xs[None]), torch.from_numpy(ys[None]), max_context=512, pred_len=60,
            clip=5, T=1.0, top_k=0, top_p=.9, sample_count=16, verbose=False)
    return paths * (sd + 1e-5) + mu


def gbm(n, seed, sigma=.004):
    import numpy as np
    import pandas as pd
    rng = np.random.default_rng(seed)
    r = rng.normal(0, sigma, n)
    c = np.exp(np.cumsum(r))
    o = np.r_[c[0], c[:-1]]
    h = np.maximum(o, c) * np.exp(np.abs(rng.normal(0, sigma / 2, n)))
    l = np.minimum(o, c) * np.exp(-np.abs(rng.normal(0, sigma / 2, n)))
    v = rng.lognormal(10, .5, n)
    return pd.DataFrame(dict(open=o, high=h, low=l, close=c, volume=v, amount=v * c))


def synthetic_gate(forecast):
    import pandas as pd
    stamps = pd.date_range('2030-01-01', periods=572, freq='5min')
    opens = (stamps.asi8 // 1_000_000).tolist()[:512]
    probabilities = []
    for s in range(50):
        context = gbm(572, 5000 + s).values[:512]
        paths = forecast(context, opens, 11 + s)
        probabilities.append(float((paths[:, -1, 3] > context[-1, 3]).mean()))
        print(f'G3 {s + 1}/50 p_up={probabilities[-1]:.4f}', flush=True)
    average = math.fsum(probabilities) / 50
    return dict(valid=.35 <= average <= .65, mean_p_up=average, p_up=probabilities, n=50)


def forecast_summary(paths, context, side_sign):
    import numpy as np
    closes = np.asarray(context, dtype=float)[:, 3]
    terminal = np.asarray(paths)[:, -1, 3]
    p0 = float(closes[-1])
    adverse = terminal < p0 if side_sign == 1 else terminal > p0
    p_adverse = float(adverse.mean())
    with np.errstate(divide='ignore', invalid='ignore'):
        m = float(np.log(terminal / p0).mean())
    sd = float(closes.std())
    return dict(p_adverse=p_adverse, drop=p_adverse >= .75, p0=p0,
                mean_terminal=float(terminal.mean()), m=m if math.isfinite(m) else None,
                z_last=float((p0 - closes.mean()) / sd) if sd else None,
                past5h=float(np.log(p0 / closes[-61])), n_context=len(closes))


def load_checkpoints(path):
    if not path.exists():
        return {}
    raw = path.read_bytes()
    # Only a torn final append is discarded. Complete lines remain immutable.
    complete = raw[:raw.rfind(b'\n') + 1]
    if raw != complete:
        with path.open('r+b') as stream:
            stream.truncate(len(complete))
    rows = [json.loads(line) for line in complete.splitlines() if line.strip()]
    return {(row['trade_id'], row['inputs_sha256']): row for row in rows}


def validate_data(trades, klines_dir):
    prepared = {}
    for trade in trades:
        pre = Path(klines_dir) / kline_filename(trade)
        pre_sha, reason, context, opens = None, None, None, None
        try:
            pre_sha = file_sha256(pre)
            rows = validate_context(json.loads(pre.read_bytes()), trade['entry_ms'])
            context, opens = [r[1:7] for r in rows], [r[0] for r in rows]
        except (OSError, ValueError, TypeError, IndexError, OverflowError) as exc:
            reason = str(exc)
        prepared[trade['trade_id']] = (pre_sha, reason, context, opens)
    return prepared


def collect_forecasts(trades, prepared, out, inputs, forecast):
    path = Path(out) / 'forecasts.jsonl'
    path.parent.mkdir(parents=True, exist_ok=True)
    cached = load_checkpoints(path)
    records, contexts = [], {}
    for index, trade in enumerate(trades, 1):
        pre_sha, reason, context, opens = prepared[trade['trade_id']]
        identity = dict(inputs=inputs, pre_sha256=pre_sha, trade=trade['trade_id'])
        key = hashlib.sha256(json_bytes(identity)).hexdigest()
        if reason is None:
            contexts[trade['trade_id']] = (context, opens)
        record = cached.get((trade['trade_id'], key))
        if record is None:
            record = dict(trade_id=trade['trade_id'], side_sign=trade['side_sign'], inputs_sha256=key, pre_sha256=pre_sha,
                          no_forecast=reason is not None, reason=reason, drop=False)
            if reason is None:
                started = time.monotonic()
                paths = forecast(context, opens, 20261111 + int(trade['trade_id']))
                record.update(forecast_summary(paths, context, trade['side_sign']),
                              seconds=time.monotonic() - started, paths=paths.tolist())
            with path.open('ab') as stream:
                stream.write(json_bytes(record))
                stream.flush()
                os.fsync(stream.fileno())
        records.append(evolution_ledger.json_safe(record))
        print(f"交易 {index}/{len(trades)} id={trade['trade_id']} drop={record['drop']} "
              f"p_adverse={record.get('p_adverse')} reason={record['reason']}", flush=True)
    return records, contexts


def reproducibility_gate(records, contexts, forecast):
    import numpy as np
    by_id = {r['trade_id']: r for r in records if not r['no_forecast']}
    chosen = random.Random(7).sample(sorted(by_id), min(10, len(by_id)))
    mismatches = []
    for key in chosen:
        context, opens = contexts[key]
        paths = forecast(context, opens, 20261111 + int(key))
        old = by_id[key]
        probability = forecast_summary(paths, context, old['side_sign'])['p_adverse']
        identical = np.array_equal(paths, np.asarray(old['paths'])) and probability == old['p_adverse']
        if not identical:
            mismatches.append(key)
    return dict(valid=len(chosen) == 10 and not mismatches, trade_ids=chosen, mismatches=mismatches)


def s0_gates(records, g1, g3):
    missing = sum(r['no_forecast'] for r in records)
    dropped = sum(r['drop'] for r in records)
    share = missing / len(records) if records else 1.
    return dict(G1=g1, G2=dict(valid=share <= .05, no_forecast_n=missing, share=share),
                G3=g3, G4=dict(valid=dropped >= 20, dropped_n=dropped))


def proxy_mask(trades, records):
    by_id = {r['trade_id']: r for r in records}
    def score(trade):
        row = by_id[trade['trade_id']]
        z = row.get('z_last')
        # A flat context has an undefined z score, sorted with unavailable scores.
        return trade['side_sign'] * z if not row['no_forecast'] and z is not None else -math.inf
    ranked = sorted(trades, key=lambda t: (-score(t), int(t['trade_id'])))
    selected = {t['trade_id'] for t in ranked[:sum(r['drop'] for r in records)]}
    return {t['trade_id']: t['trade_id'] in selected for t in trades}


def spearman(pairs):
    pairs = [(x, y) for x, y in pairs if x is not None and y is not None and math.isfinite(x) and math.isfinite(y)]
    def ranks(values):
        ordered = sorted(range(len(values)), key=values.__getitem__)
        result = [0.] * len(values)
        start = 0
        while start < len(values):
            end = start + 1
            while end < len(values) and values[ordered[end]] == values[ordered[start]]:
                end += 1
            for index in ordered[start:end]:
                result[index] = (start + end - 1) / 2
            start = end
        return result
    if len(pairs) < 2:
        return dict(n=len(pairs), rho=None)
    x, y = (ranks(list(values)) for values in zip(*pairs))
    mid = (len(pairs) - 1) / 2
    numerator = math.fsum((a - mid) * (b - mid) for a, b in zip(x, y))
    denominator = math.sqrt(math.fsum((a - mid) ** 2 for a in x) * math.fsum((b - mid) ** 2 for b in y))
    return dict(n=len(pairs), rho=numerator / denominator if denominator else None)


def descriptives(trades, records, klines_dir):
    joined = [dict(t, **{k: v for k, v in r.items() if k != 'trade_id'}) for t, r in zip(trades, records)]
    valid = [t for t in joined if not t['no_forecast']]
    values, drops = [t['r'] for t in joined], [t['drop'] for t in joined]
    hits, post_hashes = [], {}
    for trade in valid:
        path = Path(klines_dir) / 'post' / kline_filename(trade)
        try:
            post_hashes[trade['trade_id']] = file_sha256(path)
            rows = json.loads(path.read_bytes())
            start, end = kline_window(trade['entry_ms'], True)
            if [r[0] for r in rows] == list(range(start, end, STEP)):
                close = float(rows[-1][4])
                if math.isfinite(close) and close > 0:
                    actual = math.log(close / trade['p0'])
                    predicted = trade['mean_terminal'] - trade['p0']
                    hits.append((predicted > 0) - (predicted < 0) == (actual > 0) - (actual < 0))
        except (OSError, ValueError, TypeError, IndexError):
            continue
    return dict(threshold_deltas={str(n) + '/16': incremental_filter.mean_delta(values,
                    [not t['no_forecast'] and t['p_adverse'] >= n / 16 for t in joined]) for n in (11, 13)},
        side_deltas={side: incremental_filter.mean_delta([t['r'] for t in joined if t['side'] == side],
                    [t['drop'] for t in joined if t['side'] == side]) for side in ('LONG', 'SHORT')},
        baseline_delta=incremental_filter.mean_delta([t['r_baseline'] for t in joined], drops),
        directional_hit_rate=dict(n=len(hits), rate=sum(hits) / len(hits) if hits else None),
        side_m_vs_r=spearman([(t['side_sign'] * t['m'], t['r']) for t in valid if t['m'] is not None]),
        side_past5h_vs_p_adverse=spearman([(t['side_sign'] * t['past5h'], t['p_adverse']) for t in valid]),
        side_z_vs_p_adverse=spearman([(t['side_sign'] * t['z_last'], t['p_adverse']) for t in valid if t['z_last'] is not None]),
        mean_seconds=math.fsum(t['seconds'] for t in valid) / len(valid) if valid else None), post_hashes


def study(options):
    if options.device != 'cpu':
        raise ValueError('预注册只允许 --device cpu')
    guard_outputs(options)
    inputs = verify_inputs(options)
    trades = load_trades(options.trades, options.results)
    prepared = validate_data(trades, options.klines_dir)
    bundle = load_models(options.kronos_repo, options.weights)
    def forecast(context, opens, seed):
        return forecast_paths(bundle, context, opens, seed)
    g3 = synthetic_gate(forecast)
    records, contexts = collect_forecasts(trades, prepared, options.out, inputs, forecast)
    gates = s0_gates(records, reproducibility_gate(records, contexts, forecast), g3)
    # All gates are fixed before the first performance calculation.
    mask = {r['trade_id']: r['drop'] for r in records}
    stats = incremental_filter.filter_statistics(trades, mask, seed=20261103, placebo_n=2000)
    verdict = incremental_filter.decide_verdict(stats, 5) if all(g['valid'] for g in gates.values()) else 'invalid'
    proxy = proxy_mask(trades, records)
    proxy_delta = incremental_filter.mean_delta([t['r'] for t in trades], [proxy[t['trade_id']] for t in trades])
    descriptions, post_hashes = descriptives(trades, records, options.klines_dir)
    eligible = (verdict in ('screen_pass', 'strong') and proxy_delta is not None and stats['delta'] > proxy_delta)
    inputs = dict(inputs, pre_files={r['trade_id']: r['pre_sha256'] for r in records}, post_files=post_hashes)
    manifest = Path(options.klines_dir) / 'manifest.json'
    inputs['manifest_sha256'] = file_sha256(manifest) if manifest.exists() else None
    return dict(verdict=verdict, prereg_sha256=PREREG_SHA256, preregistered_K=5, z_K=incremental_filter.z_threshold(5),
        input_hashes=inputs, gates=gates, stats=stats, proxy=dict(delta=proxy_delta, mask=proxy,
            stats=incremental_filter.filter_statistics(trades, proxy, seed=20261103, placebo_n=2000)),
        descriptives=descriptions, s2_eligible=eligible, trials=1,
        trades=[dict(t, **{k: v for k, v in r.items() if k not in ('paths', 'trade_id')}) for t, r in zip(trades, records)])


def report(result):
    stats, gates = result['stats'], result['gates']
    return '\n'.join(['# Kronos 开仓否决 S1', '', f"判定 `{result['verdict']}`，预注册 K=5。",
        f"剔除 {stats['dropped_n']} / {stats['n']}，无预测 {gates['G2']['no_forecast_n']}。", '',
        f"ΔavgR={stats['delta']}，z={stats['z']}，安慰剂分位={stats['placebo_pct']}。",
        f"前半 Δ={stats['dev_delta']}，后半 Δ={stats['holdout_delta']}。",
        f"同笔数代理 Δ={result['proxy']['delta']}，s2_eligible={result['s2_eligible']}。", '',
        *[f"- {name} {'通过' if gate['valid'] else '失败'}，{json_bytes(gate).decode().strip()}" for name, gate in gates.items()],
        '', '样本曾用于提前减仓、DVOL 和相关性状态研究，做多子样本与因子否决研究重叠。',
        'R 复用冻结的 Backtrader no_trim 结果。结论用于 S2 候选筛选。逐笔数据及描述项见 results.json。', ''])


def write_outputs(result, out):
    out = Path(out)
    out.mkdir(parents=True, exist_ok=True)
    hashes = {}
    for name, fields in (('trades.jsonl', ('trade_id', 'entry_time', 'r')), ('mask.jsonl', ('trade_id', 'drop'))):
        path = out / name
        temporary = path.with_name(path.name + '.tmp')
        temporary.write_bytes(b''.join(json_bytes({key: t[key] for key in fields}) for t in result['trades']))
        temporary.replace(path)
        hashes[name] = file_sha256(path)
    result['output_hashes'] = hashes
    write_json(out / 'results.json', result)
    temporary = out / 'report.md.tmp'
    temporary.write_text(report(result), encoding='utf-8')
    temporary.replace(out / 'report.md')


def write_ledger(result, path):
    def build(k, previous):
        if result['verdict'] != 'invalid' and k != 5:
            raise ValueError(f'实际账本 K={k} 与预注册 K=5 不一致，拒绝追加')
        return dict(result['stats'], track='crypto_llm', stage='S1', change_type='B',
            idea_id='kronos-entry-veto-v1', baseline_version='no-entry-veto', verdict=result['verdict'],
            K=None if result['verdict'] == 'invalid' else k, preregistered_K=5,
            z_K=incremental_filter.z_threshold(5), prereg_sha256=PREREG_SHA256, input_hashes=result['input_hashes'],
            proxy_delta=result['proxy']['delta'], s2_eligible=result['s2_eligible'], trials=1,
            counts_toward_k=result['verdict'] != 'invalid')
    return evolution_ledger.append_event(path, 'crypto_llm', build)


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    commands = parser.add_subparsers(dest='command', required=True)
    fetch = commands.add_parser('fetch', help='下载并保留入场前及描述性入场后 K 线')
    fetch.add_argument('--trades', type=Path, required=True)
    fetch.add_argument('--klines-dir', type=Path, default=DATA / 'kronos_veto/kl5m_pre')
    fetch.add_argument('--sleep', type=float, default=.3)
    run = commands.add_parser('run', help='冻结 CPU 离线筛选，支持逐笔续跑')
    for name in ('trades', 'results', 'klines-dir', 'kronos-repo', 'weights', 'out'):
        run.add_argument('--' + name, type=Path, required=True)
    run.add_argument('--write-ledger', type=Path, nargs='?', const=LEDGER)
    run.add_argument('--device', default='cpu')
    args = parser.parse_args(argv)
    try:
        if args.command == 'fetch':
            fetch_klines(args.trades, args.klines_dir, args.sleep)
            return 0
        result = study(args)
        write_outputs(result, args.out)
        if args.write_ledger is not None:
            write_ledger(result, args.write_ledger)
        print(f"VERDICT {result['verdict']}")
        return 1 if result['verdict'] == 'invalid' else 0
    except (OSError, ValueError, subprocess.CalledProcessError) as exc:
        parser.error(str(exc))


if __name__ == '__main__':
    raise SystemExit(main())
