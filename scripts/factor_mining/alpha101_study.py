"""Read-only predeclared Alpha101 zoo-entry study; never updates mining state.

python -m scripts.factor_mining.alpha101_study --root /path/to/factor_mining \
    --out .alpha101_out/report.md --json .alpha101_out/report.json
"""
from __future__ import annotations

import argparse
from collections import Counter
import datetime as dt
import hashlib
import json
import math
from pathlib import Path
import time

from . import config, evaluate, store, zoo
from .alpha101 import ALPHAS
from .expr import evaluate_expr, validate_expr
from .panel import forward_target, load_panels, make_splits
from .sensitivity_20261028 import (
    FUNDING_SETTLEMENTS_PER_DAY, funding_adjusted_ls, holding_funding,
    old_universe, still_accepted,
)


def ledger_counts(rows):
    """Every recorded trial counts, including repeated IDs and invalid trials."""
    return Counter(row['horizon'] for row in rows)


def classify_rows(rows, ledger):
    """Fix each K only after counting all evaluable entries in this study."""
    counts = Counter(row['horizon'] for row in rows if row['metrics'] is not None)
    trials = {h: ledger[h] + counts[h] for h in sorted({r['horizon'] for r in rows})}
    for row in rows:
        h, metrics = row['horizon'], row['metrics']
        row['K'] = trials[h]
        row['t_req'] = evaluate.t_required(trials[h])
        row['status'] = (evaluate.classify(metrics['train'], metrics['val'], trials[h], row['t_req'])
                         if metrics is not None else 'not_evaluable')
        row['base_status'] = row['status']
    return trials


def holm_passes(p_values, alpha=0.05):
    """Holm step-down, including p=1 for non-evaluable non-skipped hypotheses."""
    ordered = sorted(enumerate(p_values), key=lambda pair: (pair[1], pair[0]))
    passed = [False] * len(p_values)
    for k, (i, p) in enumerate(ordered):
        if not math.isfinite(p) or p > alpha / (len(ordered) - k):
            break
        passed[i] = True
    return passed


def integration_pass(status, redundant, market, funding_pass=None):
    return status == 'accepted' and not redundant and (market != 'crypto' or funding_pass is True)


def output_paths(root, out, json_out):
    """Resolve symlinks before any work; writing anywhere inside root is forbidden."""
    root = Path(root).resolve()
    paths = [Path(p).resolve() for p in (out, json_out)]
    if any(p.is_relative_to(root) for p in paths):
        raise ValueError('outputs must be outside the read-only --root directory')
    if paths[0] == paths[1]:
        raise ValueError('Markdown and JSON outputs must be different files')
    return paths


def _sha(path):
    return hashlib.sha256(Path(path).read_bytes()).hexdigest()


def _sample_ranks(values, direction, panels, dates):
    return (values.loc[dates] * direction).where(panels.mask.loc[dates]).rank(axis=1, method='average')


def _top(rows, h):
    return sorted((r for r in rows if r['horizon'] == h and r['metrics'] is not None),
                  key=lambda r: (-r['metrics']['train']['t'], r['alpha']))[:15]


def _progress(message, start):
    print(f'[{time.monotonic() - start:.1f}s] {message}', flush=True)


def analyse(root, markets=('crypto', 'us'), progress=True):
    """All input operations are reads. Returned result contains no panel objects."""
    start = time.monotonic()
    root = Path(root).resolve()
    result = dict(root=str(root), generated_at=dt.datetime.now(dt.timezone.utc).isoformat(),
                  predeclared=dict(window='floor, minimum 1; full windows',
                                   industry='cross-sectional demean over all observed names',
                                   redundancy_threshold=config.CORR_REDUNDANT,
                                   holm_alpha=0.05, holm_unevaluable_p=1,
                                   funding_settlements_per_day=FUNDING_SETTLEMENTS_PER_DAY),
                  source_sha256={name: _sha(Path(__file__).with_name(name)) for name in
                                 ('alpha101.py', 'alpha101_study.py', 'evaluate.py', 'expr.py',
                                  'panel.py', 'config.py', 'zoo.py', 'sensitivity_20261028.py')},
                  skipped=[dict(alpha=n, reason=item['skipped']) for n, item in ALPHAS.items() if item['skipped']],
                  formula_uncertainties=[], markets={})
    for market in markets:
        market_start = time.monotonic()
        if progress:
            _progress(f'{market}: loading panels and read-only ledger/zoo snapshots', start)
        cfg = config.MARKETS[market]
        panels = load_panels(market, root)
        horizons = cfg['horizons']
        index = panels.mask.index
        ledger_path, zoo_path = root / market / 'ledger.jsonl', root / market / 'zoo.json'
        input_hashes = {str(p.relative_to(root)): _sha(p) for p in (ledger_path, zoo_path)}
        ledger = ledger_counts(store.read_jsonl(ledger_path))
        snapshot = store.read_json(zoo_path)
        # Forward-monitoring/holdout records in zoo are never used in the study.
        existing = [{k: f[k] for k in ('id', 'expr', 'horizon', 'direction')}
                    for f in snapshot['factors'] if f['status'] == 'accepted']
        splits = {h: make_splits(index, h, panels.windows) for h in horizons}
        targets = {h: forward_target(panels, h) for h in horizons}
        values_cache, rows = {}, []
        for n, item in ALPHAS.items():
            if item['skipped']:
                continue
            values = item['fn'](panels.fields)
            values_cache[n] = values
            for h in horizons:
                excess, raw = targets[h]
                metrics = evaluate.evaluate_factor(values, excess, raw, panels.mask, splits[h], h, cfg)
                rows.append(dict(alpha=n, market=market, horizon=h, metrics=metrics,
                                 indneutral=item['indneutral'], redundant=False, corr_max=None,
                                 corr_with=None, seed_corr_abs=None, seed_corr_signed=None,
                                 seed_name=None, funding=None, funding_pass=None,
                                 old_universe=None, old_universe_pass=None, integrable=False))
            if progress and n % 10 == 0:
                _progress(f'{market}: Alpha#{n}, evaluated all {len(horizons)} horizons', start)
        trials = classify_rows(rows, ledger)
        p_values = [math.erfc(abs(r['metrics']['train']['t']) / math.sqrt(2))
                    if r['metrics'] is not None else 1. for r in rows]
        for row, p, passed in zip(rows, p_values, holm_passes(p_values)):
            row['train_p_two_sided'], row['holm_pass'] = p, passed

        # Compute zoo redundancy for all accepted entries and for descriptive top15.
        alpha_ranks = {}
        for h in horizons:
            visible = splits[h]['train'] | splits[h]['val']
            dates = index[visible][::5]
            top = _top(rows, h)
            needed = {r['alpha'] for r in top} | {r['alpha'] for r in rows
                                                  if r['horizon'] == h and r['base_status'] == 'accepted'}
            for n in needed:
                row = next(r for r in rows if r['alpha'] == n and r['horizon'] == h)
                alpha_ranks[n, h] = _sample_ranks(values_cache[n], row['metrics']['direction'], panels, dates)
                row['corr_max'] = 0.  # Same signed-positive maximum convention as miner.
            for other in (f for f in existing if f['horizon'] == h):
                other_values = evaluate_expr(other['expr'], panels.fields)
                other_rank = _sample_ranks(other_values, other['direction'], panels, dates)
                for row in (r for r in rows if r['horizon'] == h and r['alpha'] in needed):
                    corr = evaluate.cross_corr(alpha_ranks[row['alpha'], h], other_rank, dates)
                    if corr > row['corr_max']:
                        row['corr_max'], row['corr_with'] = corr, other['id']
            for row in rows:
                if row['horizon'] == h and row['base_status'] == 'accepted':
                    row['redundant'] = row['corr_max'] >= config.CORR_REDUNDANT
                    if row['redundant']:
                        row['status'] = 'redundant'

        # seed_factors contains the Alpha158 library plus nofx extensions. Use the
        # whole function's actual returned set, as predeclared in the task.
        seeds = zoo.seed_factors(market)
        unavailable_seeds = []
        for seed in seeds:
            missing = set(validate_expr(seed['expr'])['fields']) - set(panels.fields) - {'ret'}
            if missing:
                unavailable_seeds.append(dict(name=seed['name'], missing=sorted(missing)))
                continue
            seed_values = evaluate_expr(seed['expr'], panels.fields)
            for h in horizons:
                dates = index[splits[h]['train'] | splits[h]['val']][::5]
                seed_rank = _sample_ranks(seed_values, 1, panels, dates)
                for row in _top(rows, h):
                    corr = evaluate.cross_corr(alpha_ranks[row['alpha'], h], seed_rank, dates)
                    if row['seed_corr_abs'] is None or abs(corr) > row['seed_corr_abs']:
                        row['seed_corr_abs'], row['seed_corr_signed'], row['seed_name'] = abs(corr), corr, seed['name']

        subset_meta = None
        if market == 'crypto':
            subset = old_universe(panels)
            subset_meta = dict(n_symbols=len(subset.mask.columns), symbols=list(subset.mask.columns),
                               cutoff='2022-01-01', fixed_training_direction=True)
            subset_targets = {h: forward_target(subset, h) for h in horizons}
            # Funding is unavailable if the panel has no such field; never assume zero.
            accruals = ({h: holding_funding(panels.fields['funding'], h) for h in horizons}
                        if 'funding' in panels.fields else {})
            accepted = [r for r in rows if r['base_status'] == 'accepted']
            for n in sorted({r['alpha'] for r in accepted}):
                sub_values = ALPHAS[n]['fn'](subset.fields)
                for row in (r for r in accepted if r['alpha'] == n):
                    h, direction = row['horizon'], row['metrics']['direction']
                    excess, raw = targets[h]
                    sub_excess, sub_raw = subset_targets[h]
                    sub_metrics = evaluate.evaluate_factor(sub_values, sub_excess, sub_raw, subset.mask,
                                                           splits[h], h, {**cfg, 'direction': direction})
                    row['old_universe'] = sub_metrics
                    row['old_universe_pass'] = still_accepted(sub_metrics, trials[h])
                    if not row['redundant']:
                        if h in accruals:
                            row['funding'] = {s: funding_adjusted_ls(values_cache[n] * direction, raw,
                                                                    accruals[h], panels.mask, splits[h][s], h, cfg)
                                              for s in ('train', 'val')}
                            row['funding_pass'] = still_accepted(row['metrics'], trials[h], row['funding'])
                        else:
                            row['funding_pass'] = False
                            row['funding_reason'] = 'funding field unavailable'
        for row in rows:
            row['integrable'] = integration_pass(row['base_status'], row['redundant'], market, row['funding_pass'])

        summary = []
        for h in horizons:
            current = [r for r in rows if r['horizon'] == h]
            counts = Counter(r['status'] for r in current)
            summary.append(dict(market=market, horizon=h, ledger_K=ledger[h],
                                evaluable=sum(r['metrics'] is not None for r in current),
                                K=trials[h], t_req=evaluate.t_required(trials[h]),
                                rejected=counts['rejected'], candidate=counts['candidate'],
                                accepted=counts['accepted'], redundant=counts['redundant'],
                                accepted_before_redundancy=sum(r['base_status'] == 'accepted' for r in current),
                                not_evaluable=counts['not_evaluable'],
                                holm_pass=sum(r['holm_pass'] for r in current),
                                integrable=sum(r['integrable'] for r in current),
                                ranges=splits[h]['ranges'], rolling=splits[h]['rolling']))
        for name, digest in input_hashes.items():
            if _sha(root / name) != digest:
                raise RuntimeError(f'input snapshot changed during study: {name}')
        result['markets'][market] = dict(data=panels.meta, calendar_rows=len(index),
                                         horizons=list(horizons), summary=summary, rows=rows,
                                         top15={str(h): [r['alpha'] for r in _top(rows, h)] for h in horizons},
                                         holm_family_size=len(rows), holm_pass=sum(r['holm_pass'] for r in rows),
                                         zoo_accepted_count=len(existing), seed_count=len(seeds),
                                         unavailable_seeds=unavailable_seeds,
                                         old_universe=subset_meta, input_sha256=input_hashes,
                                         seconds=time.monotonic() - market_start)
        if progress:
            _progress(f'{market}: complete, INTEGRABLE={sum(r["integrable"] for r in rows)}', start)
    result['integrable'] = [dict(market=m, alpha=r['alpha'], horizon=r['horizon'], direction=r['metrics']['direction'])
                            for m, block in result['markets'].items() for r in block['rows'] if r['integrable']]
    result['conclusion'] = '可作为种子进入 zoo 走每晚挖掘 + 前向校验' if result['integrable'] else '无可融合'
    result['seconds'] = time.monotonic() - start
    return store.finite(result)


def _num(x, percent=False):
    return 'NA' if x is None else (f'{x:.2%}' if percent else f'{x:.4f}')


def _pair(metrics, key):
    return 'NA' if metrics is None else ' / '.join(_num(metrics[s][key], key == 'ls_ret') for s in ('train', 'val'))


def summary_table(result):
    lines = ['| market | h | 可评估 | not_evaluable | rejected | candidate | accepted | redundant | Holm通过 | INTEGRABLE |',
             '|---|---:|---:|---:|---:|---:|---:|---:|---:|---:|']
    for market, block in result['markets'].items():
        for row in block['summary']:
            lines.append('| ' + ' | '.join(str(row[k]) for k in ('market', 'horizon', 'evaluable', 'not_evaluable',
                                                                'rejected', 'candidate', 'accepted', 'redundant',
                                                                'holm_pass', 'integrable')) + ' |')
    return '\n'.join(lines)


def top_table(block, h):
    lines = ['| Alpha | 方向 | train IC | train t | train ls_ret | val IC | val t | val ls_ret | status | zoo最大相关 | Alpha158种子最大\u007c相关\u007c（名称） |',
             '|---:|---:|---:|---:|---:|---:|---:|---:|---|---:|---|']
    # Escape literal pipes inside Markdown headers.
    lines[0] = lines[0].replace('最大|相关|', '最大\\|相关\\|')
    for row in _top(block['rows'], h):
        m = row['metrics']
        lines.append(f"| #{row['alpha']} | {m['direction']:+d} | {_num(m['train']['ic'])} | {_num(m['train']['t'])} | "
                     f"{_num(m['train']['ls_ret'], True)} | {_num(m['val']['ic'])} | {_num(m['val']['t'])} | "
                     f"{_num(m['val']['ls_ret'], True)} | {row['status']} | {_num(row['corr_max'])} | "
                     f"{_num(row['seed_corr_abs'])}（{row['seed_name']}） |")
    return '\n'.join(lines)


def render_report(result):
    lines = ['# WorldQuant Alpha101 只读预声明检验', '',
             f"结论：**{result['conclusion']}**。INTEGRABLE 共 {len(result['integrable'])} 个 alpha×market×h 条目。", '',
             f"只读根目录：`{result['root']}`；运行时间 {result['seconds']:.2f} 秒。生成时间：{result['generated_at']}。", '',
             '## 固定口径与偏差', '',
             '决策 t 收盘，t+1 开盘入场、t+1+h 开盘出场。只调用 forward_target、make_splits 和冻结 evaluate_factor；'
             'holdout 标签封存。目标完全沿用 forward_target：crypto 为合资格币池截面去均值收益，US 为相对 SPY 开盘收益。'
             'US 的共同基准与截面去均值相差每日常数，对 RankIC 和多空收益无影响。', '',
             'rank 在全部有数据标的上取平均秩/非缺失个数；不套 universe mask。窗口小数一律 floor，最小 1，滚动要求满窗；'
             'stddev/covariance 为 ddof=1，相关任一方零方差为 NaN。ts_rank=当前值平均秩/d；'
             'argmax/argmin 最老=1、今天=d，并列取最早。decay_linear 从旧到新权重 1..d；'
             'scale 按行绝对值和缩放；signedpower 保留底数符号；普通幂负底非整数指数为 NaN。'
             '比较为 1/0 且 NaN 传播；三元条件 NaN 也为 NaN；min/max 的数字第二参数代表时序窗口。', '',
             'returns=close/delay(close,1)-1；adv 为 volume×vwap 的满窗均值（美元成交额，非成交量）。'
             'indneutralize 缺行业资料，统一改为全部有数据标的逐行截面去均值；嵌套调用保留。'
             '这是对原行业中性因子的系统偏差，相关条目在 JSON 标记 indneutral。#56 因 needs cap 跳过。'
             'crypto vwap 缓存来源 quoteVolume/volume（无有效成交额时采集器回退 close）；'
             'US vwap 是 (high+low+close)/3 代理，并非真实成交加权价。crypto 使用 7×24 UTC 日历；'
             'US 使用 SPY 交易日日历。未填补缺失观测，不为通过门槛添加 epsilon。', '',
             'K_h=该市场 ledger 中该 horizon 全部有效 JSON 行数+本研究该 horizon 可评估 Alpha 数；'
             '调用原 t_required/classify。方向只由 train IC 符号选择。Holm 为每市场全部 100×3 个假设的描述性检验，'
             '两侧 p=erfc(|train NW t|/sqrt(2))，不可评估项 p=1；α=0.05，不参与融合门槛。', '',
             '冗余只比较同市场同 h 的现有 zoo accepted：visible(train|val) 每 5 行抽样，在 mask 内方向调整后逐行 rank，'
             '沿用 cross_corr 的平均有符号相关，最大值以 0 为下限，达到 0.8 判 redundant；本批 Alpha 之间不另加去重门槛。'
             '前15名与 zoo.seed_factors(market) 实际返回的全部种子（Alpha158 及 nofx 扩展）比较最大绝对相关，仅描述。', '',
             'crypto 非冗余 accepted 的含资金费收益调用 holding_funding/funding_adjusted_ls：日均结算费率×3，'
             '累加 t+1..t+h，多头付空头收，沿用原持仓、成本和换手；所选腿缺费率则该段 NA，不可通过融合。'
             '每天3次是敏感性假设，未重建动态结算次数或浮动名义金额。旧币池按2022年前有效历史筛选，'
             '重算截面公式、资格及流动性池，固定全池训练方向；仅报告，不作门槛。仍有存活快照/退市缺失偏差。', '',
             'INTEGRABLE = 原 classify accepted 且非 redundant 且（crypto）资金费后 still_accepted；'
             '旧币池与 Holm 均不改变判定。accepted/redundant 为互斥最终状态；accepted 明细覆盖冗余判定前的全部 accepted。', '',
             '## 数据、切分与门槛', '',
             '| market | 数据区间 | 日历行数 | 标的数 | train/val/holdout配置 | zoo accepted数 | 种子数 | 秒 |',
             '|---|---|---:|---:|---|---:|---:|---:|']
    for market, block in result['markets'].items():
        meta = block['data']
        windows = config.MARKETS[market]['windows']
        lines.append(f"| {market} | {meta['start']} ～ {meta['end']} | {block['calendar_rows']} | {meta['symbols']} | "
                     f"{windows['train']}/{windows['val']}/{windows['holdout']} | {block['zoo_accepted_count']} | "
                     f"{block['seed_count']} | {block['seconds']:.2f} |")
    lines += ['', '| market | h | train | val | holdout（封存） | ledger行数 | 新可评估数 | K_h | t_req |',
              '|---|---:|---|---|---|---:|---:|---:|---:|']
    for market, block in result['markets'].items():
        for r in block['summary']:
            dates = [' ～ '.join(r['ranges'][s]) for s in ('train', 'val', 'holdout')]
            lines.append(f"| {market} | {r['horizon']} | {' | '.join(dates)} | {r['ledger_K']} | "
                         f"{r['evaluable']} | {r['K']} | {r['t_req']:.6f} |")
    lines += ['', '## 每个 market×h 汇总', '', summary_table(result), '', '## train t 前15（各市场×h）', '']
    for market, block in result['markets'].items():
        for h in block['horizons']:
            lines += [f'### {market} h={h}', '', top_table(block, h), '']
    lines += ['## 全部 accepted 明细（冗余判定前）', '',
              'IC、t、ls_ret、资金费收益各格均为 train / val；收益为含交易成本的算术年化。', '',
              '| market | h | Alpha | 方向 | IC | t | ls_ret | 最终status | zoo相关/对象 | 含资金费ls_ret | 资金费仍accepted | 旧池IC | 旧池t | 旧池ls_ret | 旧池仍accepted | INTEGRABLE |',
              '|---|---:|---:|---:|---|---|---|---|---|---|---|---|---|---|---|---|']
    for market, block in result['markets'].items():
        for r in block['rows']:
            if r['base_status'] != 'accepted':
                continue
            m = r['metrics']
            flag = lambda x: 'NA' if x is None else ('是' if x else '否')
            lines.append(f"| {market} | {r['horizon']} | #{r['alpha']} | {m['direction']:+d} | {_pair(m, 'ic')} | "
                         f"{_pair(m, 't')} | {_pair(m, 'ls_ret')} | {r['status']} | {_num(r['corr_max'])}/{r['corr_with']} | "
                         f"{_pair(r['funding'], 'ls_ret')} | {flag(r['funding_pass'])} | {_pair(r['old_universe'], 'ic')} | "
                         f"{_pair(r['old_universe'], 't')} | {_pair(r['old_universe'], 'ls_ret')} | "
                         f"{flag(r['old_universe_pass'])} | {flag(r['integrable'])} |")
            if r['funding'] is not None:
                for segment, funding in r['funding'].items():
                    lines.append(f"<!-- {market}/h{r['horizon']}/Alpha{r['alpha']}/{segment}: "
                                 f"missing funding legs={funding['missing_legs']}/{funding['selected_legs']}; "
                                 f"n_portfolios={funding['n_portfolios']} -->")
    for market, block in result['markets'].items():
        if block['old_universe'] is not None:
            sub = block['old_universe']
            lines += ['', f"旧币池 {sub['n_symbols']} 币：" + '、'.join(sub['symbols']) + '。']
    lines += ['', '## INTEGRABLE 清单', '', '| market | h | Alpha | 方向 |', '|---|---:|---:|---:|']
    for r in result['integrable']:
        lines.append(f"| {r['market']} | {r['horizon']} | #{r['alpha']} | {r['direction']:+d} |")
    if not result['integrable']:
        lines += ['', '无。']
    lines += ['', f"总结论：**{result['conclusion']}**。", '', '## 附录：全部 Alpha×market×h', '',
              '| market | h | Alpha | status | train t | val t |', '|---|---:|---:|---|---:|---:|']
    for market, block in result['markets'].items():
        for r in block['rows']:
            m = r['metrics']
            lines.append(f"| {market} | {r['horizon']} | #{r['alpha']} | {r['status']} | "
                         f"{_num(m['train']['t'] if m else None)} | {_num(m['val']['t'] if m else None)} |")
        for h in block['horizons']:
            for r in result['skipped']:
                lines.append(f"| {market} | {h} | #{r['alpha']} | skipped: {r['reason']} | NA | NA |")
    lines += ['', '### skipped', '']
    lines.extend(f"- Alpha#{r['alpha']}: {r['reason']}" for r in result['skipped'])
    lines += ['', '公式翻译未决疑点：无。公式字符串逐字保留本地附录；#29 的 min(...,5) 按数字参数解释为时序最小值；'
              '#100 两次嵌套 indneutralize 均保留。行业近似、vwap代理、资金费结算频次均为上述预声明偏差。', '',
              '### 可复核 SHA256', '']
    lines.extend(f"- `{name}`: `{digest}`" for name, digest in result['source_sha256'].items())
    for market, block in result['markets'].items():
        lines.extend(f"- `{name}`: `{digest}`" for name, digest in block['input_sha256'].items())
    lines += ['', f"运行耗时：{result['seconds']:.2f} 秒（不含测试与 Markdown/JSON 写出）。", '']
    return '\n'.join(lines)


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--root', type=Path, required=True)
    parser.add_argument('--out', type=Path, default=Path('.alpha101_out/report.md'))
    parser.add_argument('--json', type=Path, default=Path('.alpha101_out/report.json'))
    parser.add_argument('--markets', default='crypto,us')
    args = parser.parse_args(argv)
    markets = tuple(args.markets.split(','))
    if not markets or len(set(markets)) != len(markets) or any(m not in config.MARKETS for m in markets):
        parser.error('--markets must contain distinct names from crypto,us')
    try:
        out, json_out = output_paths(args.root, args.out, args.json)
    except ValueError as exc:
        parser.error(str(exc))
    result = analyse(args.root, markets)
    markdown = render_report(result)
    payload = json.dumps(result, ensure_ascii=False, indent=2, allow_nan=False) + '\n'
    for path, content in ((out, markdown), (json_out, payload)):
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_text(content, encoding='utf-8')
    print(summary_table(result))
    print(f"{result['conclusion']}; seconds={result['seconds']:.2f}; wrote {out} and {json_out}")
    return 0


if __name__ == '__main__':
    raise SystemExit(main())
