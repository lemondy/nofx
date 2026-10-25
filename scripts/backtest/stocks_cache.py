"""Offline factor-mining Yahoo daily rows -> validated stocks cache (no HTTP)."""
import argparse
import hashlib
import json
import math
from pathlib import Path
import re

from .stocks_data import History

BASE = Path('/private/tmp/claude-501/uscand')
SOURCE = Path('/Users/zhangyun/workspace/nofx/data/factor_mining/us/daily')
PROTECTED = Path('/Users/zhangyun/workspace/nofx/data').resolve()


def output_root(path):
    root = Path(path).resolve()
    if not root.is_relative_to(BASE.resolve()):
        raise ValueError('all study outputs must be under '+str(BASE))
    return root


def convert_rows(rows, symbol):
    """Factor rows: date, adjusted O/H/L/C, volume, vwap; History validates bars."""
    raw = []
    for row in rows if isinstance(rows, list) else []:
        try:
            if (not isinstance(row, list) or len(row) != 7 or
                    not isinstance(row[0], str) or
                    not re.fullmatch(r'\d{4}-\d{2}-\d{2}', row[0]) or
                    not math.isfinite(float(row[6])) or float(row[6]) <= 0):
                continue
            raw.append(dict(open_time=row[0]+'T00:00:00Z',
                            **dict(zip(('open', 'high', 'low', 'close', 'volume'), row[1:6]))))
        except (ValueError, TypeError, OverflowError):
            continue
    h = History(raw)
    bars = [dict(open_time=k.strftime('%Y-%m-%dT00:00:00Z'), **b)
            for k, b in zip(h.keys, h.bars)]
    return dict(series=dict(symbol=symbol+'BUSDT', timeframe='1d', bars=bars,
                            source='yahoo(factor_mining)'))


def rebuild(source=SOURCE, cache=BASE/'cache', tf='1d'):
    if tf != '1d':
        raise ValueError('only --tf 1d is supported')
    source, cache = Path(source).resolve(), output_root(cache)
    if cache == source or cache.is_relative_to(PROTECTED):
        raise ValueError('source data is read-only')
    files = sorted(source.glob('*.json'))
    if not files or not (source/'SPY.json').is_file():
        raise ValueError('source must contain stock JSON files and SPY.json')
    cache.mkdir(parents=True, exist_ok=True)
    if any(p.name not in {f.stem+'BUSDT.json' for f in files}
           for p in cache.glob('*BUSDT.json')):
        raise ValueError('stale cache symbols: use an empty cache directory')
    manifest = dict(source=str(source), timeframe=tf, symbols={})
    for path in files:
        if not re.fullmatch('[A-Z]+', path.stem):
            raise ValueError('unexpected symbol: '+path.stem)
        content = path.read_bytes()
        rows = json.loads(content)
        doc = convert_rows(rows, path.stem)
        bars = doc['series']['bars']
        if not bars:
            raise ValueError('no valid bars: '+path.name)
        target = cache/(path.stem+'BUSDT.json')
        if target.resolve().is_relative_to(PROTECTED):
            raise ValueError('refusing data-directory symlink')
        target.write_text(json.dumps(doc, allow_nan=False, separators=(',', ':')))
        manifest['symbols'][path.stem] = dict(sha256=hashlib.sha256(content).hexdigest(),
            input_rows=len(rows), bars=len(bars), removed=len(rows)-len(bars),
            first=bars[0]['open_time'], last=bars[-1]['open_time'])
    (cache/'manifest.json').write_text(json.dumps(manifest, indent=2))
    return manifest


def main(argv=None):
    p = argparse.ArgumentParser(description=__doc__)
    p.add_argument('--source', default=str(SOURCE))
    p.add_argument('--cache', default=str(BASE/'cache'))
    p.add_argument('--tf', choices=['1d'], default='1d')
    a = p.parse_args(argv)
    manifest = rebuild(a.source, a.cache, a.tf)
    print(f"converted {len(manifest['symbols'])} symbols to {a.cache}")


if __name__ == '__main__':
    main()
