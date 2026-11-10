"""增量评估账本（docs/architecture/INCREMENTAL_EVAL_DESIGN_2026-11-03.md「记账」）。

Append-only JSONL shared by every stage (S1 offline screen, S2 online pair,
S3 merge/rollback). K is per track: the number of S1 screens that were not
`invalid`, including the one being appended.
"""
import fcntl
import json
import math
import os
from datetime import datetime, timezone
from pathlib import Path

DEFAULT_PATH = 'data/evolution/ledger.jsonl'
TRACKS = ('crypto_llm', 'us_llm', 'fq_crypto', 'fq_us', 'crypto_carry')
CHANGE_TYPES = ('A', 'B', 'C', 'D', 'E')
STAGES = ('S1', 'S2', 'S3')
REQUIRED = ('track', 'baseline_version', 'change_type', 'stage', 'idea_id', 'verdict')


def json_safe(value):
    if isinstance(value, float) and not math.isfinite(value):
        return 'inf' if value > 0 else '-inf' if value < 0 else None
    if isinstance(value, dict):
        return {k: json_safe(v) for k, v in value.items()}
    if isinstance(value, (list, tuple)):
        return [json_safe(v) for v in value]
    return value


def counts_toward_k(row):
    return row.get('stage') == 'S1' and row.get('verdict') != 'invalid'


def next_k(rows, track):
    return sum(r.get('track') == track and counts_toward_k(r) for r in rows) + 1


def read_rows(path):
    path = Path(path)
    if not path.exists():
        return []
    return [json.loads(line) for line in path.read_text(encoding='utf-8').splitlines() if line.strip()]


def validate(row):
    missing = [f for f in REQUIRED if row.get(f) in (None, '')]
    if missing:
        raise ValueError(f'账本行缺少字段：{",".join(missing)}')
    if row['track'] not in TRACKS:
        raise ValueError(f'track 必须是 {"/".join(TRACKS)}')
    if row['change_type'] not in CHANGE_TYPES:
        raise ValueError(f'change_type 必须是 {"/".join(CHANGE_TYPES)}')
    if row['stage'] not in STAGES:
        raise ValueError(f'stage 必须是 {"/".join(STAGES)}')


def append_event(path, track, build):
    """Lock, compute K for `track`, call build(k, previous_rows) -> row, append.

    The K calculation and the O_APPEND write share one flock so concurrent
    scorers cannot reuse a K. Existing bytes are never rewritten.
    """
    if track not in TRACKS:
        raise ValueError(f'track 必须是 {"/".join(TRACKS)}')
    path = Path(path)
    path.parent.mkdir(parents=True, exist_ok=True)
    fd = os.open(path, os.O_RDWR | os.O_CREAT | os.O_APPEND, 0o600)
    try:
        fcntl.flock(fd, fcntl.LOCK_EX)
        with os.fdopen(os.dup(fd), 'r', encoding='utf-8') as f:
            old = f.read()
        if old and not old.endswith('\n'):
            raise ValueError('账本末行不完整，拒绝追加或修改已有字节')
        previous = [json.loads(line) for line in old.splitlines() if line.strip()]
        k = next_k(previous, track)
        row = dict(build(k, previous))
        row['track'] = track
        row.setdefault('recorded_at', datetime.now(timezone.utc).isoformat())
        validate(row)
        row['K'] = k if counts_toward_k(row) else None
        row['counts_toward_k'] = counts_toward_k(row)
        body = (json.dumps(json_safe(row), ensure_ascii=False, allow_nan=False,
                           separators=(',', ':')) + '\n').encode()
        if os.write(fd, body) != len(body):
            raise OSError('账本追加不完整')
        os.fsync(fd)
        return row
    finally:
        os.close(fd)
