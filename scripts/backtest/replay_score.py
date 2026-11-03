"""离线 LLM 重放评分：复用 Backtrader，按决策点配对并按 UTC 日聚类。"""
import hashlib
import json
import math
import sqlite3
from collections import Counter, defaultdict
from contextlib import closing
from datetime import datetime, timezone
from pathlib import Path
from statistics import NormalDist

from . import backtrader_adapter, evolution_ledger, exitsim, fetch, signals
from .klines import floor_min, load_bars
from .params import FALLBACK


def weighted_cluster(points):
    """Hájek weighted mean; CR1 cluster sandwich G/(G-1), no iid point SE.

    Each point has diff, weight, day. The sampled UTC days are the clusters;
    weights are population-stratum-size / sampled-stratum-size.
    """
    n = len(points)
    if not n:
        return dict(n=0, days=0, mean=None, se=None, ci95=None, z=None)
    if any(not math.isfinite(p['weight']) or p['weight'] <= 0
           or not math.isfinite(p['diff']) for p in points):
        raise ValueError('配对差与权重必须有限，权重必须大于 0')
    total = sum(p['weight'] for p in points)
    mean = sum(p['weight'] * p['diff'] for p in points) / total
    scores = defaultdict(float)
    for p in points:
        scores[p['day']] += p['weight'] * (p['diff'] - mean)
    g = len(scores)
    if g < 2:
        return dict(n=n, days=g, mean=mean, se=None, ci95=None, z=None)
    se = math.sqrt(g / (g - 1) * sum(v * v for v in scores.values())) / total
    z = mean / se if se else (math.copysign(math.inf, mean) if mean else 0.0)
    critical = NormalDist().inv_cdf(.975)
    return dict(n=n, days=g, mean=mean, se=se,
                ci95=[mean - critical * se, mean + critical * se], z=z)


def split_halves(pairs):
    days = sorted({p['day'] for p in pairs})
    dev_days = set(days[:(len(days) + 1) // 2])
    return dict(dev=weighted_cluster([p for p in pairs if p['day'] in dev_days]),
                holdout=weighted_cluster([p for p in pairs if p['day'] not in dev_days]))


def screen_verdict(stats, halves, noise, parse_failure_rate, patch_hit_rate, k,
                   open_rate_change, open_rate_limit=.5):
    if k < 1 or not math.isfinite(open_rate_limit) or open_rate_limit < 0:
        raise ValueError('K 至少为 1，开仓率阈值必须是非负有限数')
    z_k = NormalDist().inv_cdf(1 - .05 / (2 * k))
    reasons, warnings = [], []
    if noise is not None and noise['z'] is not None and abs(noise['z']) >= 2:
        reasons.append('噪声组 |z| ≥ 2')
    if stats['n'] < 100:
        reasons.append('有效配对点 < 100')
    if parse_failure_rate > .10:
        reasons.append('解析失败率 > 10%')
    if patch_hit_rate < .80:
        reasons.append('补丁命中率 < 80%')
    if noise is None:
        warnings.append('未提供噪声组，无法检查重放稳定性；结论需谨慎')
    if stats['se'] is None or (noise is not None and noise['se'] is None):
        warnings.append('UTC 日聚类不足，无法估计 SE；不确定的 z 不视为通过证据')
    guard = abs(open_rate_change) <= open_rate_limit
    if not guard:
        warnings.append('开仓率变化超出阈值，需要人工复核')
    if reasons:
        result = 'invalid'
    elif not guard:
        result = 'fewer_trades'
    elif stats['z'] is not None and all(halves[h]['mean'] is not None and halves[h]['mean'] > 0
                                       for h in ('dev', 'holdout')):
        result = 'strong' if stats['z'] >= z_k else 'screen_pass' if stats['z'] >= 1 else 'fail'
    else:
        result = 'fail'
    return dict(verdict=result, z_K=z_k, K=k, invalid_reasons=reasons,
                open_rate_guard=guard, warnings=warnings)


def _cache_rows(path):
    """Read append-only Go cache, including explicitly sealed crash tails."""
    rows, damaged = [], None
    with open(path, 'rb') as f:
        lines = f.readlines()
    for index, raw in enumerate(lines):
        try:
            row = json.loads(raw)
        except (ValueError, UnicodeDecodeError):
            if index == len(lines) - 1 and not raw.endswith(b'\n'):
                break  # Writer was interrupted; no completed row was lost.
            if damaged is not None:
                raise ValueError('重放缓存存在损坏行')
            damaged = raw.rstrip(b'\n')
            continue
        if damaged is not None:
            if row.get('cache_tail_recovery') != hashlib.sha256(damaged).hexdigest():
                raise ValueError('重放缓存存在损坏行，缺少恢复标记')
            damaged = None
            continue
        if 'record_id' in row:
            rows.append(row)
    if damaged is not None:
        raise ValueError('重放缓存存在未恢复的损坏行')
    return rows


def load_replay(directory, variant_id, model=None, repeat=0, arms=('baseline', 'variant', 'noise')):
    root = Path(directory)
    manifest = json.loads((root / 'sample.json').read_text())
    records = manifest['records']
    ids = {r['record_id'] for r in records}
    if len(ids) != len(records) or not ids:
        raise ValueError('样本清单为空或含重复记录')
    for r in records:
        if not math.isfinite(r['weight']) or r['weight'] <= 0:
            raise ValueError('样本权重必须大于 0 且有限')
    rows = [r for r in _cache_rows(root / 'results.jsonl')
            if r.get('repeat', 0) == repeat and r['record_id'] in ids and r['arm'] in arms]
    variants = {(r.get('variant_id'), r['variant_hash']) for r in rows
                if r['arm'] == 'variant' and variant_id in (r.get('variant_id'), r['variant_hash'])}
    if len(variants) != 1:
        raise ValueError('变体未找到或 id 对应多个哈希，请指定完整哈希')
    vid, vhash = variants.pop()
    available_models = {r['model'] for r in rows if r['arm'] == 'variant' and r['variant_hash'] == vhash}
    if model is None:
        if len(available_models) != 1:
            raise ValueError('缓存含多个模型，请指定 --model 完整缓存模型键')
        model = available_models.pop()
    selected = {arm: {} for arm in arms}
    timestamps = {r['record_id']: signals.parse_utc(r['timestamp']) for r in records}
    # Later attempts supersede failed calls, but a completed key must not change.
    done = set()
    for row in rows:
        arm = row['arm']
        if row['model'] != model or row['variant_hash'] != (vhash if arm == 'variant' else ''):
            continue
        if signals.parse_utc(row['timestamp']) != timestamps[row['record_id']]:
            raise ValueError('缓存时间与共享样本清单不匹配')
        key = (arm, row['record_id'])
        if key in done:
            raise ValueError('缓存出现重复的已完成键')
        selected[arm][row['record_id']] = row
        if row['status'] in ('ok', 'parse_error', 'patch_miss'):
            done.add(key)
    variant_path = root / f'variant-{vhash}.json'
    proposal = json.loads(variant_path.read_text()) if variant_path.exists() else {}
    return manifest, selected, dict(id=vid, hash=vhash, hypothesis=proposal.get('hypothesis', ''),
                                    source=proposal.get('source', ''), model=model, repeat=repeat)


def decision_signals(row):
    """No fill matching, chain merging or historical position lookup."""
    t = signals.parse_utc(row['timestamp']).astimezone(timezone.utc)
    out = []
    for i, d in enumerate(row.get('opens') or []):
        out.append(dict(id=i, symbol=d['symbol'], side=d['side'], kind=d['order_type'],
                        t0=t.isoformat(), t0_ms=int(t.timestamp() * 1000),
                        px0=d.get('limit_price') or d.get('entry_price'), sl0=d.get('stop_loss'),
                        tp0=d.get('take_profit'), exit_mode='range', atr1h=None,
                        outcome='unmatched', fill_px=None, fill_t=None))
    return out


def simulate_record(row, cache, horizon=72, commission=exitsim.TAKER,
                    slippage=exitsim.SLIP, limit_valid_min=45):
    """Only conversion/coverage checks here; orders/PnL use existing adapters."""
    result = dict(r=0.0, trade_rs=[], excluded=False, counts=Counter())
    rules = dict(FALLBACK, be_arm=0, lock_r=0, lock_trim=False, trim_r=0,
                 trail=False, pd_arm=0, tp_cap=0, quick_stop_h=0,
                 use_tp=True, force_mode='range', horizon_h=horizon)
    for sig in decision_signals(row):
        if not sig['sl0'] or sig['sl0'] <= 0:
            result['counts']['missing_sl'] += 1
            continue
        try:
            got = load_bars(cache, sig['symbol'], sig['t0_ms'])
        except ValueError:
            got = None
        if got is None:
            result['counts']['missing_klines'] += 1
            result['excluded'] = True
            continue
        bars, n1m = got
        # Require the full horizon even for early exits: partial coverage must
        # not select signals according to their later SL/TP outcome.
        extra_min = limit_valid_min if sig['kind'] == 'limit' else 0
        end = floor_min(sig['t0_ms']) + (horizon * 60 + extra_min + 10) * 60_000
        if n1m < 10 or not bars or bars[-1][0] + (60_000 if len(bars) <= n1m else 300_000) < end:
            result['counts']['missing_klines'] += 1
            result['excluded'] = True
            continue
        tr = exitsim.build_trade(sig, bars, 'lim' if sig['kind'] == 'limit' else 'mkt',
                                 n1m, limit_valid_min)
        if tr is None:
            result['counts']['invalid_open'] += 1
            continue
        simulated = backtrader_adapter.run_trade(tr, rules, commission, slippage)
        if simulated['r'] is None:
            if simulated['how'] == 'unfilled':
                result['counts']['unfilled'] += 1
            else:
                result['counts']['invalid_open'] += 1
        elif simulated['open']:
            result['excluded'] = True
            result['counts']['unfinished'] += 1
        else:
            result['r'] += simulated['r']
            result['trade_rs'].append(simulated['r'])
    return result


def _signature(row):
    # Compare executable decisions, ignoring prose/annotation randomness.
    fields = ('symbol', 'action', 'price', 'stop_loss', 'take_profit', 'position_size_usd', 'leverage')
    if 'decisions' in row:
        return sorted(tuple((d.get(k) or 0) if k not in ('symbol', 'action') else d.get(k, '')
                            for k in fields) for d in (row['decisions'] or []))
    return sorted((d['symbol'], d['side'], d['order_type'], d.get('entry_price', 0),
                   d.get('stop_loss', 0), d.get('take_profit', 0)) for d in row.get('opens', []))


def _auxiliary(pairs, rows, scored):
    total_weight = sum(p['weight'] for p in pairs)
    rates = {}
    for arm in ('baseline', 'variant'):
        rates[arm] = (sum(p['weight'] * bool(rows[arm][p['record_id']].get('opens')) for p in pairs)
                      / total_weight if total_weight else 0.0)
    change = rates['variant'] / rates['baseline'] - 1 if rates['baseline'] else (math.inf if rates['variant'] else 0.0)
    agreement, flips = 0.0, 0
    trade_avgs = {}
    for p in pairs:
        b, v = (rows[a][p['record_id']] for a in ('baseline', 'variant'))
        agreement += p['weight'] * (_signature(b) == _signature(v))
        bside = {(d['symbol'], d['side']) for d in b.get('opens', [])}
        vside = {(d['symbol'], d['side']) for d in v.get('opens', [])}
        flips += sum((sym, 'short' if side == 'long' else 'long') in vside for sym, side in bside)
    for arm in ('baseline', 'variant'):
        values = [(p['weight'], r) for p in pairs for r in scored[(arm, p['record_id'])]['trade_rs']]
        trade_avgs[arm] = (sum(w * r for w, r in values) / sum(w for w, _ in values)) if values else None
    return dict(open_rates=rates, open_rate_change=change, direction_flips=flips,
                decision_agreement=agreement / total_weight if total_weight else None,
                avgR_per_open=trade_avgs)


def failure_mode_changes(manifest, pairs, rows, db_path=None):
    path = db_path or manifest.get('db')
    if not path or not Path(path).is_file():
        return dict(status='unavailable', reason='无可读交易日志数据库')
    try:
        with closing(signals.connect_ro(str(Path(path).resolve()))) as conn:
            columns = {r[1] for r in conn.execute('PRAGMA table_info(trade_journal)')}
            if 'failure_mode' not in columns:
                return dict(status='unavailable', reason='trade_journal 尚无 failure_mode 列')
            journals = conn.execute('SELECT symbol, lower(side), entry_time, failure_mode FROM trade_journal '
                                    "WHERE trader_id=? AND ai_managed=1 AND failure_mode!=''",
                                    (manifest['trader_id'],)).fetchall()
            # Match the nearest ORIGINAL successful open, including unsampled
            # records. A newly replayed open cannot own a historical journal.
            original = defaultdict(list)
            for rid, timestamp, decisions in conn.execute(
                    "SELECT id, timestamp, decisions FROM decision_records WHERE trader_id=? AND success=1",
                    (manifest['trader_id'],)):
                try:
                    ts = int(signals.parse_utc(timestamp).timestamp() * 1000)
                    for d in json.loads(decisions or '[]'):
                        action = d.get('action')
                        if action in signals.OPEN_ACTIONS and d.get('success'):
                            original[(d.get('symbol'), signals._side(action))].append((ts, rid))
                except (ValueError, TypeError, AttributeError):
                    continue
        matches = defaultdict(list)
        by_id = {p['record_id']: p for p in pairs}
        for sym, side, entry_ms, mode in journals:
            candidates = [(ts, rid) for ts, rid in original[(sym, side)]
                          if 0 <= entry_ms - ts <= 30 * 60_000]
            if candidates:
                _, rid = max(candidates)
                p = by_id.get(rid)
                if p is not None and p not in matches[mode]:
                    matches[mode].append(p)
        return dict(status='available', by_failure_mode={mode: weighted_cluster(ps) for mode, ps in matches.items()},
                    match='同币同方向、入场前30分钟内最近原始成功开仓决策（描述性关联）')
    except (sqlite3.Error, OSError, ValueError) as exc:
        return dict(status='unavailable', reason=str(exc))


def score(directory, variant_id, cache, horizon=72, model=None, repeat=0,
          arms=('baseline', 'variant', 'noise'), do_fetch=False, simulator=None,
          commission=exitsim.TAKER, slippage=exitsim.SLIP, limit_valid_min=45, db_path=None):
    for value in (horizon, limit_valid_min):
        if not math.isfinite(value) or value <= 0:
            raise ValueError('持仓上限和限价有效期必须大于 0 且有限')
    if any(not math.isfinite(v) or v < 0 for v in (commission, slippage)):
        raise ValueError('手续费和滑点必须是非负有限数')
    if not {'baseline', 'variant'} <= set(arms) or len(set(arms)) != len(arms) or set(arms) - {'baseline', 'variant', 'noise'}:
        raise ValueError('--arms 必须包含 baseline,variant，可选 noise')
    manifest, rows, proposal = load_replay(directory, variant_id, model, repeat, arms)
    if not rows['baseline'] or not rows['variant']:
        raise ValueError('缺少同模型的基线或变体组')
    patch_hit_rate = 1 - sum(r['status'] == 'patch_miss' for r in rows['variant'].values()) / len(manifest['records'])
    simulator = simulator or simulate_record
    if do_fetch:
        sigs = [s for arm in rows.values() for row in arm.values() if row['status'] == 'ok'
                for s in decision_signals(row)]
        fetch.fetch_all(sigs, cache)
    scored, counts = {}, Counter()
    failures_by_arm, attempted_by_arm = Counter(), Counter()
    for arm, arm_rows in rows.items():
        for rid, row in arm_rows.items():
            if row['status'] in ('ok', 'parse_error'):
                attempted_by_arm[arm] += 1
            if row.get('parse_failure') or row['status'] == 'parse_error':
                failures_by_arm[arm] += 1
            if row['status'] != 'ok' or row.get('parse_failure'):
                counts[row['status'] if row['status'] != 'ok' else 'parse_error'] += 1
                continue
            result = simulator(row, cache, horizon, commission, slippage, limit_valid_min)
            counts.update(result['counts'])
            if not result['excluded']:
                scored[(arm, rid)] = result
    pairs, noise_pairs = [], []
    for rec in manifest['records']:
        rid = rec['record_id']
        base = scored.get(('baseline', rid))
        if base is None:
            continue
        for arm, dest in (('variant', pairs), ('noise', noise_pairs)):
            other = scored.get((arm, rid))
            if other is not None:
                dest.append(dict(record_id=rid, timestamp=rec['timestamp'], weight=rec['weight'],
                                 day=signals.parse_utc(rec['timestamp']).astimezone(timezone.utc).date().isoformat(),
                                 diff=other['r'] - base['r'], baseline_r=base['r'], arm_r=other['r']))
    parse_rates = {a: failures_by_arm[a] / n for a, n in attempted_by_arm.items() if n}
    main_stats = weighted_cluster(pairs)
    has_noise = bool(rows.get('noise'))
    noise_stats = weighted_cluster(noise_pairs) if has_noise else None
    aux = _auxiliary(pairs, rows, scored)
    if noise_pairs:
        weight = sum(p['weight'] for p in noise_pairs)
        aux['noise_decision_agreement'] = sum(p['weight'] * (
            _signature(rows['baseline'][p['record_id']]) == _signature(rows['noise'][p['record_id']]))
            for p in noise_pairs) / weight
        vals = [(p['weight'], r) for p in noise_pairs for r in scored[('noise', p['record_id'])]['trade_rs']]
        aux['avgR_per_open']['noise'] = sum(w*r for w, r in vals) / sum(w for w, _ in vals) if vals else None
    else:
        aux['noise_decision_agreement'] = None
    aux['failure_mode'] = failure_mode_changes(manifest, pairs, rows, db_path)
    params = {k: v for k, v in manifest.items() if k != 'records'}
    params['sample_ids_hash'] = hashlib.sha256(json.dumps(manifest['records'], sort_keys=True).encode()).hexdigest()
    warnings = [] if has_noise else ['未提供噪声组，未执行模型随机性健康检查']
    if len(pairs) != len(manifest['records']):
        warnings.append('存在缺失或排除的配对点，请检查覆盖率')
    return dict(variant_id=proposal['id'], variant_hash=proposal['hash'], hypothesis=proposal['hypothesis'],
                source=proposal['source'], model=proposal['model'], repeat=repeat, sample=params,
                horizon_h=horizon, engine='backtrader', commission=commission, slippage=slippage,
                limit_valid_min=limit_valid_min, limit_fill=backtrader_adapter.LIMIT_FILL_DEFAULT,
                stats=main_stats, halves=split_halves(pairs), noise=noise_stats, auxiliary=aux,
                patch_hit_rate=patch_hit_rate,
                parse_failure_rate=max(parse_rates.values(), default=0), parse_failure_rates=parse_rates, counts=dict(counts),
                available_rows={a: len(v) for a, v in rows.items()}, pairs=pairs, noise_pairs=noise_pairs,
                warnings=warnings)


def append_ledger(path, result, track, baseline_version, change_type='D', idea_id=None,
                  open_rate_limit=.5):
    """Use the shared track-local S1 budget and append-only ledger lock.

    Omit unbounded pair arrays; keep summary statistics, sample hash and scoring
    parameters so the replay manifest/cache can reproduce the evaluation.
    """
    if change_type not in ('A', 'D'):
        raise ValueError('重放评分 change_type 必须是 A/D')

    def build(k, previous):
        judgment = screen_verdict(result['stats'], result['halves'], result['noise'],
                                  result['parse_failure_rate'], result['patch_hit_rate'], k,
                                  result['auxiliary']['open_rate_change'], open_rate_limit)
        row = {key: value for key, value in result.items() if key not in ('pairs', 'noise_pairs')}
        row['warnings'] = result['warnings'] + judgment.pop('warnings')
        rates = result['auxiliary']['open_rates']
        row.update(judgment, stage='S1', baseline_version=baseline_version, change_type=change_type,
                   idea_id=result['variant_id'] if idea_id is None else idea_id,
                   delta=result['stats']['mean'], se=result['stats']['se'], z=result['stats']['z'],
                   trade_ratio=rates['variant'] / rates['baseline'] if rates['baseline'] else None,
                   placebo_pct=None, dev_delta=result['halves']['dev']['mean'],
                   holdout_delta=result['halves']['holdout']['mean'], open_rate_limit=open_rate_limit,
                   scored_at=datetime.now(timezone.utc).isoformat())
        return row

    return evolution_ledger.append_event(path, track, build)


def print_summary(result):
    s = result['stats']
    print(f"重放结论：{result['verdict']}；有效配对 {s['n']}，UTC 日 {s['days']}，K={result['K']}，z_K={result['z_K']:.3f}")
    print(f"加权 ΔR={s['mean']}，聚类 SE={s['se']}，95% CI={s['ci95']}，z={s['z']}")
    print(f"时间二分：dev {result['halves']['dev']}；holdout {result['halves']['holdout']}")
    print(f"补丁命中率 {result['patch_hit_rate']:.1%}；解析失败率 {result['parse_failure_rate']:.1%}；开仓率 {result['auxiliary']['open_rates']}；变化 {result['auxiliary']['open_rate_change']:+.1%}")
    print(f"噪声组：{result['noise']}；缺失 K 线 {result['counts'].get('missing_klines', 0)}，计数 {result['counts']}")
    aux = result['auxiliary']
    print(f"决策一致率：基线/变体 {aux.get('decision_agreement')}，基线/噪声 {aux.get('noise_decision_agreement')}；方向翻转 {aux.get('direction_flips')}")
    print(f"每笔成交开仓 avgR：{aux.get('avgR_per_open')}；failure_mode：{aux.get('failure_mode')}")
    for reason in result['invalid_reasons'] + result['warnings']:
        print('提示：' + reason)
    print('screen_pass/strong 仅进入线上配对队列，不直接上线；fewer_trades 归入基线交易频率问题')


def cmd_replay_score(a):
    if a.repeat < 0:
        raise ValueError('repeat 必须是非负整数')
    backtrader_adapter._backtrader()
    backtrader_adapter.LIMIT_FILL_DEFAULT = a.limit_fill
    try:
        result = score(a.replay_dir, a.variant, a.cache, a.horizon, a.model, a.repeat,
                       tuple(a.arms.split(',')), a.fetch, commission=a.commission,
                       slippage=a.slippage, limit_valid_min=a.limit_valid_min, db_path=a.db)
        result = append_ledger(a.ledger, result, track=a.track, baseline_version=a.baseline_version,
                               change_type=a.change_type, idea_id=a.idea_id, open_rate_limit=a.open_rate_limit)
    except OSError as exc:
        raise ValueError(f'重放文件读写失败：{exc}') from exc
    print_summary(result)
    return 0
