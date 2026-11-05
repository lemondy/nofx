"""Offline, restartable preregistered diverse-parent A/B study (no data fetch).

Run as ``python -m scripts.factor_mining.miner_ab_study {setup,run,report}``.
Mining always writes independent arm roots; setup/report can read snapshots.
"""
import argparse
from contextlib import contextmanager
import datetime as dt
import hashlib
import math
import os
from pathlib import Path
import shutil
import statistics
import time

from scripts.backtest import evolution_ledger
from . import options, store
from .config import MARKETS
from .diversity import diverse_select
from .expr import evaluate_expr
from .families import family_fine
from .miner import Miner
from .panel import load_panels, make_splits


PREREG = 'docs/research/2026-11-05-us-miner-diverse-parents-prereg.md'
EXPECTED_HASHES = {
    'aggregate': 'a262c75ee127a51deb79a51e001b38e0f87ca15083026bc32f42b4a64b9f358a',
    'zoo.json': '66f7daaa6d6b5fc1a39b5326de5ae9b266588388a4032efb4bfcbec312bc8af6',
    'ledger.jsonl': '383d77ca29ee64711155339af2ed9afd6122d17d8a97ce3bb87ad12498b281a9',
}
NIGHTS, MUTATIONS, BUDGET_MIN = 4, 1500, 180
FIRST_SEED = 20261106
FIRST_NIGHT = dt.datetime(2026, 11, 6, 19, 0, tzinfo=dt.timezone.utc)
BASELINE_VERSION = 'miner@37898817c6af'
IDEA_ID = 'us-miner-diverse-parents-v1'


def snapshot_hashes(snapshot):
    """Hash concatenated file contents in find/sort LC_ALL=C byte order.

    Paths determine order but are NOT themselves hashed. Symlinks are excluded,
    matching find's default ``-type f`` without ``-L``. Hash before excluding
    llm_work from the independent mining copies.
    """
    snapshot = Path(snapshot)
    base = snapshot / 'us'
    if not base.is_dir():
        raise ValueError(f'snapshot must contain us/: {snapshot}')
    files = sorted((p for p in base.rglob('*') if p.is_file() and not p.is_symlink()),
                   key=lambda p: os.fsencode(p.relative_to(snapshot).as_posix()))
    digest = hashlib.sha256()
    for path in files:
        with path.open('rb') as stream:
            for block in iter(lambda: stream.read(1024 * 1024), b''):
                digest.update(block)
    return dict(aggregate=digest.hexdigest(), **{
        name: hashlib.sha256((base / name).read_bytes()).hexdigest()
        for name in ('zoo.json', 'ledger.jsonl')})


def setup(snapshot, work, *, allow_input_drift=False):
    snapshot, work = Path(snapshot).resolve(), Path(work).resolve()
    # Refuse both arms before writing either; never merge into an old study.
    if any(os.path.lexists(work / arm) for arm in ('B', 'D')) or (work / 'ab_setup.json').exists():
        raise FileExistsError(f'A/B work directories already exist: {work}')
    if work == snapshot or snapshot in work.parents or work in snapshot.parents:
        raise ValueError('snapshot and work must be separate, non-overlapping directories')
    hashes = snapshot_hashes(snapshot)
    drift = {name: dict(expected=EXPECTED_HASHES[name], actual=value)
             for name, value in hashes.items() if value != EXPECTED_HASHES[name]}
    if drift and not allow_input_drift:
        raise ValueError(f'snapshot hash mismatch (use --allow-input-drift to override): {drift}')
    for arm in ('B', 'D'):
        shutil.copytree(snapshot / 'us', work / arm / 'us',
                        ignore=shutil.ignore_patterns('llm_work'))
    manifest = dict(snapshot=str(snapshot), snapshot_hashes=hashes, input_drift=drift,
                    allow_input_drift=allow_input_drift, prereg=PREREG)
    store.write_json(work / 'ab_setup.json', manifest)
    return manifest


def night_identity(index):
    now = FIRST_NIGHT + dt.timedelta(days=index)
    seed = FIRST_SEED + index
    return now, seed, f'us-{now.strftime("%Y%m%dT%H%M%S%fZ")}-{seed}'


@contextmanager
def arm_environment(arm):
    """Clear every declared FM option each night, restoring the caller afterward."""
    names = {value for value in vars(options).values()
             if isinstance(value, str) and value.startswith('FM_')}
    previous = {name: os.environ.get(name) for name in names}
    try:
        for name in names:
            os.environ.pop(name, None)
        if arm == 'D':
            os.environ[options.DIVERSE_PARENTS] = '1'
        yield
    finally:
        for name, value in previous.items():
            if value is None:
                os.environ.pop(name, None)
            else:
                os.environ[name] = value


def completed(row):
    return bool(row.get('run_id')) and not row.get('error') and not row.get('budget_exhausted')


def run(work, arm, *, nights=NIGHTS, mutations=MUTATIONS, budget_min=BUDGET_MIN):
    if arm not in ('B', 'D') or not 1 <= nights <= NIGHTS or mutations < 0 or budget_min <= 0:
        raise ValueError('require arm B/D, 1..4 nights, nonnegative mutations and positive budget')
    root = Path(work) / arm
    if not (root / 'us').is_dir():
        raise ValueError('run setup before mining')
    progress_path = root / 'ab_progress.jsonl'
    parameters = dict(nights=nights, mutations=mutations, budget_min=budget_min)
    # A changed search budget on resume would silently change the experiment.
    parameter_path = root / 'ab_parameters.json'
    with store.FileLock(root):
        previous = store.read_json(parameter_path)
        if previous is not None and previous != parameters:
            raise ValueError('run parameters differ from the existing arm; use a fresh work directory')
        store.write_json(parameter_path, parameters)
        progress = store.read_jsonl(progress_path)
        for index in range(nights):
            night = index + 1
            rows = [r for r in progress if r['night'] == night]
            if any(not completed(r) for r in rows):
                raise ValueError(f'night {night} previously failed; preserve this invalid study and use a fresh work directory')
            if rows:
                continue
            now, seed, run_id = night_identity(index)
            saved = next((r for r in reversed(store.read_jsonl(root / 'runs.jsonl'))
                          if r['run_id'] == run_id), None)
            wall_start = time.monotonic()
            error = None
            try:
                # A durable run row can precede a crash while appending progress.
                if saved is None:
                    if any(r.get('run_id') == run_id for r in store.read_ledger('us', root)):
                        raise RuntimeError('interrupted night has trials but no final run row; use a fresh work directory')
                    with arm_environment(arm):
                        Miner(root=root).run_market(
                            'us', n_mutations=mutations, n_llm=0, budget_s=budget_min * 60,
                            seed=seed, use_llm=False, now=now)
            except BaseException as exc:
                error = f'{type(exc).__name__}: {exc}'
                raise
            finally:
                rows = store.read_jsonl(root / 'runs.jsonl')
                result = next((r for r in reversed(rows) if r['run_id'] == run_id), {})
                entry = dict(night=night, seed=seed, run_id=run_id,
                             seconds=saved['seconds'] if saved else round(time.monotonic() - wall_start, 6),
                             budget_exhausted=result.get('budget_exhausted', False),
                             error=error or result.get('error') or (None if result else 'missing run row'))
                store.append_jsonl(progress_path, [entry])
                progress.append(entry)
            if not completed(entry):
                break
    return progress


def ab_run_ids(progress, runs):
    """Include progress IDs and fixed A/B runs whose progress append was lost."""
    expected = {night_identity(i)[2] for i in range(NIGHTS)}
    return ({r['run_id'] for r in progress if r.get('run_id')} |
            {r['run_id'] for r in runs if r.get('run_id') in expected})


def new_factors(factors, ledger, run_ids):
    keys = {(r['horizon'], r['id']) for r in ledger if r.get('run_id') in run_ids}
    return [f for f in factors if (f['horizon'], f['id']) in keys]


def verdict(arms):
    if any(a['completed_nights'] < NIGHTS or a['budget_exhausted'] or a['errors']
           for a in arms.values()):
        return 'invalid'
    return 'screen_pass' if arms['D']['M1'] >= 1 and arms['D']['M1'] > arms['B']['M1'] else 'fail'


def arm_report(root):
    root = Path(root)
    progress = store.read_jsonl(root / 'ab_progress.jsonl')
    runs = store.read_jsonl(root / 'runs.jsonl')
    run_ids = ab_run_ids(progress, runs)
    runs = [r for r in runs if r['run_id'] in run_ids]
    # Reconcile missing progress from durable runs for read-only reporting too.
    nights = list(progress)
    for i in range(NIGHTS):
        _, seed, run_id = night_identity(i)
        if not any(p.get('run_id') == run_id for p in nights):
            for row in runs:
                if row['run_id'] == run_id:
                    nights.append(dict(night=i + 1, seed=seed, run_id=run_id,
                                       seconds=row['seconds'], error=row.get('error'),
                                       budget_exhausted=row.get('budget_exhausted', False)))
    factors = store.read_json(root / 'us/zoo.json', {'factors': []})['factors']
    ledger = store.read_ledger('us', root)
    fresh = new_factors(factors, ledger, run_ids)
    accepted = [f for f in fresh if f['status'] == 'accepted']
    non_rejected = [f for f in fresh if f['status'] != 'rejected']
    # All non-rejected factors survive the miner's rejected-only zoo pruning.
    panels = load_panels('us', root)
    split = make_splits(panels.mask.index, min(MARKETS['us']['horizons']), panels.windows)
    dates = panels.mask.index[split['train']][::5]
    clusters = {}
    for h in MARKETS['us']['horizons']:
        ranked = sorted((f for f in non_rejected if f['horizon'] == h),
                        key=lambda f: (-f['train']['t'], f['id']))

        def rank_of(f):
            values = evaluate_expr(f['expr'], panels.fields).loc[dates] * f['direction']
            return values.where(panels.mask.loc[dates]).rank(axis=1, method='average')

        # Clustering has no family cap; each greedy representative is one cluster.
        _, stats = diverse_select(ranked, 300, rank_of, dates, per_family=301)
        clusters[str(h)] = dict(n_top=stats['n_scanned'], n_clusters=stats['n_selected'])
    val_ics = [f['val']['ic'] for f in non_rejected
               if f.get('val', {}).get('ic') is not None and math.isfinite(f['val']['ic'])]
    return dict(
        M1=sum(f.get('forward', {}).get('verdict') == 'confirmed' for f in accepted),
        M2=len(accepted),
        completed_nights=len({r['night'] for r in nights
                              if 1 <= r['night'] <= NIGHTS and completed(r)}),
        budget_exhausted=any(r.get('budget_exhausted') for r in nights + runs),
        errors=list(dict.fromkeys(r['error'] for r in nights + runs if r.get('error'))),
        run_ids=sorted(run_ids),
        diagnostics=dict(new_non_rejected=len(non_rejected), clusters=clusters,
                         n_families=len({family_fine(f['expr']) for f in non_rejected}),
                         median_val_ic=statistics.median(val_ics) if val_ics else None,
                         parent_pool_stats=[dict(run_id=r['run_id'], stats=r.get('diverse_parents'))
                                            for r in runs],
                         per_night_seconds=[dict(night=r['night'], seconds=r['seconds'])
                                            for r in sorted(nights, key=lambda r: r['night'])]))


def render_report(result):
    lines = ['# 美股因子挖掘：多样化父代 A/B', '',
             '| 组别 | 完成晚数 | M1：新增接受且前向确认 | M2：新增接受 | 新增训练通过 | 族数 | 验证 IC 中位数 |',
             '|---|---:|---:|---:|---:|---:|---:|']
    for arm, a in result['arms'].items():
        d = a['diagnostics']
        median = '无' if d['median_val_ic'] is None else f"{d['median_val_ic']:.6f}"
        lines.append(f"| {arm} | {a['completed_nights']} | {a['M1']} | {a['M2']} | "
                     f"{d['new_non_rejected']} | {d['n_families']} | {median} |")
    lines += ['', f"结论：**{result['verdict']}**", '', f'预注册：`{PREREG}`。',
              'M1、M2 只计入账本 run_id 属于本次 A/B 且保留在最终因子库的（持有期，因子 ID）。',
              '少于四晚、任一报错或预算耗尽均判为 invalid。LLM 改动未在本次 A/B 中检验。']
    for arm, a in result['arms'].items():
        d = a['diagnostics']
        lines += ['', f'## {arm} 组诊断', '',
                  '训练段前 300 个新增非拒绝因子的贪心簇数（绝对相关阈值 0.8）：', '']
        lines += [f"- 持有期 {h}：{s['n_clusters']} 簇，扫描 {s['n_top']} 个。"
                  for h, s in d['clusters'].items()]
        lines += ['', '每晚耗时：', '']
        lines += [f"- 第 {r['night']} 晚：{r['seconds']:.3f} 秒。" for r in d['per_night_seconds']]
        lines += ['', '父代选择统计（来自运行记录）：', '']
        for row in d['parent_pool_stats']:
            stats = row['stats']
            if stats is None:
                lines.append('- 开关关闭，运行记录不含父代统计。')
            else:
                for h, s in stats['per_horizon'].items():
                    lines.append(f"- {row['run_id']}，持有期 {h}：选择 {s['n_selected']}，"
                                 f"族数 {s['n_families']}，扫描 {s['n_scanned']}，"
                                 f"相关拒绝 {s['n_rejected_corr']}，族配额拒绝 {s['n_rejected_family']}。")
        if a['errors']:
            lines += ['', f"运行错误：{'；'.join(a['errors'])}"]
        if a['budget_exhausted']:
            lines += ['', '存在预算耗尽记录。']
    lines += ['', '## 输入哈希', '']
    lines += [f'- {name}：`{digest}`。' for name, digest in result['snapshot_hashes'].items()]
    lines += ['', '## 偏离与解释', '']
    lines += [f'- {deviation}' for deviation in result['deviations']] or ['- 无。']
    return '\n'.join(lines) + '\n'


def report(work, out, *, ledger=evolution_ledger.DEFAULT_PATH, snapshot=None):
    work, out = Path(work), Path(out)
    manifest = store.read_json(work / 'ab_setup.json', {})
    hashes = snapshot_hashes(snapshot) if snapshot is not None else manifest.get('snapshot_hashes', {})
    deviations = [
        '族配额拒绝优先计数；同时满足两种拒绝条件时只计入族配额拒绝。',
        '族数与验证 IC 中位数使用最终新增非拒绝因子；聚类不施加族配额。',
        '基线关闭时保持运行记录字段不变，因此基线父代池统计为空；D 组选择数即所选父代的贪心簇数。',
        '失败夜不在原目录重试；已有运行尾记录可恢复进度，只有部分试验账本且无运行尾记录时视为错误，须新建实验。',
    ]
    if manifest.get('input_drift'):
        deviations.append('设置阶段已显式允许输入哈希偏离预注册，实际哈希见输入记录。')
    if not hashes:
        deviations.append('未提供快照且缺少设置清单，无法提供输入哈希。')
    if snapshot is not None and hashes != manifest.get('snapshot_hashes', hashes):
        deviations.append('报告时提供的快照哈希与设置阶段不同；两者均记录在 JSON。')
    parameters = {arm: store.read_json(work / arm / 'ab_parameters.json') for arm in ('B', 'D')}
    for arm, p in parameters.items():
        if p and p != dict(nights=NIGHTS, mutations=MUTATIONS, budget_min=BUDGET_MIN):
            deviations.append(f"{arm} 组使用非预注册运行参数：晚数 {p['nights']}，变异 {p['mutations']}，预算 {p['budget_min']} 分钟。")
    arms = {arm: arm_report(work / arm) for arm in ('B', 'D')}
    result = dict(prereg=PREREG, baseline_version=BASELINE_VERSION, idea_id=IDEA_ID,
                  arms=arms, verdict=verdict(arms), snapshot_hashes=hashes,
                  setup=manifest, parameters=parameters, deviations=deviations)
    if ledger is not None:
        result['ledger_row'] = evolution_ledger.append_event(
            ledger, 'fq_us', lambda k, previous: dict(
                stage='S1', change_type='E', baseline_version=BASELINE_VERSION, idea_id=IDEA_ID,
                verdict=result['verdict'], arms=arms, snapshot_hashes=hashes,
                prereg=PREREG, deviations=deviations))
    out.mkdir(parents=True, exist_ok=True)
    store.write_json(out / 'report.json', result)
    (out / 'report.md').write_text(render_report(result), encoding='utf-8')
    return result


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    sub = parser.add_subparsers(dest='command', required=True)
    p = sub.add_parser('setup')
    p.add_argument('--snapshot', type=Path, required=True)
    p.add_argument('--work', type=Path, required=True)
    p.add_argument('--allow-input-drift', action='store_true')
    p = sub.add_parser('run')
    p.add_argument('--work', type=Path, required=True)
    p.add_argument('--arm', choices=('B', 'D'), required=True)
    p.add_argument('--nights', type=int, default=NIGHTS)
    p.add_argument('--mutations', type=int, default=MUTATIONS)
    p.add_argument('--budget-min', type=float, default=BUDGET_MIN)
    p = sub.add_parser('report')
    p.add_argument('--work', type=Path, required=True)
    p.add_argument('--out', type=Path, required=True)
    p.add_argument('--snapshot', type=Path)
    group = p.add_mutually_exclusive_group()
    group.add_argument('--ledger', type=Path, default=Path(evolution_ledger.DEFAULT_PATH))
    group.add_argument('--no-ledger', action='store_true')
    args = parser.parse_args(argv)
    if args.command == 'setup':
        setup(args.snapshot, args.work, allow_input_drift=args.allow_input_drift)
    elif args.command == 'run':
        run(args.work, args.arm, nights=args.nights, mutations=args.mutations, budget_min=args.budget_min)
    else:
        result = report(args.work, args.out, ledger=None if args.no_ledger else args.ledger,
                        snapshot=args.snapshot)
        print(f"结论：{result['verdict']}；报告：{args.out / 'report.md'}")
    return 0


if __name__ == '__main__':
    raise SystemExit(main())
