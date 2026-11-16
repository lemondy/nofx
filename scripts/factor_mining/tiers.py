"""Point-in-time liquidity/market-cap tier classification for factor mining."""
from __future__ import annotations

from typing import TYPE_CHECKING

import numpy as np
import pandas as pd

if TYPE_CHECKING:
    from .panel import Panels

VALID_TIERS = ('all', 'mega_large', 'mid_small')
US_MEGA_LARGE_ADV20 = 500_000_000.0
CRYPTO_MEGA_LARGE_ADV20 = 100_000_000.0
US_ETFS = frozenset({
    'SPY', 'QQQ', 'TQQQ', 'SQQQ', 'SOXL', 'SOXS', 'UVXY',
    'SMH', 'EWY', 'KORU', 'MUU', 'DRAM',
})


def validate_tier(tier: str) -> str:
    if tier not in VALID_TIERS:
        raise ValueError(f'tier must be one of {VALID_TIERS}, got {tier!r}')
    return tier


def default_threshold(market: str) -> float:
    if market == 'us':
        return US_MEGA_LARGE_ADV20
    if market == 'crypto':
        return CRYPTO_MEGA_LARGE_ADV20
    raise ValueError(f'market must be us or crypto, got {market!r}')


def compute_adv20(panels: Panels) -> pd.DataFrame:
    """Causal 20-bar rolling mean dollar volume (volume * vwap, or volume * close)."""
    volume = panels.fields['volume']
    price = panels.fields['vwap'] if 'vwap' in panels.fields else panels.fields['close']
    dollar_vol = (volume * price).replace([np.inf, -np.inf], np.nan)
    return dollar_vol.rolling(20, min_periods=1).mean()


def classify_tier_frame(
    panels: Panels,
    market: str,
    *,
    threshold: float | None = None,
) -> pd.DataFrame:
    """Point-in-time tier labels: 'etf', 'mega_large', 'mid_small', or '' if unmasked."""
    thr = default_threshold(market) if threshold is None else float(threshold)
    mask = panels.mask.fillna(False).astype(bool)
    adv20 = compute_adv20(panels).reindex(index=mask.index, columns=mask.columns)
    is_mega = adv20.ge(thr).fillna(False).to_numpy()
    m_arr = mask.to_numpy(dtype=bool)
    labels = np.where(m_arr, np.where(is_mega, 'mega_large', 'mid_small'), '')
    if market == 'us':
        etf_cols = np.asarray([str(col) in US_ETFS for col in mask.columns], dtype=bool)[None, :]
        labels = np.where(m_arr & etf_cols, 'etf', labels)
    return pd.DataFrame(labels, index=mask.index, columns=mask.columns, dtype=object)


def tier_mask(
    panels: Panels,
    market: str,
    tier: str = 'all',
    *,
    include_etf_in_mega: bool = False,
    threshold: float | None = None,
) -> pd.DataFrame:
    """Boolean mask restricted to the requested liquidity/market-cap tier."""
    validate_tier(tier)
    base = panels.mask.fillna(False).astype(bool)
    if tier == 'all':
        return base.copy()
    tier_df = classify_tier_frame(panels, market, threshold=threshold)
    if tier == 'mega_large':
        selected = tier_df.eq('mega_large')
        if include_etf_in_mega:
            selected = selected | tier_df.eq('etf')
        return (base & selected).astype(bool)
    return (base & tier_df.eq('mid_small')).astype(bool)


def classify_symbols_as_of(
    panels: Panels,
    market: str,
    as_of,
    *,
    threshold: float | None = None,
) -> dict[str, dict]:
    """Return {symbol: {'tier': str, 'adv20': float}} for masked symbols at or before as_of."""
    if panels.mask.empty:
        return {}
    ts = pd.Timestamp(as_of) if as_of is not None else panels.mask.index[-1]
    if ts.tzinfo is not None:
        ts = ts.tz_convert('UTC').tz_localize(None)
    ts = ts.normalize()
    idx = panels.mask.index
    if ts in idx:
        row_ts = ts
    else:
        prior = idx[idx <= ts]
        if prior.empty:
            return {}
        row_ts = prior[-1]
    # Slice up to row_ts so ADV20 calculation never reads future rows.
    sub_mask = panels.mask.loc[:row_ts]
    sub_fields = {name: frame.loc[:row_ts] for name, frame in panels.fields.items()}
    from .panel import Panels as _Panels  # local import avoids import cycle
    sub_panels = _Panels(sub_fields, sub_mask, meta=panels.meta, windows=panels.windows)
    adv20_row = compute_adv20(sub_panels).loc[row_ts]
    tier_row = classify_tier_frame(sub_panels, market, threshold=threshold).loc[row_ts]
    mask_row = sub_mask.loc[row_ts].fillna(False)
    out: dict[str, dict] = {}
    for sym in mask_row.index[mask_row]:
        val = adv20_row.get(sym, np.nan)
        out[str(sym)] = {
            'tier': str(tier_row.get(sym, '')),
            'adv20': float(val) if np.isfinite(val) else 0.0,
        }
    return out
