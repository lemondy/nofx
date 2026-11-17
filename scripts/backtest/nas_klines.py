"""Read-only LAN NAS MariaDB/MySQL client for crypto & US K-line data.

Connects via pure Python stdlib (mysql_native_password over TCP, no external
driver required) to the Synology MariaDB instance storing:
  - ``nofx.crypto_klines``: Binance USDT-M klines (1h / 4h / 1d, 2020-01+,
    including quote_volume, count, taker_buy_volume, taker_buy_quote_volume)
  - ``nofx.us_daily``: Yahoo split/dividend-adjusted US daily bars (1980+,
    plus dividends and stock_splits)

Configuration precedence:
  1. Explicit ``Config`` / kwargs
  2. Environment variables ``NAS_MYSQL_HOST``, ``NAS_MYSQL_PORT``,
     ``NAS_MYSQL_USER``, ``NAS_MYSQL_PASSWORD``, ``NAS_MYSQL_DB``
  3. Repo ``.env`` file (for those same keys)

Usage:
    python -m scripts.backtest.nas_klines check
    python -m scripts.backtest.nas_klines crypto BTCUSDT --interval 1h --start 2026-11-01 --limit 5
    python -m scripts.backtest.nas_klines us AAPL --start 2026-11-01 --limit 5
"""
from __future__ import annotations

import argparse
from dataclasses import dataclass
import datetime as dt
import hashlib
import json
import math
import os
from pathlib import Path
import re
import socket
import struct
import sys
import time
from typing import Iterable

REPO = Path(__file__).resolve().parents[2]
DAY_MS = 86_400_000
INTERVAL_MS = {
    '1m': 60_000,
    '3m': 180_000,
    '5m': 300_000,
    '15m': 900_000,
    '30m': 1_800_000,
    '1h': 3_600_000,
    '2h': 7_200_000,
    '4h': 14_400_000,
    '6h': 21_600_000,
    '8h': 28_800_000,
    '12h': 43_200_000,
    '1d': 86_400_000,
}

_IDENT_RE = re.compile(r'^[A-Za-z0-9._/-]{1,32}$')
_DATE_RE = re.compile(r'^\d{4}-\d{2}-\d{2}$')


@dataclass(frozen=True)
class NasMysqlConfig:
    host: str = '192.168.50.244'
    port: int = 3307
    user: str = 'nofx'
    password: str = ''
    database: str = 'nofx'
    timeout: float = 15.0


def _read_dotenv(path: Path) -> dict[str, str]:
    out: dict[str, str] = {}
    try:
        lines = path.read_text(encoding='utf-8').splitlines()
    except OSError:
        return out
    for raw in lines:
        line = raw.strip()
        if not line or line.startswith('#') or '=' not in line:
            continue
        key, val = line.split('=', 1)
        key = key.strip()
        val = val.strip()
        if len(val) >= 2 and val[0] == val[-1] and val[0] in ("'", '"'):
            val = val[1:-1]
        if key:
            out[key] = val
    return out


def load_config(
    *,
    host: str | None = None,
    port: int | None = None,
    user: str | None = None,
    password: str | None = None,
    database: str | None = None,
    timeout: float = 15.0,
    env_path: Path | None = None,
) -> NasMysqlConfig:
    dotenv = _read_dotenv(env_path if env_path is not None else (REPO / '.env'))

    def pick(env_key: str, default: str) -> str:
        if env_key in os.environ and os.environ[env_key] != '':
            return os.environ[env_key]
        if env_key in dotenv and dotenv[env_key] != '':
            return dotenv[env_key]
        return default

    resolved_host = host or pick('NAS_MYSQL_HOST', '192.168.50.244')
    resolved_port = int(port if port is not None else pick('NAS_MYSQL_PORT', '3307'))
    resolved_user = user or pick('NAS_MYSQL_USER', 'nofx')
    resolved_pass = password if password is not None else pick('NAS_MYSQL_PASSWORD', '')
    resolved_db = database or pick('NAS_MYSQL_DB', 'nofx')
    return NasMysqlConfig(
        host=resolved_host,
        port=resolved_port,
        user=resolved_user,
        password=resolved_pass,
        database=resolved_db,
        timeout=timeout,
    )


def _quote_str(value: str) -> str:
    escaped = (
        value.replace('\\', '\\\\')
        .replace('\x00', '\\0')
        .replace('\n', '\\n')
        .replace('\r', '\\r')
        .replace("'", "\\'")
        .replace('"', '\\"')
        .replace('\x1a', '\\Z')
    )
    return f"'{escaped}'"


def _validate_ident(value: str, label: str) -> str:
    if not isinstance(value, str) or not _IDENT_RE.fullmatch(value):
        raise ValueError(f'Invalid {label}: {value!r}')
    return value


def _to_ms(value: int | float | dt.date | dt.datetime | str | None) -> int | None:
    if value is None:
        return None
    if isinstance(value, (int, float)) and not isinstance(value, bool):
        return int(value)
    if isinstance(value, dt.datetime):
        t = value if value.tzinfo else value.replace(tzinfo=dt.timezone.utc)
        return int(t.timestamp() * 1000)
    if isinstance(value, dt.date):
        t = dt.datetime(value.year, value.month, value.day, tzinfo=dt.timezone.utc)
        return int(t.timestamp() * 1000)
    if isinstance(value, str):
        s = value.strip()
        if _DATE_RE.fullmatch(s):
            d = dt.date.fromisoformat(s)
            return int(dt.datetime(d.year, d.month, d.day, tzinfo=dt.timezone.utc).timestamp() * 1000)
        t = dt.datetime.fromisoformat(s.replace('Z', '+00:00'))
        if t.tzinfo is None:
            t = t.replace(tzinfo=dt.timezone.utc)
        return int(t.timestamp() * 1000)
    raise TypeError(f'Unsupported timestamp: {value!r}')


def _to_date_str(value: dt.date | dt.datetime | str | None) -> str | None:
    if value is None:
        return None
    if isinstance(value, dt.datetime):
        return value.date().isoformat()
    if isinstance(value, dt.date):
        return value.isoformat()
    if isinstance(value, str):
        s = value.strip()[:10]
        if not _DATE_RE.fullmatch(s):
            raise ValueError(f'Invalid date string: {value!r}')
        return s
    raise TypeError(f'Unsupported date: {value!r}')


def _ms_to_utc_date(ms: int) -> str:
    return dt.datetime.fromtimestamp(ms / 1000, dt.timezone.utc).strftime('%Y-%m-%d')


def _fnum(val: str | None) -> float | None:
    if val is None:
        return None
    v = float(val)
    return v if math.isfinite(v) else None


def _recv_exact(sock: socket.socket, n: int) -> bytes:
    buf = bytearray()
    while len(buf) < n:
        chunk = sock.recv(n - len(buf))
        if not chunk:
            raise ConnectionError('Unexpected EOF from MySQL server')
        buf.extend(chunk)
    return bytes(buf)


def _recv_pkt(sock: socket.socket) -> tuple[int, bytes]:
    hdr = _recv_exact(sock, 4)
    length = int.from_bytes(hdr[:3], 'little')
    seq = hdr[3]
    if length == 0:
        return seq, b''
    body = _recv_exact(sock, length)
    while length == 0xFFFFFF:
        hdr2 = _recv_exact(sock, 4)
        length = int.from_bytes(hdr2[:3], 'little')
        seq = hdr2[3]
        if length:
            body += _recv_exact(sock, length)
    return seq, body


def _send_pkt(sock: socket.socket, seq: int, payload: bytes) -> None:
    sock.sendall(len(payload).to_bytes(3, 'little') + bytes([seq & 0xFF]) + payload)


def _read_lenenc_int(buf: bytes, off: int) -> tuple[int, int]:
    b = buf[off]
    if b < 0xFB:
        return b, off + 1
    if b == 0xFC:
        return int.from_bytes(buf[off + 1:off + 3], 'little'), off + 3
    if b == 0xFD:
        return int.from_bytes(buf[off + 1:off + 4], 'little'), off + 4
    if b == 0xFE:
        return int.from_bytes(buf[off + 1:off + 9], 'little'), off + 9
    raise ValueError(f'Unexpected lenenc prefix: {b:#x}')


def _read_lenenc_str(buf: bytes, off: int) -> tuple[str | None, int]:
    if buf[off] == 0xFB:
        return None, off + 1
    n, off = _read_lenenc_int(buf, off)
    return buf[off:off + n].decode('utf-8', 'replace'), off + n


def _native_auth_token(password: str, scramble: bytes) -> bytes:
    if not password:
        return b''
    pw = password.encode('utf-8')
    s1 = hashlib.sha1(pw).digest()
    s2 = hashlib.sha1(s1).digest()
    s3 = hashlib.sha1(scramble + s2).digest()
    return bytes(a ^ b for a, b in zip(s1, s3))


class ReadOnlyMysqlClient:
    """Minimal read-only MySQL/MariaDB client over TCP using mysql_native_password."""

    def __init__(self, config: NasMysqlConfig | None = None):
        self.config = config or load_config()
        if not self.config.password:
            raise RuntimeError(
                'NAS MySQL password missing; set NAS_MYSQL_PASSWORD in environment or .env'
            )
        self._sock: socket.socket | None = None

    def __enter__(self) -> 'ReadOnlyMysqlClient':
        self.connect()
        return self

    def __exit__(self, *exc) -> None:
        self.close()

    def connect(self) -> None:
        if self._sock is not None:
            return
        sock = socket.create_connection((self.config.host, self.config.port), timeout=self.config.timeout)
        sock.settimeout(self.config.timeout)
        try:
            _, g = _recv_pkt(sock)
            if g[0] == 0xFF:
                raise RuntimeError(g[9:].decode('utf-8', 'replace'))
            ver_end = g.index(0, 1)
            off = ver_end + 1 + 4
            scramble1 = g[off:off + 8]
            off += 8 + 1  # filler
            cap_low = int.from_bytes(g[off:off + 2], 'little')
            off += 2 + 1 + 2  # charset + status
            cap_high = int.from_bytes(g[off:off + 2], 'little')
            caps_srv = cap_low | (cap_high << 16)
            off += 2
            auth_len = g[off]
            off += 1 + 10  # reserved
            scramble2_len = max(13, auth_len - 8)
            scramble2 = g[off:off + scramble2_len].rstrip(b'\x00')
            scramble = scramble1 + scramble2
            token = _native_auth_token(self.config.password, scramble)

            # CLIENT_LONG_PASSWORD | CLIENT_PROTOCOL_41 | CLIENT_SECURE_CONNECTION |
            # CLIENT_PLUGIN_AUTH | CLIENT_CONNECT_WITH_DB
            caps = 0x00000001 | 0x00000200 | 0x00008000 | 0x00080000
            if self.config.database and (caps_srv & 0x00000008):
                caps |= 0x00000008
            pkt = struct.pack('<IIB23s', caps, 16 * 1024 * 1024, 45, b'\x00' * 23)
            pkt += self.config.user.encode('utf-8') + b'\x00'
            pkt += bytes([len(token)]) + token
            if caps & 0x00000008:
                pkt += self.config.database.encode('utf-8') + b'\x00'
            pkt += b'mysql_native_password\x00'
            _send_pkt(sock, 1, pkt)
            _, resp = _recv_pkt(sock)
            if resp[0] == 0xFF:
                code = int.from_bytes(resp[1:3], 'little')
                msg = resp[9:].decode('utf-8', 'replace')
                raise RuntimeError(f'NAS MySQL auth error {code}: {msg}')
            self._sock = sock
            self._exec_raw('SET SESSION TRANSACTION READ ONLY')
        except Exception:
            sock.close()
            raise

    def close(self) -> None:
        if self._sock is not None:
            try:
                _send_pkt(self._sock, 0, b'\x01')  # COM_QUIT
            except OSError:
                pass
            self._sock.close()
            self._sock = None

    def _exec_raw(self, sql: str) -> tuple[list[str], list[list[str | None]]]:
        assert self._sock is not None
        _send_pkt(self._sock, 0, b'\x03' + sql.encode('utf-8'))
        _, pkt = _recv_pkt(self._sock)
        if pkt[0] == 0xFF:
            code = int.from_bytes(pkt[1:3], 'little')
            raise RuntimeError(f'NAS MySQL error {code}: {pkt[9:].decode("utf-8", "replace")}')
        if pkt[0] == 0x00:
            return [], []
        col_cnt, _ = _read_lenenc_int(pkt, 0)
        cols: list[str] = []
        for _ in range(col_cnt):
            _, cp = _recv_pkt(self._sock)
            off = 0
            for _ in range(4):
                _, off = _read_lenenc_str(cp, off)
            name, _ = _read_lenenc_str(cp, off)
            cols.append(name or '')
        _, eof = _recv_pkt(self._sock)
        if eof[0] != 0xFE:
            raise RuntimeError(f'Expected EOF after column defs, got {eof[0]:#x}')
        rows: list[list[str | None]] = []
        while True:
            _, rp = _recv_pkt(self._sock)
            if rp[0] == 0xFE and len(rp) < 9:
                break
            if rp[0] == 0xFF:
                code = int.from_bytes(rp[1:3], 'little')
                raise RuntimeError(f'NAS MySQL stream error {code}: {rp[9:].decode("utf-8", "replace")}')
            off = 0
            row: list[str | None] = []
            for _ in range(col_cnt):
                val, off = _read_lenenc_str(rp, off)
                row.append(val)
            rows.append(row)
        return cols, rows

    def query(self, sql: str) -> tuple[list[str], list[list[str | None]]]:
        stripped = sql.lstrip()
        head = stripped.split(None, 1)[0].upper() if stripped else ''
        if head not in ('SELECT', 'SHOW', 'DESCRIBE', 'EXPLAIN'):
            raise ValueError(f'Read-only client refuses statement starting with {head!r}')
        self.connect()
        return self._exec_raw(sql)


def _format_crypto_row(row: list[str | None], fmt: str):
    open_ms = int(row[0])
    o, h, l, c = _fnum(row[1]), _fnum(row[2]), _fnum(row[3]), _fnum(row[4])
    vol = _fnum(row[5])
    close_ms = int(row[6]) if row[6] is not None else None
    qv = _fnum(row[7])
    if fmt == 'klines':
        return [open_ms, o, h, l, c]
    if fmt == 'market_cache':
        return [open_ms, o, h, l, c, vol, qv]
    if fmt == 'panel':
        vwap = (qv / vol) if (vol and qv) else c
        return [_ms_to_utc_date(open_ms), o, h, l, c, vol, vwap]
    if fmt == 'dict':
        return {
            'open_time': open_ms,
            'open_dt': row[8],
            'open': o,
            'high': h,
            'low': l,
            'close': c,
            'volume': vol,
            'close_time': close_ms,
            'quote_volume': qv,
            'count': int(row[9]) if row[9] is not None else None,
            'taker_buy_volume': _fnum(row[10]),
            'taker_buy_quote_volume': _fnum(row[11]),
        }
    raise ValueError(f'Unknown crypto format: {fmt!r}')


def _format_us_row(row: list[str | None], fmt: str):
    date_s = str(row[0])
    o, h, l, c, vol = _fnum(row[1]), _fnum(row[2]), _fnum(row[3]), _fnum(row[4]), _fnum(row[5])
    if fmt == 'panel':
        vwap = (h + l + c) / 3.0 if None not in (h, l, c) else None
        return [date_s, o, h, l, c, vol, vwap]
    if fmt == 'dict':
        return {
            'date': date_s,
            'open': o,
            'high': h,
            'low': l,
            'close': c,
            'volume': vol,
            'dividends': _fnum(row[6]),
            'stock_splits': _fnum(row[7]),
            'source': row[8],
        }
    raise ValueError(f'Unknown us format: {fmt!r}')


def _is_closed_crypto(open_ms: int, close_ms: int | None, interval: str, now_ms: int) -> bool:
    step = INTERVAL_MS.get(interval)
    if step is not None and open_ms + step > now_ms:
        return False
    if close_ms is not None and close_ms >= now_ms:
        return False
    return True


def fetch_crypto_bars(
    symbol: str,
    interval: str = '1d',
    *,
    start: int | float | dt.date | dt.datetime | str | None = None,
    end: int | float | dt.date | dt.datetime | str | None = None,
    market: str = 'binance_um',
    closed_only: bool = True,
    now_ms: int | None = None,
    limit: int | None = None,
    fmt: str = 'panel',
    client: ReadOnlyMysqlClient | None = None,
) -> list:
    _validate_ident(symbol, 'symbol')
    _validate_ident(interval, 'interval')
    _validate_ident(market, 'market')
    start_ms = _to_ms(start)
    end_ms = _to_ms(end)
    ref_ms = int(time.time() * 1000) if now_ms is None else int(now_ms)

    clauses = [
        f"market = {_quote_str(market)}",
        f"symbol = {_quote_str(symbol)}",
        f"`interval` = {_quote_str(interval)}",
    ]
    if start_ms is not None:
        clauses.append(f"open_time >= {start_ms}")
    if end_ms is not None:
        clauses.append(f"open_time <= {end_ms}")
    sql = (
        "SELECT open_time, open, high, low, close, volume, close_time, "
        "quote_volume, open_dt, count, taker_buy_volume, taker_buy_quote_volume "
        "FROM crypto_klines WHERE " + " AND ".join(clauses) + " ORDER BY open_time ASC"
    )
    if limit is not None:
        sql += f" LIMIT {int(limit)}"

    def _run(cli: ReadOnlyMysqlClient):
        _, rows = cli.query(sql)
        out = []
        for r in rows:
            open_ms = int(r[0])
            close_ms = int(r[6]) if r[6] is not None else None
            if closed_only and not _is_closed_crypto(open_ms, close_ms, interval, ref_ms):
                continue
            out.append(_format_crypto_row(r, fmt))
        return out

    if client is not None:
        return _run(client)
    with ReadOnlyMysqlClient() as cli:
        return _run(cli)


def bulk_crypto_panel_rows(
    symbols: Iterable[str],
    *,
    start: int | float | dt.date | dt.datetime | str | None = None,
    end: int | float | dt.date | dt.datetime | str | None = None,
    market: str = 'binance_um',
    interval: str = '1d',
    closed_only: bool = True,
    now_ms: int | None = None,
    client: ReadOnlyMysqlClient | None = None,
) -> dict[str, list[list]]:
    sym_list = sorted({_validate_ident(s, 'symbol') for s in symbols})
    if not sym_list:
        return {}
    _validate_ident(market, 'market')
    _validate_ident(interval, 'interval')
    start_ms = _to_ms(start)
    end_ms = _to_ms(end)
    ref_ms = int(time.time() * 1000) if now_ms is None else int(now_ms)

    def _run(cli: ReadOnlyMysqlClient) -> dict[str, list[list]]:
        out: dict[str, list[list]] = {s: [] for s in sym_list}
        for i in range(0, len(sym_list), 50):
            batch = sym_list[i:i + 50]
            in_sql = ', '.join(_quote_str(s) for s in batch)
            clauses = [
                f"market = {_quote_str(market)}",
                f"symbol IN ({in_sql})",
                f"`interval` = {_quote_str(interval)}",
            ]
            if start_ms is not None:
                clauses.append(f"open_time >= {start_ms}")
            if end_ms is not None:
                clauses.append(f"open_time <= {end_ms}")
            sql = (
                "SELECT symbol, open_time, open, high, low, close, volume, close_time, "
                "quote_volume, open_dt, count, taker_buy_volume, taker_buy_quote_volume "
                "FROM crypto_klines WHERE " + " AND ".join(clauses) + " ORDER BY symbol ASC, open_time ASC"
            )
            _, rows = cli.query(sql)
            for r in rows:
                sym = str(r[0])
                open_ms = int(r[1])
                close_ms = int(r[7]) if r[7] is not None else None
                if closed_only and not _is_closed_crypto(open_ms, close_ms, interval, ref_ms):
                    continue
                out[sym].append(_format_crypto_row(r[1:], 'panel'))
        return out

    if client is not None:
        return _run(client)
    with ReadOnlyMysqlClient() as cli:
        return _run(cli)


def fetch_us_daily(
    symbol: str,
    *,
    start: dt.date | dt.datetime | str | None = None,
    end: dt.date | dt.datetime | str | None = None,
    closed_only: bool = True,
    today_utc: str | None = None,
    limit: int | None = None,
    fmt: str = 'panel',
    client: ReadOnlyMysqlClient | None = None,
) -> list:
    _validate_ident(symbol, 'symbol')
    start_s = _to_date_str(start)
    end_s = _to_date_str(end)
    today = today_utc or dt.datetime.now(dt.timezone.utc).strftime('%Y-%m-%d')

    clauses = [f"symbol = {_quote_str(symbol)}"]
    if start_s is not None:
        clauses.append(f"`date` >= {_quote_str(start_s)}")
    if end_s is not None:
        clauses.append(f"`date` <= {_quote_str(end_s)}")
    if closed_only:
        clauses.append(f"`date` < {_quote_str(today)}")
    sql = (
        "SELECT `date`, open, high, low, close, volume, dividends, stock_splits, source "
        "FROM us_daily WHERE " + " AND ".join(clauses) + " ORDER BY `date` ASC"
    )
    if limit is not None:
        sql += f" LIMIT {int(limit)}"

    def _run(cli: ReadOnlyMysqlClient):
        _, rows = cli.query(sql)
        return [_format_us_row(r, fmt) for r in rows]

    if client is not None:
        return _run(client)
    with ReadOnlyMysqlClient() as cli:
        return _run(cli)


def bulk_us_panel_rows(
    symbols: Iterable[str],
    *,
    start: dt.date | dt.datetime | str | None = None,
    end: dt.date | dt.datetime | str | None = None,
    closed_only: bool = True,
    today_utc: str | None = None,
    client: ReadOnlyMysqlClient | None = None,
) -> dict[str, list[list]]:
    sym_list = sorted({_validate_ident(s, 'symbol') for s in symbols})
    if not sym_list:
        return {}
    start_s = _to_date_str(start)
    end_s = _to_date_str(end)
    today = today_utc or dt.datetime.now(dt.timezone.utc).strftime('%Y-%m-%d')

    def _run(cli: ReadOnlyMysqlClient) -> dict[str, list[list]]:
        out: dict[str, list[list]] = {s: [] for s in sym_list}
        for i in range(0, len(sym_list), 50):
            batch = sym_list[i:i + 50]
            in_sql = ', '.join(_quote_str(s) for s in batch)
            clauses = [f"symbol IN ({in_sql})"]
            if start_s is not None:
                clauses.append(f"`date` >= {_quote_str(start_s)}")
            if end_s is not None:
                clauses.append(f"`date` <= {_quote_str(end_s)}")
            if closed_only:
                clauses.append(f"`date` < {_quote_str(today)}")
            sql = (
                "SELECT symbol, `date`, open, high, low, close, volume, dividends, stock_splits, source "
                "FROM us_daily WHERE " + " AND ".join(clauses) + " ORDER BY symbol ASC, `date` ASC"
            )
            _, rows = cli.query(sql)
            for r in rows:
                out[str(r[0])].append(_format_us_row(r[1:], 'panel'))
        return out

    if client is not None:
        return _run(client)
    with ReadOnlyMysqlClient() as cli:
        return _run(cli)


def check_connection(config: NasMysqlConfig | None = None) -> dict:
    with ReadOnlyMysqlClient(config) as cli:
        _, info = cli.query("SELECT USER(), DATABASE(), VERSION()")
        _, tables = cli.query(
            "SELECT TABLE_NAME, TABLE_ROWS, ROUND((DATA_LENGTH+INDEX_LENGTH)/1048576, 2) "
            "FROM information_schema.TABLES WHERE TABLE_SCHEMA = DATABASE() ORDER BY TABLE_NAME"
        )
        _, cr_bounds = cli.query("SELECT MIN(open_dt), MAX(open_dt) FROM crypto_klines")
        _, aapl_bounds = cli.query("SELECT MIN(`date`), MAX(`date`) FROM us_daily WHERE symbol = 'AAPL'")
    return {
        'user': info[0][0],
        'database': info[0][1],
        'version': info[0][2],
        'tables': [
            {'name': r[0], 'approx_rows': int(r[1]), 'size_mb': float(r[2])}
            for r in tables
        ],
        'crypto_klines_range': cr_bounds[0] if cr_bounds else None,
        'us_daily_aapl_range': aapl_bounds[0] if aapl_bounds else None,
    }


def main(argv: list[str] | None = None) -> int:
    ap = argparse.ArgumentParser(description='Read-only LAN NAS MariaDB/MySQL K-line client')
    sub = ap.add_subparsers(dest='cmd', required=True)
    sub.add_parser('check', help='Verify connection and summarize tables')

    cr = sub.add_parser('crypto', help='Query crypto_klines')
    cr.add_argument('symbol')
    cr.add_argument('--interval', default='1d')
    cr.add_argument('--market', default='binance_um')
    cr.add_argument('--start', default=None)
    cr.add_argument('--end', default=None)
    cr.add_argument('--limit', type=int, default=10)
    cr.add_argument('--format', choices=('panel', 'klines', 'market_cache', 'dict'), default='dict')
    cr.add_argument('--include-open', action='store_true')

    us = sub.add_parser('us', help='Query us_daily')
    us.add_argument('symbol')
    us.add_argument('--start', default=None)
    us.add_argument('--end', default=None)
    us.add_argument('--limit', type=int, default=10)
    us.add_argument('--format', choices=('panel', 'dict'), default='dict')
    us.add_argument('--include-open', action='store_true')

    args = ap.parse_args(argv)
    if args.cmd == 'check':
        print(json.dumps(check_connection(), indent=2, ensure_ascii=False))
        return 0
    if args.cmd == 'crypto':
        rows = fetch_crypto_bars(
            args.symbol,
            args.interval,
            start=args.start,
            end=args.end,
            market=args.market,
            closed_only=not args.include_open,
            limit=args.limit,
            fmt=args.format,
        )
        print(json.dumps(rows, indent=2, ensure_ascii=False))
        return 0
    if args.cmd == 'us':
        rows = fetch_us_daily(
            args.symbol,
            start=args.start,
            end=args.end,
            closed_only=not args.include_open,
            limit=args.limit,
            fmt=args.format,
        )
        print(json.dumps(rows, indent=2, ensure_ascii=False))
        return 0
    return 1


if __name__ == '__main__':
    sys.exit(main())
