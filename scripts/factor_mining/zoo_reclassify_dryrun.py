"""Read-only H1 zoo reclassification under old NW and alternative B conventions.

See docs/research/2026-11-08-ic-overlap-h1-prereg.md, section 3.
Only reports outside data_fetch.ROOT are written.

  python -m scripts.factor_mining.zoo_reclassify_dryrun --market crypto --out /tmp/h1
"""
import argparse
from collections import Counter, defaultdict
from copy import deepcopy
import hashlib
from pathlib import Path

from . import data_fetch, evaluate, live_scores, store
from .config import MARKETS
from .expr import ExprError, evaluate_expr
from .miner import Miner
from .panel import forward_target, load_panels, make_splits

STATUSES = ('accepted', 'candidate', 'redundant', 'rejected', 'dropped')


def _sha256(path):
    return hashlib.sha256(path.read_bytes()).hexdigest() if path.exists() else None


def _reevaluate(factors, panels, targets, splits, cfg):
    grouped = defaultdict(list)
    for factor in factors:
        grouped[factor['id']].append(factor)
    old, new, missing = {}, {}, {}
    original_overlap = evaluate.overlap_t
    try:
        for number, group in enumerate(grouped.values(), 1):
            try:
                values = evaluate_expr(group[0]['expr'], panels.fields)
            except ExprError as exc:
                if '缺少字段' not in str(exc):
                    raise
                for factor in group:
                    missing[factor['horizon'], factor['id']] = deepcopy(factor)
            else:
                for factor in group:
                    h, fid = factor['horizon'], factor['id']
                    try:
                        evaluate.overlap_t = lambda s, h: evaluate.nw_t(s, h)
                        metrics = evaluate.evaluate_factor(values, *targets[h], panels.mask, splits[h], h, cfg)
                    finally:
                        evaluate.overlap_t = original_overlap
                    if metrics is not None:
                        old[h, fid] = dict(factor, **metrics)
                    metrics = evaluate.evaluate_factor(values, *targets[h], panels.mask, splits[h], h, cfg)
                    if metrics is not None:
                        new[h, fid] = dict(factor, **metrics)
            if number % 25 == 0 or number == len(grouped):
                print(f'重评估表达式 {number}/{len(grouped)}，旧口径 {len(old)}，新口径 {len(new)}，'
                      f'缺字段保留 {len(missing)}', flush=True)
    finally:
        evaluate.overlap_t = original_overlap
    return old, new, missing


def _transitions(keys, old, new):
    matrix = {status: dict.fromkeys(STATUSES, 0) for status in STATUSES}
    for key in keys:
        before = old[key]['status'] if key in old else 'dropped'
        after = new[key]['status'] if key in new else 'dropped'
        matrix[before][after] += 1
    return matrix


def _snapshot(factors, h):
    rows = [f for f in factors.values() if f['horizon'] == h]
    counts = Counter(f['status'] for f in rows)
    accepted = [dict(id=f['id'], train_t=f['train']['t'], val_t=f['val']['t'])
                for f in sorted(rows, key=lambda f: (-f['train']['t'], f['id']))
                if f['status'] == 'accepted']
    return dict(counts={s: counts[s] for s in STATUSES}, accepted=accepted)


def _markdown(report):
    lines = [f"# {report['market']} H1 zoo 重分类干跑", '',
             f"zoo 生成时间为 {report['zoo_generated_at']}。本轮纳入 {report['factor_count']} 条因子记录。", '',
             '| horizon | K | 状态 | 旧口径 | 新口径 |',
             '|---|---|---|---|---|']
    for h, row in report['horizons'].items():
        for status in STATUSES:
            lines.append(f"| {h} | {report['K'][h]} | {status} | {row['old']['counts'][status]} | "
                         f"{row['new']['counts'][status]} |")
    lines += ['', '## Accepted 因子', '', '| horizon | 口径 | id | train t | val t |',
              '|---|---|---|---|---|']
    for h, row in report['horizons'].items():
        for convention, label in (('old', '旧'), ('new', '新')):
            for factor in row[convention]['accepted']:
                lines.append(f"| {h} | {label} | {factor['id']} | {factor['train_t']:.6f} | "
                             f"{factor['val_t']:.6f} |")
    lines += ['', '## 入选因子', '', '| horizon | 旧 id | 新 id | 列表相同 |', '|---|---|---|---|']
    for h, row in report['horizons'].items():
        selection = row['ensemble']
        lines.append(f"| {h} | {', '.join(selection['old']) or '无'} | "
                     f"{', '.join(selection['new']) or '无'} | {'是' if selection['same'] else '否'} |")
    lines += ['', '## 状态转移', '', '| 旧状态 | 新状态 | 数量 |', '|---|---|---|']
    for before, changes in report['status_transitions'].items():
        for after, count in changes.items():
            if count:
                lines.append(f'| {before} | {after} | {count} |')
    sanity = report['old_status_sanity']
    ratio = f"{sanity['ratio']:.2%}" if sanity['ratio'] is not None else '无样本'
    lines += ['',
              f"缺字段保留 {report['missing_field_retained_count']} 条记录，涉及 "
              f"{report['missing_field_expression_count']} 个表达式。这些记录沿用旧快照，单独保留原状态和相关性信息。", '',
              f"旧口径状态与 zoo 原状态一致 {sanity['matches']}/{sanity['total']}，比例 {ratio}。", '',
              '## 输入 SHA256', '', '| 文件 | SHA256 |', '|---|---|']
    for name, digest in report['input_sha256'].items():
        lines.append(f'| {name} | {digest} |')
    return '\n'.join(lines) + '\n'


def run_dryrun(market, out, limit=None):
    root, out = Path(data_fetch.ROOT).resolve(), Path(out).resolve()
    if out.is_relative_to(root):
        raise ValueError('--out 必须位于数据根之外')
    if limit is not None and limit < 1:
        raise ValueError('--limit 必须为正整数')
    cfg = MARKETS[market]
    zoo_path, ledger_path = root / market / 'zoo.json', root / market / 'ledger.jsonl'
    hashes = {'zoo.json': _sha256(zoo_path), 'ledger.jsonl': _sha256(ledger_path),
              'evaluate.py': _sha256(Path(evaluate.__file__))}
    zoo = store.read_json(zoo_path)
    factors = zoo['factors'][:limit]
    original = {(f['horizon'], f['id']): f for f in factors}
    panels = load_panels(market, root)
    splits = {h: make_splits(panels.mask.index, h, panels.windows) for h in cfg['horizons']}
    targets = {h: forward_target(panels, h) for h in cfg['horizons']}
    K = Counter(row['horizon'] for row in store.read_ledger(market, root))
    print(f'{market} 已加载 {len(factors)} 条因子记录，K={dict(K)}', flush=True)
    old, new, missing = _reevaluate(factors, panels, targets, splits, cfg)
    old, new = deepcopy(old), deepcopy(new)
    print('正在按旧、新口径分别分类并计算相关性', flush=True)
    for refreshed in (old, new):
        Miner._classify_and_correlate(refreshed, K, panels, splits, cfg)
        # Missing fields have no ranks to correlate. Preserve their entire old
        # snapshot in both passes, including status, instead of inventing values.
        refreshed.update(deepcopy(missing))

    matches = sum(key in old and old[key]['status'] == f['status'] for key, f in original.items())
    report = dict(market=market, data_root=str(root), input_sha256=hashes,
                  zoo_generated_at=zoo.get('generated_at'), limit=limit, factor_count=len(original),
                  zoo_factor_count=len(zoo['factors']), K={str(h): K[h] for h in cfg['horizons']},
                  horizons={}, status_transitions=_transitions(original, old, new),
                  missing_field_retained_count=len(missing),
                  missing_field_expression_count=len({fid for _, fid in missing}),
                  missing_field_factors=[dict(horizon=h, id=fid) for h, fid in sorted(missing)],
                  old_status_sanity=dict(matches=matches, total=len(original),
                                         ratio=matches / len(original) if original else None))
    for h in cfg['horizons']:
        keys = [key for key in original if key[0] == h]
        before, after = _snapshot(old, h), _snapshot(new, h)
        before['counts']['dropped'] = sum(key not in old for key in keys)
        after['counts']['dropped'] = sum(key not in new for key in keys)
        old_ids = [f['id'] for f in live_scores.select_ensemble_factors(list(old.values()), h)]
        new_ids = [f['id'] for f in live_scores.select_ensemble_factors(list(new.values()), h)]
        old_accepted = {f['id'] for f in before['accepted']}
        new_accepted = {f['id'] for f in after['accepted']}
        report['horizons'][str(h)] = dict(
            old=before, new=after, status_transitions=_transitions(keys, old, new),
            accepted_changes=dict(added=sorted(new_accepted - old_accepted),
                                  removed=sorted(old_accepted - new_accepted)),
            ensemble=dict(old=old_ids, new=new_ids, same=old_ids == new_ids))
    out.mkdir(parents=True, exist_ok=True)
    store.write_json(out / f'{market}.json', report)
    store._replace(out / f'{market}.md', lambda stream: stream.write(_markdown(report)))
    print(f'报告已写入 {out / market}.json 和 {out / market}.md', flush=True)
    return report


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--market', required=True, choices=tuple(MARKETS))
    parser.add_argument('--out', required=True, type=Path)
    parser.add_argument('--limit', type=int)
    args = parser.parse_args(argv)
    try:
        run_dryrun(args.market, args.out, args.limit)
    except ValueError as exc:
        parser.error(str(exc))


if __name__ == '__main__':
    main()
