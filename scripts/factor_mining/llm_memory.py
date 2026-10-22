"""Cross-night LLM memory (opt-in via FM_LLM_MEMORY=1).

Stores only the TRAIN-only per-batch summary (summarize_outcomes output:
expr/horizon/train_t/train_ic/train_screen). Validation/holdout never enters.
"""
from pathlib import Path

from . import store


def _path(root, market):
    return Path(root) / market / 'llm_memory.jsonl'


def record_batch(root, market, run_id, batch, summary):
    store.append_jsonl(_path(root, market),
                       [dict(run_id=run_id, ts=store.timestamp(), batch=batch, items=list(summary))])


def history(root, market, current_run_id, nights=3):
    """The last `nights` distinct runs before the current one: top-5 expressions
    by train_t and the count of train-screen failures."""
    by_run = {}
    for row in store.read_jsonl(_path(root, market)):
        if row['run_id'] != current_run_id:
            by_run.setdefault(row['run_id'], []).append(row)
    result = []
    recent = list(by_run)[-nights:] if nights > 0 else []
    for run_id in recent:
        rows = by_run[run_id]
        best = {}
        fails = total = 0
        for row in rows:
            for item in row['items']:
                total += 1
                fails += str(item.get('train_screen', '')).startswith('fail')
                t = item.get('train_t')
                if t is not None and (item['expr'] not in best or t > best[item['expr']]['train_t']):
                    best[item['expr']] = item
        top = sorted(best.values(), key=lambda i: -i['train_t'])[:5]
        result.append(dict(night=rows[0]['ts'][:10], top=top, fails=fails, total=total))
    return result
