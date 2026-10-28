"""Atomic JSON replacement, durable JSONL append and a nonblocking process lock."""
import datetime as dt
import fcntl
import json
import logging
import math
import os
from pathlib import Path
import tempfile

import numpy as np

from .data_fetch import ROOT


class AlreadyRunning(RuntimeError):
    pass


class FileLock:
    def __init__(self, root=ROOT):
        self.path = Path(root) / '.lock'
        self.stream = None

    def __enter__(self):
        self.path.parent.mkdir(parents=True, exist_ok=True)
        self.stream = self.path.open('a')
        try:
            fcntl.flock(self.stream, fcntl.LOCK_EX | fcntl.LOCK_NB)
        except BlockingIOError as exc:
            self.stream.close()
            self.stream = None
            raise AlreadyRunning(f'factor mining already running: {self.path}') from exc
        return self

    def __exit__(self, *exc):
        if self.stream is not None:
            fcntl.flock(self.stream, fcntl.LOCK_UN)
            self.stream.close()
            self.stream = None


def timestamp():
    return dt.datetime.now(dt.timezone.utc).isoformat(timespec='seconds')


def finite(value):
    if isinstance(value, dict):
        return {str(k): finite(v) for k, v in value.items()}
    if isinstance(value, (list, tuple)):
        return [finite(v) for v in value]
    if isinstance(value, np.generic):
        value = value.item()
    if isinstance(value, float) and not math.isfinite(value):
        return None
    return value


def read_json(path, default=None):
    try:
        return json.loads(Path(path).read_text(encoding='utf-8'))
    except FileNotFoundError:
        return default


def _replace(path, write):
    path = Path(path)
    path.parent.mkdir(parents=True, exist_ok=True)
    fd, name = tempfile.mkstemp(prefix='.' + path.name + '.', dir=path.parent)
    try:
        with os.fdopen(fd, 'w', encoding='utf-8') as stream:
            write(stream)
            stream.flush()
            os.fsync(stream.fileno())
        os.replace(name, path)
    finally:
        if os.path.exists(name):
            os.unlink(name)


def write_json(path, obj):
    _replace(path, lambda f: json.dump(finite(obj), f, allow_nan=False, ensure_ascii=False,
                                      separators=(',', ':')))


def read_jsonl(path):
    path = Path(path)
    if not path.exists():
        return []
    rows = []
    with path.open('rb') as stream:
        for number, line in enumerate(stream, 1):
            if not line.strip():
                continue
            try:
                rows.append(json.loads(line))
            except (ValueError, UnicodeDecodeError):
                # A crash can leave an incomplete tail (even half a UTF-8
                # character). Later appends leave that damaged row in place.
                logging.getLogger(__name__).warning('Skipping invalid JSONL row %s:%d (possible torn append)', path, number)
    return rows


def append_jsonl(path, rows):
    path = Path(path)
    rows = list(rows)
    if not rows:
        return
    path.parent.mkdir(parents=True, exist_ok=True)
    # Do not join the next valid record onto an incomplete pre-crash tail.
    needs_newline = False
    if path.exists() and path.stat().st_size:
        with path.open('rb') as old:
            old.seek(-1, os.SEEK_END)
            needs_newline = old.read(1) != b'\n'
    with path.open('a', encoding='utf-8') as stream:
        if needs_newline:
            stream.write('\n')
        for row in rows:
            stream.write(json.dumps(finite(row), allow_nan=False, ensure_ascii=False,
                                    separators=(',', ':')) + '\n')
        stream.flush()
        os.fsync(stream.fileno())


def read_ledger(market, root=ROOT):
    return read_jsonl(Path(root) / market / 'ledger.jsonl')


def append_ledger(market, rows, root=ROOT):
    append_jsonl(Path(root) / market / 'ledger.jsonl', rows)


def write_zoo(market, zoo, root=ROOT):
    write_json(Path(root) / market / 'zoo.json', zoo)


def write_ensemble(market, ensemble, root=ROOT):
    write_json(Path(root) / market / 'ensemble.json', ensemble)


def write_status(patch, root=ROOT):
    path = Path(root) / 'status.json'
    value = read_json(path, dict(updated_at=None, running=False, current=None,
                                 schedule='每天 02:30', markets={}))
    def merge(base, updates):
        for key, item in updates.items():
            if isinstance(item, dict) and isinstance(base.get(key), dict):
                merge(base[key], item)
            else:
                base[key] = item
    merge(value, patch)
    value['updated_at'] = timestamp()
    write_json(path, value)


def append_run(row, root=ROOT):
    append_jsonl(Path(root) / 'runs.jsonl', [row])
