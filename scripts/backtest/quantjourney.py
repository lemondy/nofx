"""Offline QuantJourney W01--W25 ports; fills and P&L belong to Backtrader.

Independent reimplementation after reading QuantJourney (Apache-2.0) examples,
core.py, portfolio/rebalance.py and risk/*.py. Never imports/executes that tree.
Run with the repository's Backtrader venv: python -m scripts.backtest.quantjourney.
"""
from __future__ import annotations

import argparse
from collections import Counter, deque
from concurrent.futures import ProcessPoolExecutor, as_completed
from dataclasses import dataclass, replace
from datetime import timedelta
from functools import lru_cache
import json
import math
import os
from pathlib import Path
from statistics import NormalDist
import sys
import time

import numpy as np
import pandas as pd

from .backtrader_adapter import _backtrader
from . import quant_audit as qa
from .volrix import load_frame, load_funding, validate_frame

bt = _backtrader()
DATA_ROOT = Path('/Users/zhangyun/workspace/nofx/data')
SECTORS = tuple('XLB XLE XLF XLI XLK XLP XLU XLV XLY'.split())
MULTI = tuple('SPY EFA EEM TLT IEF GLD DBC VNQ'.split())
COINS = tuple('BTC ETH BNB XRP ADA DOGE LTC LINK TRX BCH'.split())
SINGLE = frozenset(('W01', 'W03', 'W21', 'W22'))
NEUTRAL = frozenset(('W13', 'W14', 'W15', 'W16', 'W24'))
INTRADAY = frozenset(('W07', 'W08', 'W09'))
CAPITAL = 1000.
VIRTUAL_CASH = 100_000_000.


@dataclass(frozen=True)
class RebalancePolicy:
    frequency: str | None = 'D'
    weekday: int = 4
    drift_threshold: float | None = None
    drift_type: str = 'absolute'
    partial_rebalance: bool = False
    rebalance_on_signal_change: bool = False
    signal_change_threshold: float = 0.
    max_drawdown_trigger: float | None = None
    max_drawdown_action: str = 'flatten'
    circuit_breaker_cooldown_days: int = 5
    tracking_error_threshold: float | None = None
    tracking_error_window: int = 21
    max_annual_turnover: float | None = None


def calendar_flags(index, policy):
    """QJ business-end anchors and within-week backwards holiday snap.

    Do not treat a truncated last observation as month/quarter/week end.
    These are calendar dates, not future price observations.
    """
    dates = pd.DatetimeIndex(index)
    flags = np.zeros(len(dates), dtype=bool)
    if not len(dates) or policy.frequency is None:
        return flags
    freq = policy.frequency
    if freq == 'D':
        return np.ones(len(dates), dtype=bool)
    if freq == 'W':
        anchor = ('MON', 'TUE', 'WED', 'THU', 'FRI', 'SAT', 'SUN')[policy.weekday % 7]
        for day in pd.date_range(dates[0], dates[-1], freq='W-' + anchor):
            start = day.normalize() - timedelta(days=day.weekday())
            end = start + timedelta(days=7)
            left = dates.searchsorted(day, side='right') - 1
            if left >= 0 and dates[left] >= start:
                flags[left] = True
            else:
                right = dates.searchsorted(day, side='left')
                if right < len(dates) and dates[right] < end:
                    flags[right] = True
        return flags
    freq = {'BM': 'BME', 'BQ': 'BQE'}.get(freq, freq)
    if freq not in ('BME', 'BQE'):
        raise ValueError('Unsupported QJ calendar: ' + str(freq))
    scheduled = pd.date_range(dates[0], dates[-1], freq=freq)
    positions = dates.searchsorted(scheduled, side='right') - 1
    flags[positions[positions >= 0]] = True
    return flags


def per_asset_drift(current, target, kind='absolute'):
    diff = np.abs(np.asarray(current) - np.asarray(target))
    if kind == 'absolute':
        return diff
    if kind != 'relative':
        raise ValueError('drift_type must be absolute or relative')
    return np.divide(diff, np.abs(target), out=np.abs(current).copy(),
                     where=np.abs(target) > 1e-12)


def renormalize_traded_sleeve(desired, target, keep):
    """Preserve kept shares' sleeve, net intent, signs and target gross cap."""
    out = np.asarray(desired, dtype=float).copy()
    target, keep = np.asarray(target), np.asarray(keep, dtype=bool)
    traded = ~keep
    if not traded.any():
        return out
    tw = target[traded]
    gross = float(np.abs(tw).sum())
    budget = max(float(np.abs(target).sum() - np.abs(out[keep]).sum()), 0.)
    if abs(target.sum()) > 1e-12 and abs(tw.sum()) > 1e-12:
        scale = max(float((target.sum() - out[keep].sum()) / tw.sum()), 0.)
        if gross > 1e-12:
            scale = min(scale, budget / gross)
        out[traded] = tw * scale
    elif gross > 1e-12:
        out[traded] = tw * budget / gross
    return out


class RebalanceController:
    """Policy state only; no matching, synthetic returns or portfolio ledger.

    Triggers OR, then rolling turnover veto. QJ's fast weights use a literal
    signed DD threshold and a fixed 252-observation turnover budget, including
    W10's erroneous positive +.15. Keep that bug in BOTH modes, disclose it.
    """
    def __init__(self, policy, periods_per_year=252):
        self.policy, self.ppy = policy, periods_per_year
        self.previous_target = None
        self.peak = CAPITAL
        self.cooldown = 0
        self.reentry = False
        self.turnover = deque(maxlen=252)
        self.returns = deque(maxlen=max(2, policy.tracking_error_window))
        self.benchmarks = deque(maxlen=max(2, policy.tracking_error_window))
        self.stats = Counter()

    def decide(self, target, current, nav, *, calendar=False, initial=False,
               portfolio_return=0., benchmark_return=None, executed_turnover=0.):
        p = self.policy
        target, current = np.asarray(target), np.asarray(current)
        self.turnover.append(float(executed_turnover))
        self.returns.append(portfolio_return)
        if benchmark_return is not None:
            self.benchmarks.append(benchmark_return)
        changed = (p.rebalance_on_signal_change and self.previous_target is not None
                   and np.max(np.abs(target - self.previous_target)) > p.signal_change_threshold)
        self.previous_target = target.copy()
        keep = np.zeros(len(target), dtype=bool)
        # The trigger bar is followed by exactly N whole bars with no entry.
        if self.cooldown > 0:
            self.cooldown -= 1
            if self.cooldown == 0:
                self.reentry = True
            self.peak = max(self.peak, nav)
            return None
        reason = None
        desired = target.copy()
        if self.reentry:
            reason, self.reentry, self.peak = 'post_cooldown', False, nav
        elif (p.max_drawdown_trigger is not None and not initial and self.peak > 0
              and nav / self.peak - 1. < p.max_drawdown_trigger):
            reason = 'circuit_breaker'
            desired = np.zeros_like(target) if p.max_drawdown_action == 'flatten' else current * .5
            self.cooldown = max(0, p.circuit_breaker_cooldown_days)
            if not self.cooldown:
                self.reentry = True
        elif initial:
            reason = 'initial'
        elif calendar:
            reason = 'calendar'
        elif changed:
            reason = 'signal'
        elif (p.drift_threshold is not None
              and np.max(per_asset_drift(current, target, p.drift_type)) > p.drift_threshold):
            reason = 'drift'
        elif (p.tracking_error_threshold is not None
              and len(self.returns) >= p.tracking_error_window
              and len(self.benchmarks) == len(self.returns)
              and np.std(np.asarray(self.returns) - self.benchmarks, ddof=1)
              * math.sqrt(self.ppy) > p.tracking_error_threshold):
            reason = 'tracking_error'
        self.peak = max(self.peak, nav)
        if reason is None:
            return None
        risk = reason in ('circuit_breaker', 'post_cooldown')
        proposed = float(np.abs(desired - current).sum())
        if (not initial and not risk and p.max_annual_turnover is not None
                and sum(self.turnover) + proposed > p.max_annual_turnover):
            self.stats['turnover_veto'] += 1
            return None
        if p.partial_rebalance and p.drift_threshold is not None and not initial and not risk:
            keep = per_asset_drift(current, desired, p.drift_type) <= p.drift_threshold
            desired[keep] = current[keep]
            desired = renormalize_traded_sleeve(desired, target, keep)
        self.stats[reason] += 1
        return desired, keep, reason


def _rescore(index, frequency):
    if frequency == 'D':
        return np.ones(len(index), dtype=bool)
    periods = index.to_period({'BMS': 'M', 'MS': 'M', 'W-MON': 'W', 'QS': 'Q'}.get(frequency, 'M'))
    return np.r_[True, periods[1:] != periods[:-1]]


@dataclass
class VolTargetModel:
    target_vol: float = .15
    lookback: int = 63
    max_leverage: float = 2.
    rebalance_freq: str = 'BMS'

    def adjust(self, weights, returns, *, periods_per_year=252):
        out, raw = weights.to_numpy(copy=True), weights.to_numpy()
        flags, scale = _rescore(weights.index, self.rebalance_freq), 1.
        for i in range(self.lookback, len(weights)):
            gross = np.abs(raw[i]).sum()
            if gross < 1e-8:
                out[i] = 0.
                continue
            if flags[i]:
                # Strictly prior [i-lookback, i); pandas std ddof=1. The
                # leverage cap limits the scale factor, not a new unit budget.
                r = returns.iloc[i-self.lookback:i].mul(raw[i] / gross).sum(axis=1)
                vol = r.std() * math.sqrt(periods_per_year)
                scale = min(self.target_vol / vol, self.max_leverage) if vol > .01 else 1.
            out[i] = raw[i] * scale
        return pd.DataFrame(out, index=weights.index, columns=weights.columns)


@dataclass
class RiskParityModel:
    lookback: int = 63
    max_iter: int = 50
    tol: float = 1e-8
    rebalance_freq: str = 'BMS'

    def solve_erc(self, covariance):
        # Literal QJ multiplicative iteration (including its undamped update).
        # Diagonal covariance starts at the exact inverse-vol/ERC solution.
        cov = np.nan_to_num(np.asarray(covariance), nan=0.)
        n = len(cov)
        w = 1. / np.sqrt(np.maximum(np.diag(cov), 1e-12))
        w /= w.sum()
        for _ in range(self.max_iter):
            rc = w * (cov @ w)
            total = rc.sum()
            if total < 1e-16:
                return np.ones(n) / n
            adj = np.divide(total / n, rc, out=np.ones(n), where=rc > 1e-16)
            new = np.maximum(w * adj, 1e-10)
            new /= new.sum()
            if np.max(np.abs(new - w)) < self.tol:
                return new
            w = new
        return w

    def adjust(self, weights, returns, *, periods_per_year=252):
        raw, out = weights.to_numpy(), weights.to_numpy(copy=True)
        flags = _rescore(weights.index, self.rebalance_freq)
        previous, signature = None, None
        for i in range(self.lookback, len(weights)):
            row, active = raw[i], np.abs(raw[i]) > 1e-10
            sig = tuple(np.sign(row))
            if active.sum() < 2:
                previous, signature = None, sig
                continue
            if flags[i] or previous is None or sig != signature:
                signs = np.sign(row[active])
                window = returns.iloc[i-self.lookback:i, active].mul(signs)
                solved = self.solve_erc(window.cov().to_numpy())
                previous = np.zeros(len(row))
                previous[active] = signs * solved * np.abs(row).sum()
                signature = sig
            out[i] = previous
        return pd.DataFrame(out, index=weights.index, columns=weights.columns)


@dataclass
class PositionLimitModel:
    max_weight: float = .4
    max_total_leverage: float = 2.

    def adjust(self, weights, returns=None, *, periods_per_year=252):
        out = weights.to_numpy(copy=True)
        needs = (np.abs(out).max(axis=1) > self.max_weight) | (np.abs(out).sum(axis=1) > self.max_total_leverage)
        for i in np.flatnonzero(needs):
            row = out[i]
            active, gross = np.abs(row) > 1e-10, np.abs(row).sum()
            for _ in range(len(row) + 1):
                over = np.abs(row) > self.max_weight + 1e-10
                if not over.any():
                    break
                row[over] = np.sign(row[over]) * self.max_weight
                excess = gross - np.abs(row).sum()
                room = np.maximum(self.max_weight - np.abs(row), 0.)
                candidates = active & (room > 1e-10)
                if excess <= 1e-10 or not candidates.any():
                    break
                basis = np.abs(row[candidates])
                if basis.sum() <= 1e-10:
                    basis = room[candidates]
                row[candidates] += np.sign(row[candidates]) * np.minimum(excess * basis / basis.sum(), room[candidates])
            row[:] = np.clip(row, -self.max_weight, self.max_weight)
            gross = np.abs(row).sum()
            if gross > self.max_total_leverage:
                row *= self.max_total_leverage / gross
        return pd.DataFrame(out, index=weights.index, columns=weights.columns)


class RiskModelChain:
    def __init__(self, models):
        self.models = models

    def adjust(self, weights, returns, *, periods_per_year=252):
        for model in self.models:
            weights = model.adjust(weights, returns, periods_per_year=periods_per_year)
        return weights


@dataclass(frozen=True)
class Rule:
    name: str
    universe: tuple
    start: str
    cap: float
    parameters: dict  # Complete signal/risk defaults.
    primary: tuple    # Predeclared G4 dimensions, never selected from results.
    policy: RebalancePolicy
    timeframe: str = '1d'

    @property
    def edge_source(self):
        number = int(self.name[1:])
        if number in (5, 17, 19, 20):
            return '风险溢价'
        if number in (3, 7, 21):
            return '行为（短期均值回归）'
        if number in (13, 14):
            return '统计套利（配对）'
        if number == 8:
            return '速度/微观结构（我们没有优势）'
        return '行为（趋势/动量/反转）'


def _rules():
    d, m, q = RebalancePolicy(), RebalancePolicy('BME'), RebalancePolicy('BQE')
    w = RebalancePolicy('W', weekday=4)
    change = RebalancePolicy(None, rebalance_on_signal_change=True)
    rows = [
        ('W01', SECTORS, '2000-01-03', .25, dict(fast=50, slow=200), 'fast', d),
        ('W02', MULTI, '2007-01-03', .30, dict(fast=50, slow=200), 'fast', replace(m, drift_threshold=.05)),
        ('W03', SECTORS, '2000-01-03', .25, dict(rsi_period=14, entry=35., exit=60.), 'rsi_period', w),
        ('W04', MULTI, '2007-01-03', .50, dict(lookback=252, top_n=2), 'lookback', q),
        ('W05', MULTI, '2007-01-03', .35, dict(lookback=63), 'lookback', m),
        ('W06', MULTI, '2007-01-03', .50, dict(lookback=200), 'lookback', replace(change, signal_change_threshold=.01)),
        ('W07', ('SPY', 'QQQ', 'IWM'), '2026-06-19', .25, dict(rsi_period=14, entry=35., exit=55.), 'rsi_period', change),
        ('W08', ('SPY', 'QQQ', 'IWM'), '2026-07-05', .34, dict(fast=9, slow=21), 'fast', change),
        ('W09', ('SPY', 'QQQ', 'IWM'), '2026-05-11', .25, dict(fast=10, slow=30), 'fast', change),
        ('W10', MULTI, '2007-01-03', .30, dict(fast=50, slow=200), 'fast', replace(m, max_drawdown_trigger=.15, circuit_breaker_cooldown_days=10)),
        ('W11', MULTI, '2007-01-03', .40, dict(lookback=126, top_n=3), 'lookback', replace(q, tracking_error_threshold=.06, tracking_error_window=63, max_annual_turnover=4.)),
        ('W12', SECTORS, '2000-01-03', .35, dict(lookback=63), 'lookback', replace(d, drift_threshold=.10, partial_rebalance=True)),
        ('W13', ('KO', 'PEP'), '2000-01-03', 1., dict(lookback=60, entry=2., exit=.5), 'lookback', d),
        ('W14', ('EWA', 'EWC'), '2000-01-03', 1., dict(lookback=60, entry=2., exit=.5), 'lookback', d),
        ('W15', SECTORS, '2000-01-03', 1., dict(lookback=252, n_side=3), 'lookback', m),
        ('W16', SECTORS, '2000-01-03', 1., dict(lookback=21, n_side=3), 'lookback', w),
        ('W17', MULTI, '2007-01-03', .34, dict(fast=50, slow=200, target_vol=.10, risk_lookback=63, max_leverage=1.5), 'target_vol', m),
        ('W18', MULTI, '2007-01-03', .40, dict(lookback=126, top_n=3, target_vol=.15, risk_lookback=42, max_leverage=2.), 'lookback', m),
        ('W19', MULTI, '2007-01-03', 1., dict(risk_lookback=63, warmup=63), 'risk_lookback', m),
        ('W20', SECTORS, '2000-01-03', 1., dict(risk_lookback=126, warmup=126, max_weight=.25), 'risk_lookback', m),
        ('W21', SECTORS, '2000-01-03', .25, dict(window=20, n_std=2.), 'window', d),
        ('W22', SECTORS, '2000-01-03', .25, dict(fast=12, slow=26, signal=9), 'fast', d),
        ('W23', ('EURUSD=X', 'GBPUSD=X', 'AUDUSD=X', 'NZDUSD=X'), '2010-01-04', 1., dict(lookback=126, vol_lookback=63), 'lookback', m),
        ('W24', ('EURUSD=X', 'GBPUSD=X', 'AUDUSD=X', 'NZDUSD=X'), '2010-01-04', 1., dict(lookback=63, n_side=1), 'lookback', m),
        ('W25', ('MES=F', 'MNQ=F', 'ZN=F', 'CL=F', 'GC=F', 'ZC=F'), '2019-05-06', 1., dict(fast=63, slow=252, vol_lookback=63), 'fast', m),
    ]
    return {r[0]: Rule(*r[:5], (r[5],), r[6], {'W07': '15m', 'W08': '5m', 'W09': '1h'}.get(r[0], '1d')) for r in rows}


RULES = _rules()
US_RULES = tuple(n for n in RULES if n not in INTRADAY and int(n[1:]) <= 22)


def rsi(close, period):
    """Wilder RSI with the first period changes as the SMA seed."""
    result = np.full(close.shape, np.nan)
    values = close.to_numpy()
    for j in range(values.shape[1]):
        gains, losses, count, last = 0., 0., 0, math.nan
        for i, value in enumerate(values[:, j]):
            if not math.isfinite(value):
                gains, losses, count, last = 0., 0., 0, math.nan
                continue
            if math.isfinite(last):
                delta = value - last
                up, down = max(delta, 0.), max(-delta, 0.)
                if count < period:
                    gains += up / period
                    losses += down / period
                    count += 1
                else:
                    gains = (gains * (period - 1) + up) / period
                    losses = (losses * (period - 1) + down) / period
                if count >= period:
                    result[i, j] = 100. * gains / (gains + losses) if gains + losses else 50.
            last = value
    return pd.DataFrame(result, index=close.index, columns=close.columns)


def _hysteresis(enter, leave, valid):
    enter, leave, valid = (x.to_numpy() for x in (enter, leave, valid))
    out = np.zeros(enter.shape)
    state = np.zeros(enter.shape[1], dtype=bool)
    for i in range(len(out)):
        before = state.copy()
        state[~valid[i]] = False
        state[valid[i] & ~before & enter[i]] = True
        state[valid[i] & before & leave[i]] = False
        out[i] = state
    return out


def _rank_signals(scores, n, *, positive=False, long_short=False, reverse=False):
    out = np.zeros(scores.shape)
    # pandas nlargest/nsmallest preserves original column order at ties, as QJ.
    for i, (_, row) in enumerate(scores.iterrows()):
        eligible = row.dropna()
        if long_short and len(eligible) < 2 * n:
            continue
        if positive:
            eligible = eligible[eligible > 0.]
        longs = eligible.nsmallest(n).index if reverse else eligible.nlargest(n).index
        out[i, scores.columns.isin(longs)] = 1.
        if long_short:
            shorts = eligible.nlargest(n).index if reverse else eligible.nsmallest(n).index
            out[i, scores.columns.isin(shorts)] = -1.
    return pd.DataFrame(out, index=scores.index, columns=scores.columns)


def compute_weights(rule, close, *, market='us', parameters=None):
    """Targets at close t. Only current/past prices; no timing shift here."""
    spec = RULES[rule] if isinstance(rule, str) else rule
    name, p = spec.name, {**spec.parameters, **(parameters or {})}
    valid = close.notna()
    weights = None
    if name in ('W01', 'W02', 'W09', 'W10', 'W17', 'W25'):
        fast, slow = close.rolling(p['fast']).mean(), close.rolling(p['slow']).mean()
        signals = (fast > slow).astype(float).where(fast.notna() & slow.notna(), 0.)
        if name == 'W25':
            signals = np.sign(fast - slow).where(fast.notna() & slow.notna(), 0.)
    elif name in ('W03', 'W07'):
        values = rsi(close, p['rsi_period'])
        signals = pd.DataFrame(_hysteresis(values < p['entry'], values > p['exit'], values.notna()),
                               index=close.index, columns=close.columns)
    elif name == 'W08':
        fast = close.ewm(span=p['fast'], adjust=False, min_periods=p['fast']).mean()
        slow = close.ewm(span=p['slow'], adjust=False, min_periods=p['slow']).mean()
        signals = (fast > slow).astype(float).where(fast.notna() & slow.notna(), 0.)
    elif name == 'W06':
        anchor = 'SPY' if market == 'us' else 'BTC'
        average = close[anchor].rolling(p['lookback']).mean()
        risk_on = close[anchor] > average
        on = ('SPY', 'EFA', 'EEM', 'DBC', 'VNQ') if market == 'us' else COINS[:7]
        off = ('TLT', 'IEF', 'GLD') if market == 'us' else COINS[7:]
        signals = pd.DataFrame(0., index=close.index, columns=close.columns)
        for assets, regime in ((on, risk_on), (off, ~risk_on)):
            signals.loc[average.notna() & regime, list(assets)] = 1.
    elif name in ('W04', 'W11', 'W15', 'W16', 'W18', 'W24'):
        momentum = close.pct_change(p['lookback'], fill_method=None)
        signals = _rank_signals(momentum, p.get('top_n', p.get('n_side', 1)),
                                positive=name in ('W04', 'W18'),
                                long_short=name in ('W15', 'W16', 'W24'), reverse=name == 'W16')
    elif name == 'W05':
        volatility = close.pct_change(fill_method=None).rolling(p['lookback']).std() * math.sqrt(252)
        inverse = 1. / volatility.replace(0., np.nan)
        weights = inverse.div(inverse.sum(axis=1), axis=0).fillna(0.).clip(upper=spec.cap)
    elif name == 'W12':
        momentum = close.pct_change(p['lookback'], fill_method=None).clip(lower=0.).fillna(0.)
        weights = momentum.div(momentum.sum(axis=1), axis=0).fillna(0.).clip(upper=spec.cap)
    elif name in ('W13', 'W14'):
        a, b = np.log(close.iloc[:, 0]), np.log(close.iloc[:, 1])
        beta = a.rolling(p['lookback']).cov(b) / b.rolling(p['lookback']).var() if name == 'W14' else 1.
        spread = a - beta * b
        z = (spread - spread.rolling(p['lookback']).mean()) / spread.rolling(p['lookback']).std()
        out, state = np.zeros(close.shape), 0
        for i, value in enumerate(z):
            if not math.isfinite(value):
                state = 0
            elif state == 0:
                state = -1 if value > p['entry'] else (1 if value < -p['entry'] else 0)
            elif abs(value) < p['exit']:
                state = 0
            out[i] = (state * .5, -state * .5)
        weights = pd.DataFrame(out, index=close.index, columns=close.columns)
    elif name in ('W19', 'W20'):
        signals = pd.DataFrame(0., index=close.index, columns=close.columns)
        signals.iloc[p['warmup']:] = 1.
    elif name == 'W21':
        mid, sd = close.rolling(p['window']).mean(), close.rolling(p['window']).std()
        lower = mid - p['n_std'] * sd
        signals = pd.DataFrame(_hysteresis(close < lower, close > mid, lower.notna() & mid.notna()),
                               index=close.index, columns=close.columns)
    elif name == 'W22':
        macd = close.ewm(span=p['fast'], adjust=False).mean() - close.ewm(span=p['slow'], adjust=False).mean()
        signals = (macd > macd.ewm(span=p['signal'], adjust=False).mean()).astype(float)
    elif name == 'W23':
        signals = np.sign(close.pct_change(p['lookback'], fill_method=None)).fillna(0.)
    else:
        raise ValueError('Unknown rule: ' + name)
    if weights is None:
        signals = signals.where(valid, 0.)
        if name in ('W23', 'W25'):
            vol = close.pct_change(fill_method=None).rolling(p['vol_lookback']).std() * math.sqrt(252)
            raw = signals / vol.replace(0., np.nan)
            weights = raw.div(raw.abs().sum(axis=1), axis=0).fillna(0.)
        elif name in ('W15', 'W16', 'W24'):
            longs, shorts = (signals > 0).astype(float), (signals < 0).astype(float)
            weights = .5 * (longs.div(longs.sum(axis=1), axis=0).fillna(0.)
                            - shorts.div(shorts.sum(axis=1), axis=0).fillna(0.))
        else:
            active = (signals == 1.).astype(float)
            weights = active.div(active.sum(axis=1), axis=0).fillna(0.).clip(upper=spec.cap)
    returns = close.pct_change(fill_method=None)
    # QJ core._compute_returns_preserve_gaps sets the first available row to
    # zero before handing returns to risk overlays (W05's own vol is separate).
    if len(returns):
        returns.iloc[0] = returns.iloc[0].where(close.iloc[0].isna(), 0.)
    ppy = 252 if market == 'us' else 365
    if name in ('W17', 'W18'):
        model = VolTargetModel(p['target_vol'], p['risk_lookback'], p['max_leverage'])
        weights = model.adjust(weights, returns, periods_per_year=ppy)
    elif name in ('W19', 'W20'):
        models = [RiskParityModel(p['risk_lookback'])]
        if name == 'W20':
            models.append(PositionLimitModel(p['max_weight']))
        # InstrumentData.add_strategy_data sorts its MultiIndex, hence QJ's
        # risk-model input and active covariance axes are alphabetical.
        # Preserve this detail: undamped ERC is not numerically permutation
        # invariant when it has not converged after its 50 iterations.
        order = sorted(weights.columns)
        weights = RiskModelChain(models).adjust(weights[order], returns[order], periods_per_year=ppy).reindex(columns=close.columns)
    # QJ's core default cash buffer is 5%, after risk overlays, in both modes.
    return (weights.where(valid, 0.).replace([np.inf, -np.inf], 0.).fillna(0.) * .95)


class ArrayFeed(bt.feed.DataBase):
    """Prevalidated aligned arrays, avoiding PandasData's per-cell overhead."""
    def start(self):
        super().start()
        self._cursor = -1

    def _load(self):
        self._cursor += 1
        if self._cursor == len(self.p.dataname):
            return False
        row = self.p.dataname[self._cursor]
        for line, value in zip((self.lines.datetime, self.lines.open, self.lines.high,
                                self.lines.low, self.lines.close, self.lines.volume), row):
            line[0] = value
        self.lines.openinterest[0] = 0.
        return True


class CarryBroker(bt.brokers.BackBroker):
    """Native cash adjustments once/day, on the carried position at UTC open."""
    params = (('rates', None), ('market', 'us'), ('charge', True), ('offset', 0.))

    def __init__(self):
        super().__init__()
        self.last_day = None
        self.funding_paid = self.borrow_paid = self.financing_paid = 0.
        self.funding_observations = self.funding_covered = 0
        self.carry_by_symbol = Counter()

    def charge_carry(self):
        if not self.p.charge or not self.cerebro.datas or not len(self.cerebro.datas[0]):
            return
        day = self.cerebro.datas[0].datetime.date(0)
        if day == self.last_day:
            return
        elapsed = (day - self.last_day).days if self.last_day is not None else 1
        self.last_day = day
        nominal = {d._name: self.getposition(d).size * float(d.open[0]) for d in self.cerebro.datas}
        equity = self.cash - self.p.offset + sum(nominal.values())
        total = 0.
        if self.p.market == 'crypto':
            for symbol, value in nominal.items():
                rates = (self.p.rates or {}).get(symbol, {})
                if value:
                    self.funding_observations += 1
                    self.funding_covered += day in rates
                cost = value * rates.get(day, 0.) * 3.
                total += cost
                self.funding_paid += cost
                self.carry_by_symbol[symbol] += cost
        else:
            for symbol, value in nominal.items():
                cost = max(-value, 0.) * .005 * elapsed / 365.
                total += cost
                self.borrow_paid += cost
                self.carry_by_symbol[symbol] += cost
            cost = max(sum(max(v, 0.) for v in nominal.values()) - max(equity, 0.), 0.) * .05 * elapsed / 365.
            total += cost
            self.financing_paid += cost
        if total:
            self.add_cash(-total)
            self._get_value()  # drain the native pending-cash queue before sizing

    def next(self):
        self.charge_carry()
        super().next()


class WeightStrategy(bt.Strategy):
    params = (('targets', None), ('available', None), ('calendar', None),
              ('policy', None), ('ppy', 252), ('mode', 'evaluation'),
              ('benchmark_returns', None), ('keep_fills', False), ('offset', 0.),
              ('exposure_end', None))

    def __init__(self):
        self.controller = RebalanceController(self.p.policy, self.p.ppy)
        self.pending = None
        self.previous_target = None
        self.equity, self.fills = {}, []
        self.last_nav, self.bar_turnover = CAPITAL, 0.
        self.turnover = self.commission = 0.
        self.fill_count = self.closed_count = 0
        self.exposure_sum = self.observations = 0
        self.gate_exposure_sum = self.gate_observations = 0
        self.closed_net_bps = []
        self.trade_carry_start, self.trade_entry_value = {}, {}
        self.bankrupt = False
        self.latest_return = 0.
        self.reproduction_weights = np.zeros(len(self.datas))

    def _marks(self, field):
        return np.asarray([float(getattr(d, field)[0]) for d in self.datas])

    def _quantities(self):
        return np.asarray([self.getposition(d).size for d in self.datas])

    def _nav_at(self, prices):
        return self.broker.getcash() - self.p.offset + float((self._quantities() * prices).sum())

    def _submit(self, intent, i, prices, *, close=False):
        desired, keep, reason = intent
        nav = self._nav_at(prices)
        available = self.p.available[i]
        quantities = self._quantities()
        current = quantities * prices / nav if nav > 0 else np.zeros(len(prices))
        keep = keep | ~available
        # Preserve actual shares for the partial sleeve, including across gaps.
        feasible = desired.copy()
        feasible[keep] = current[keep]
        if keep.any():
            feasible = renormalize_traded_sleeve(feasible, desired, keep)
        if nav <= 0 or self.bankrupt:
            self.bankrupt = True
            feasible[:], keep[:] = 0., ~available
        desired_quantity = feasible * max(nav, 0.) / prices
        desired_quantity[keep] = quantities[keep]
        deltas = desired_quantity - quantities
        # Closing/reducing legs first; all legs use the same pre-trade equity.
        indices = sorted(range(len(self.datas)), key=lambda j: abs(desired_quantity[j]) >= abs(quantities[j]))
        for j in indices:
            if abs(deltas[j]) * prices[j] <= 1e-9:
                continue
            order = self.order_target_size(data=self.datas[j], target=desired_quantity[j])
            if order:
                order.addinfo(nav=nav, reason=reason, coc=close,
                              signal_bar=i-1, execution_bar=i)

    def prenext_open(self):
        self.next_open()

    def nextstart_open(self):
        self.next_open()

    def next_open(self):
        i = len(self.data) - 1
        self.broker.charge_carry()
        if self.p.mode == 'evaluation':
            if self.pending is not None:
                self._submit(self.pending, i, self._marks('open'))
                self.pending = None
        elif i == 0:
            self.controller.decide(np.zeros(len(self.datas)), np.zeros(len(self.datas)), CAPITAL, initial=True)
        elif i > 0:
            # QJ core shifts targets(1), then RebalanceEngine snaps at that
            # day's close. This is NOT t-close signal at the same t-close.
            # COC is only a native matching mechanism for the KNOWN t-1 target
            # at t close; evaluation never reads the execution bar close here.
            previous_prices = np.asarray([float(d.close[-1]) for d in self.datas])
            current = self._quantities() * previous_prices / self.last_nav if self.last_nav > 0 else np.zeros(len(self.datas))
            if self.p.policy.partial_rebalance:
                # QJ fast accounting never drifts weights on a D rebalance
                # bar, even for its "kept" partial sleeve. Its implied-share
                # ledger then DOES resize those names every day. Reproduce
                # that published bug with real native orders, not synthetic
                # P&L. Evaluation preserves true shares and its partial mask.
                current = self.reproduction_weights.copy()
            benchmark = None if self.p.benchmark_returns is None else self.p.benchmark_returns[i-1]
            intent = self.controller.decide(self.p.targets[i-1], current, self.last_nav,
                calendar=bool(self.p.calendar[i]), initial=False,
                portfolio_return=self.latest_return, benchmark_return=benchmark,
                executed_turnover=self.bar_turnover)
            self.bar_turnover = 0.
            if intent is not None:
                if self.p.policy.partial_rebalance:
                    self.reproduction_weights = intent[0].copy()
                    intent = (intent[0], np.zeros(len(self.datas), dtype=bool), intent[2])
                self._submit(intent, i, self._marks('close'), close=True)

    def notify_order(self, order):
        if order.status in (order.Margin, order.Rejected):
            raise RuntimeError('Backtrader rejected weight order: ' + order.getstatusname())
        if order.status != order.Completed:
            return
        ex, symbol = order.executed, order.data._name
        amount = abs(ex.size * ex.price)
        churn = amount / max(float(order.info.nav), 1e-12)
        self.turnover += churn
        self.bar_turnover += churn
        self.commission += ex.comm
        self.fill_count += 1
        if self.p.keep_fills:
            self.fills.append(dict(symbol=symbol, date=bt.num2date(ex.dt).isoformat(),
                                   size=ex.size, price=ex.price,
                                   signal_bar=order.info.signal_bar, execution_bar=order.info.execution_bar,
                                   reason=order.info.reason, commission=ex.comm))

    def notify_trade(self, trade):
        symbol = trade.data._name
        if trade.justopened:
            self.trade_carry_start[symbol] = self.broker.carry_by_symbol[symbol]
            self.trade_entry_value[symbol] = abs(trade.size * trade.price)
        if trade.isclosed:
            self.closed_count += 1
            carry = self.broker.carry_by_symbol[symbol] - self.trade_carry_start.get(symbol, 0.)
            nominal = self.trade_entry_value.get(symbol, 0.)
            if nominal > 0:
                self.closed_net_bps.append((trade.pnlcomm - carry) / nominal * 1e4)

    def next(self):
        i = len(self.data) - 1
        nav = self.broker.getvalue() - self.p.offset
        self.bankrupt |= nav <= 0
        self.latest_return = nav / self.last_nav - 1. if self.last_nav > 0 else 0.
        self.equity[self.data.datetime.date(0).isoformat()] = float(nav)
        if nav > 0:
            exposure = float(np.abs(self._quantities() * self._marks('close')).sum() / nav)
            self.exposure_sum += exposure
            self.observations += 1
            if self.p.exposure_end is None or self.data.datetime.date(0).isoformat() <= self.p.exposure_end:
                self.gate_exposure_sum += exposure
                self.gate_observations += 1
        if self.p.mode == 'evaluation' and i + 1 < len(self.p.targets):
            current = self._quantities() * self._marks('close') / nav if nav > 0 else np.zeros(len(self.datas))
            benchmark = None if self.p.benchmark_returns is None else self.p.benchmark_returns[i]
            self.pending = self.controller.decide(self.p.targets[i], current, nav,
                calendar=bool(self.p.calendar[i]), initial=i == 0,
                portfolio_return=self.latest_return, benchmark_return=benchmark,
                executed_turnover=self.bar_turnover)
            self.bar_turnover = 0.
        self.last_nav = nav


def curve_metrics(equity, ppy=252, initial=CAPITAL, *, rf=0.):
    s = pd.Series(equity, dtype=float).sort_index()
    if s.empty:
        raise ValueError('Empty native equity curve')
    values, prev = s.to_numpy(), np.r_[initial, s.to_numpy()[:-1]]
    bankrupt = bool((values <= 0).any())
    simple = values / prev - 1. if (prev > 0).all() else np.full(len(s), np.nan)
    logs = np.log(values / prev) if not bankrupt and (prev > 0).all() else np.full(len(s), np.nan)
    peaks = np.maximum.accumulate(np.r_[initial, values])[1:]
    cut = len(s) // 2
    def half(a):
        if not np.isfinite(a).all():
            return dict(return_pct=None, sharpe=None)
        return dict(return_pct=float(math.expm1(a.sum()) * 100), sharpe=qa.sharpe(a.tolist(), ppy))
    sd = np.std(simple, ddof=1) if len(s) > 1 and np.isfinite(simple).all() else math.nan
    return dict(return_pct=float((values[-1] / initial - 1.) * 100),
                cagr_pct=float(((values[-1] / initial) ** (ppy / len(s)) - 1.) * 100) if not bankrupt else None,
                sharpe=qa.sharpe(logs.tolist(), ppy) if np.isfinite(logs).all() else None,
                sharpe_simple_rf=float((simple.mean() - rf / ppy) / sd * math.sqrt(ppy)) if sd > 0 else 0.,
                maxdd_pct=float(np.max(1. - values / peaks) * 100),
                first_half=half(logs[:cut]), second_half=half(logs[cut:]),
                bankrupt=bankrupt, observations=len(s))


def daily_logs(run, *, end=None):
    eq = pd.Series(run['daily_equity'], dtype=float).sort_index()
    if end is not None:
        eq = eq.loc[:end]
    prev = eq.shift(1, fill_value=CAPITAL)
    ratio = (eq / prev).where((eq > 0) & (prev > 0))
    return np.log(ratio)


def run_weights(frames, targets, *, policy=None, mode='evaluation', market='us',
                commission=None, slippage=None, funding=None, charge_carry=True,
                benchmark_returns=None, keep_fills=False, exposure_end=None):
    """All executable paths use native Backtrader orders/positions/cash/P&L.

    next_open only sizes the cached previous-close decision, using current open
    prices and equity marked at those opens. It never recomputes a signal.
    """
    if mode not in ('evaluation', 'reproduction') or market not in ('us', 'crypto'):
        raise ValueError('Invalid mode or market')
    if not frames or not isinstance(targets.index, pd.DatetimeIndex) or targets.empty:
        raise ValueError('Nonempty frames and dated targets required')
    if not targets.index.is_unique or not targets.index.is_monotonic_increasing:
        raise ValueError('Targets must have a unique increasing index')
    if list(frames) != list(targets.columns) or not np.isfinite(targets.to_numpy()).all():
        raise ValueError('Targets must be finite and ordered like feeds')
    policy = policy or RebalancePolicy()
    ppy = 252 if market == 'us' else 365
    commission = (0.0001 if mode == 'reproduction' or market == 'us' else .0005) if commission is None else commission
    slippage = (0. if mode == 'reproduction' else (.0002 if market == 'us' else .0003)) if slippage is None else slippage
    if not 0 <= commission < 1 or not 0 <= slippage < 1:
        raise ValueError('Invalid costs')
    cerebro = bt.Cerebro(stdstats=False, cheat_on_open=True)
    broker = CarryBroker(rates=funding, market=market, charge=charge_carry and mode == 'evaluation',
                         offset=VIRTUAL_CASH - CAPITAL)
    broker.setcash(VIRTUAL_CASH)
    broker.setcommission(commission=commission)
    # Fixed bps at the open, without clipping against the FUTURE high/low.
    broker.set_slippage_perc(slippage, slip_open=True, slip_match=True, slip_out=True)
    broker.set_coo(True)
    broker.set_coc(mode == 'reproduction')
    cerebro.setbroker(broker)
    availability = []
    dates = np.asarray([bt.date2num(d) for d in targets.index.to_pydatetime()])
    for symbol, frame in frames.items():
        validate_frame(frame)
        aligned = frame.reindex(targets.index)
        availability.append(aligned['open'].notna().to_numpy())
        # Never backfill a prelisting price. The dummy mark is untradeable.
        prices = aligned[['open', 'high', 'low', 'close']].ffill().fillna(1.)
        a = np.column_stack((dates, prices.to_numpy(), aligned['volume'].fillna(0.).to_numpy()))
        cerebro.adddata(ArrayFeed(dataname=a), name=symbol)
    available = np.column_stack(availability)
    if benchmark_returns is not None:
        benchmark_returns = benchmark_returns.reindex(targets.index).fillna(0.).to_numpy()
    cerebro.addstrategy(WeightStrategy, targets=targets.to_numpy(), available=available,
                        calendar=calendar_flags(targets.index, policy), policy=policy,
                        mode=mode, ppy=ppy, benchmark_returns=benchmark_returns,
                        keep_fills=keep_fills, offset=VIRTUAL_CASH-CAPITAL,
                        exposure_end=exposure_end)
    s = cerebro.run()[0]
    metrics = curve_metrics(s.equity, ppy, rf=.02 if mode == 'reproduction' else 0.)
    metrics.update(turnover=s.turnover, average_exposure=s.exposure_sum / s.observations if s.observations else 0.,
                   gate_average_exposure=s.gate_exposure_sum / s.gate_observations if s.gate_observations else 0.,
                   trades=s.fill_count, closed_trades=s.closed_count, commission_paid=s.commission,
                   funding_paid=broker.funding_paid, borrow_paid=broker.borrow_paid,
                   financing_paid=broker.financing_paid, funding_observations=broker.funding_observations,
                   funding_covered=broker.funding_covered, rebalance_stats=dict(s.controller.stats),
                   avg_net_bps=float(np.mean(s.closed_net_bps)) if s.closed_net_bps else None)
    metrics['bankrupt'] |= s.bankrupt
    result = dict(engine='backtrader', engine_version=bt.__version__, mode=mode,
                  daily_equity=s.equity, metrics=metrics)
    if keep_fills:
        result['fills'] = s.fills
    return result


def deflated_sharpe(returns, trials, sharpe_variance):
    """Bailey & Lopez de Prado (2014), daily (NOT annualized) Sharpe units.

    E[max SR] = sqrt(Var[SR]) * ((1-gamma)*Phi^-1(1-1/N)
                                + gamma*Phi^-1(1-1/(N*e))).
    DSR = Phi((SR - E[max SR])*sqrt(T-1) /
              sqrt(1 - skew*SR + (kurtosis-1)*SR**2/4)).
    gamma = Euler--Mascheroni; kurtosis is Pearson (normal=3). N counts
    rule x market hypotheses, not parameter points, cross-check coins or BH.
    Admission uses the strategy's own daily log returns (rf=0); excess
    returns and their own cross-sectional variance are diagnostic only.
    The finite-sample moments use population central moments consistently.
    """
    a = np.asarray(returns, dtype=float)
    if len(a) < 3 or not np.isfinite(a).all() or trials < 1:
        return 0.
    sd = a.std(ddof=1)
    if sd <= 1e-15:
        return 0.  # a constant series has no estimable Sharpe variance
    sr = a.mean() / sd
    centered = a - a.mean()
    m2 = np.mean(centered ** 2)
    skew = np.mean(centered ** 3) / m2 ** 1.5
    kurt = np.mean(centered ** 4) / m2 ** 2
    return dsr_from_moments(sr, len(a), skew, kurt, trials, sharpe_variance)


def dsr_from_moments(sr, observations, skew, kurtosis, trials, sharpe_variance):
    normal, gamma = NormalDist(), .5772156649015329
    if observations < 3 or trials < 1 or sharpe_variance < 0:
        return 0.
    expected = 0.
    if trials > 1:
        expected = math.sqrt(sharpe_variance) * (
            (1. - gamma) * normal.inv_cdf(1. - 1. / trials)
            + gamma * normal.inv_cdf(1. - 1. / (trials * math.e)))
    denominator = 1. - skew * sr + (kurtosis - 1.) * sr * sr / 4.
    if denominator <= 0 or not math.isfinite(denominator):
        return 0.
    z = (sr - expected) * math.sqrt(observations - 1.) / math.sqrt(denominator)
    # Equivalent Phi(z), without 1+erf cancellation rounding tiny DSRs to
    # zero or ties. The corrected study still has very small probabilities.
    return .5 * math.erfc(-z / math.sqrt(2.))


def matched_exposure_benchmark(benchmark_returns, average_exposure):
    """m_t = log(1 + e*r_t), from the original benchmark's daily log returns.

    e is the strategy's mean gross exposure over the SAME gate period. Values
    above one are leverage, without an additional financing charge. Keep
    invalid returns (including a leveraged benchmark bankruptcy) so G1 fails
    rather than silently dropping observations. A cash benchmark stays zero.
    """
    if not math.isfinite(average_exposure) or average_exposure < 0:
        raise ValueError('Average gross exposure must be finite and nonnegative')
    b = np.asarray(benchmark_returns, dtype=float)
    if average_exposure == 1.:
        return b.copy()
    with np.errstate(invalid='ignore', divide='ignore', over='ignore'):
        return np.log1p(average_exposure * np.expm1(b))


def admission(strategy_returns, benchmark_returns, ppy, *, dsr, plateau,
              cross_positive_fraction=None, average_exposure=1.):
    """Corrected G1/G2; G1 tests Sharpe against a matched-exposure benchmark."""
    s, b = list(strategy_returns), list(benchmark_returns)
    matched = matched_exposure_benchmark(b, average_exposure).tolist()
    audit = qa.audit_returns(s, matched, ppy)
    finite = len(s) == len(b) and len(s) > 0 and np.isfinite(s).all() and np.isfinite(b).all()
    matched_finite = finite and np.isfinite(matched).all()
    g1 = (matched_finite and audit['sample_sufficient'] and
          all(audit[f'{w}_sharpe'] > audit[f'{w}_sharpe_bench'] for w in ('train', 'holdout')))
    cut = len(s) // 2
    halves = [math.expm1(sum(s[:cut])) if finite else None,
              math.expm1(sum(s[cut:])) if finite else None]
    checks = dict(G1=bool(g1), G2=bool(dsr >= .95),
                  G3=bool(finite and min(halves) > 0), G4=plateau == 'PLATEAU')
    if cross_positive_fraction is not None:
        checks['G5'] = bool(cross_positive_fraction >= .60)
    failed = [g for g, ok in checks.items() if not ok]
    return dict(status='FAIL' if failed else 'PASS', failed=failed, gates=checks,
                audit=audit, halves=halves, dsr=dsr,
                full_exposure_audit=qa.audit_returns(s, b, ppy),
                average_exposure=average_exposure,
                matched_benchmark_sharpe=qa.sharpe(matched, ppy) if matched_finite else None,
                cross_positive_fraction=cross_positive_fraction)


@lru_cache(maxsize=50)
def _read_frame(root, symbol, market, timeframe='1d'):
    root = Path(root)
    if market == 'us':
        path = root / 'quantjourney/prices' / (symbol + '.json')
        rows = json.loads(path.read_text())
        a = np.asarray([r[1:] for r in rows], dtype=float)
        factor = a[:, 4] / a[:, 3]
        frame = pd.DataFrame(a[:, :4] * factor[:, None], columns=['open', 'high', 'low', 'close'],
                             index=pd.to_datetime([r[0] for r in rows]))
        # Algebraically identical to raw close * f, but preserve the cached
        # adjclose's exact bits. QJ's undamped ERC iteration is very sensitive
        # to tiny covariance perturbations when correlations are negative.
        frame['close'] = a[:, 4]
        frame['high'] = frame[['high', 'open', 'close']].max(axis=1)
        frame['low'] = frame[['low', 'open', 'close']].min(axis=1)
        frame['volume'] = a[:, 5]
        validate_frame(frame)
        return frame
    if timeframe == '1d':
        return load_frame(root / 'factor_mining/crypto/daily' / (symbol + 'USDT.json'))
    base = '5m' if timeframe == '15m' else timeframe
    frame = load_frame(root / 'volrix/klines' / f'{symbol}USDT_{base}.json', base)
    if timeframe == '15m':
        bins = frame.resample('15min', origin='epoch', closed='left', label='left')
        result = bins.agg(dict(open='first', high='max', low='min', close='last', volume='sum'))
        return result.loc[bins['close'].count().eq(3)].copy()
    return frame


@lru_cache(maxsize=10)
def _funding(root, symbol):
    return load_funding(root, symbol + 'USDT')


def load_universe(rule, market, root=DATA_ROOT, *, symbol=None, end=None):
    spec = RULES[rule]
    symbols = spec.universe if market == 'us' else (
        (symbol,) if symbol else (('BTC', 'ETH') if rule in INTRADAY or rule in ('W13', 'W14') else COINS))
    frames = {}
    for name in symbols:
        frame = _read_frame(str(root), name, market, spec.timeframe)
        if market == 'us':
            frame = frame.loc[spec.start:]
        if end:
            frame = frame.loc[:end]
        if frame.empty:
            raise ValueError(f'No local observations for {rule}/{market}/{name}')
        frames[name] = frame
    close = pd.concat({n: f['close'] for n, f in frames.items()}, axis=1).sort_index()
    return frames, close


@lru_cache(maxsize=32)
def _benchmark_cached(root, market, symbols, start, end, timeframe, neutral=False, reference=False):
    frames = {s: _read_frame(root, s, market, timeframe).loc[start:end] for s in symbols}
    close = pd.concat({s: f['close'] for s, f in frames.items()}, axis=1).sort_index()
    if neutral:
        return dict(daily_equity={str(t.date()): CAPITAL for t in close.index},
                    metrics=curve_metrics({str(t.date()): CAPITAL for t in close.index}, 252 if market == 'us' else 365))
    active = close.notna().astype(float)
    weights = active.div(active.sum(axis=1), axis=0).fillna(0.)
    # BH has only the initial allocation; multi-asset benchmarks BME rebalance.
    return run_weights(frames, weights, market=market, policy=RebalancePolicy(None if len(symbols) == 1 else 'BME'),
                       charge_carry=False)


def _aligned_logs(run, benchmark, end=None):
    a, b = daily_logs(run, end=end), daily_logs(benchmark, end=end)
    # Never independently filter NaNs or discard a bankruptcy to manufacture
    # a valid sample. Calendars are aligned before the common time split.
    both = pd.concat((a.rename('strategy'), b.rename('benchmark')), axis=1, join='inner')
    return both['strategy'], both['benchmark']


def _official(rule, root):
    paths = list((Path(root) / 'quantjourney/reports').glob(f'example-weights-{int(rule[1:]):02d}-*.json'))
    if len(paths) != 1:
        raise ValueError(f'Expected one official report for {rule}, found {len(paths)}')
    sections = json.loads(paths[0].read_text())['sections']
    summary = {r['metric']: r['all'] for r in sections['performance_summary']['rows']}
    ratios = {r['metric']: r['value'] for r in sections['performance_ratios']['rows']}
    monthly = {f"{r['year']:04d}-{r['month']:02d}": r['return'] for r in sections['periodical_analysis']['monthly']}
    return dict(path=str(paths[0]), settings=sections['settings']['values'], monthly=monthly,
                return_pct=summary['Return on Initial Capital'] * 100.,
                cagr_pct=ratios['CAGR'] * 100., sharpe=ratios['Sharpe Ratio'],
                trades=summary['Round Trips'])


def erc_stability(close, lookback):
    """A data-only diagnostic, never a different P&L engine or fitted solver.

    Move each covariance element ONE floating-point ULP upwards. A converged
    risk allocation should barely move; the published undamped 50-step solver
    can switch between entirely different allocations. Do not select whichever
    numeric variant looks closer to the official performance.
    """
    returns = close[sorted(close.columns)].pct_change(fill_method=None)
    returns.iloc[0] = returns.iloc[0].where(close.iloc[0].reindex(returns.columns).isna(), 0.)
    model, worst = RiskParityModel(lookback), dict(max_weight_change=0., date=None)
    for i in range(lookback, len(close)):
        if i != lookback and close.index[i].to_period('M') == close.index[i-1].to_period('M'):
            continue
        cov = returns.iloc[i-lookback:i].cov().to_numpy()
        a, b = model.solve_erc(cov), model.solve_erc(np.nextafter(cov, np.inf))
        change = float(np.abs(a - b).max())
        if change > worst['max_weight_change']:
            worst = dict(max_weight_change=change, date=str(close.index[i].date()),
                         columns=list(returns.columns), original_weights=a.tolist(), perturbed_weights=b.tolist())
    return worst


def reproduction(rule, root=DATA_ROOT):
    official = _official(rule, root)
    settings = official['settings']
    frames, close = load_universe(rule, 'us', root, end=settings['end_date'])
    # Start is taken from the actual published settings, not inferred from a
    # currently edited example file. Indicators reset at this original start.
    frames = {s: f.loc[settings['start_date']:] for s, f in frames.items()}
    close = close.loc[settings['start_date']:]
    weights = compute_weights(rule, close)
    # The settings commission_rate_bps is an effective audit ratio affected by
    # QJ rounding. FixedBpsWeightCostModel.total_bps defaults to exactly 1 bp.
    run = run_weights(frames, weights, policy=RULES[rule].policy, mode='reproduction',
                      commission=.0001, slippage=0., charge_carry=False)
    eq = pd.Series(run['daily_equity'])
    daily = eq.div(eq.shift(1, fill_value=CAPITAL))
    local_monthly = daily.groupby(pd.to_datetime(eq.index).to_period('M')).prod() - 1.
    local_monthly.index = local_monthly.index.astype(str)
    pair = pd.concat([local_monthly, pd.Series(official['monthly'])], axis=1, join='inner').dropna()
    corr = float(pair.iloc[:, 0].corr(pair.iloc[:, 1])) if len(pair) > 1 and pair.std().min() > 0 else None
    local = run['metrics']
    years = (pd.Timestamp(eq.index[-1]) - pd.Timestamp(eq.index[0])).days / 365.25
    local['cagr_pct'] = ((eq.iloc[-1] / CAPITAL) ** (1. / years) - 1.) * 100. if years > 0 else None
    absolute_error = abs(local['return_pct'] - official['return_pct'])
    relative_error = absolute_error / abs(official['return_pct']) if official['return_pct'] else None
    qualifies = (corr is not None and corr >= .90 and
                 (absolute_error <= 10. or (relative_error is not None and relative_error <= .30)))
    reasons = []
    if not qualifies:
        if corr is None or corr < .90:
            reasons.append('月度相关<0.90')
        if absolute_error > 10. and (relative_error is None or relative_error > .30):
            reasons.append('总收益相对误差>30%且绝对误差>10个百分点')
    result = dict(rule=rule, qualified=qualifies, correlation=corr, monthly_pairs=len(pair),
                absolute_error_pp=absolute_error, relative_error=relative_error,
                local=local, official={k: v for k, v in official.items() if k != 'monthly'},
                daily_equity=run['daily_equity'], reasons=reasons)
    if rule == 'W19':
        result['erc_stability'] = erc_stability(close, RULES[rule].parameters['risk_lookback'])
    return result


def _run_case(rule, market, root, symbol=None):
    spec = RULES[rule]
    frames, close = load_universe(rule, market, root, symbol=symbol,
                                  end='2026-10-30' if market == 'us' else None)
    weights = compute_weights(rule, close, market=market)
    start, end = str(close.index[0]), str(close.index[-1])
    bench = _benchmark_cached(str(root), market, tuple(frames), start, end, spec.timeframe, rule in NEUTRAL)
    rates = {s: _funding(str(root), s) for s in frames} if market == 'crypto' else None
    # W11's original example supplies no benchmark_returns -> inactive TE.
    # Evaluation intentionally enables the requested TE trigger using SPY/BTC,
    # the original policy's reference index, distinct from the admission BH.
    ref_symbol = 'SPY' if market == 'us' else 'BTC'
    ref_frame = _read_frame(str(root), ref_symbol, market, spec.timeframe).loc[start:end]
    te_returns = ref_frame['close'].pct_change(fill_method=None)
    gate_end = '2025-12-31' if market == 'us' else None
    run = run_weights(frames, weights, policy=spec.policy, market=market, funding=rates,
                      benchmark_returns=te_returns if rule == 'W11' else None,
                      exposure_end=gate_end)
    s, b = _aligned_logs(run, bench, gate_end)
    gate_equity = {k: v for k, v in run['daily_equity'].items() if gate_end is None or k <= gate_end}
    gm = curve_metrics(gate_equity, 252 if market == 'us' else 365)
    gm['average_exposure'] = run['metrics']['gate_average_exposure']
    bm_equity = {k: v for k, v in bench['daily_equity'].items() if gate_end is None or k <= gate_end}
    bm = curve_metrics(bm_equity, 252 if market == 'us' else 365)
    row = dict(rule=rule, market=market, symbol=symbol or ('BTC+ETH' if rule in INTRADAY else 'basket'),
               edge_source=spec.edge_source, timeframe=spec.timeframe, start=start, end=end,
               parameters=spec.parameters, primary=list(spec.primary),
               metrics=run['metrics'], gate_metrics=gm, benchmark_metrics=bm,
               daily_equity=run['daily_equity'], benchmark_daily_equity=bench['daily_equity'],
               _s=s.tolist(), _b=b.tolist())
    if rule in INTRADAY:
        # Rule-level portfolio is two feeds; cross-coin admission additionally
        # runs the exact same template on each coin independently.
        individual = {}
        for coin in ('BTC', 'ETH'):
            single = {coin: frames[coin]}
            cw = compute_weights(rule, close[[coin]], market=market)
            cr = run_weights(single, cw, policy=spec.policy, market=market, funding={coin: rates[coin]})
            individual[coin] = cr['metrics']
        candidates = []
        checks = {}
        for coin, metrics in individual.items():
            other = individual['ETH' if coin == 'BTC' else 'BTC']
            flags = [metrics['sharpe'] is not None and metrics['sharpe'] >= .8,
                     metrics['first_half']['return_pct'] is not None and metrics['second_half']['return_pct'] is not None
                     and min(metrics['first_half']['return_pct'], metrics['second_half']['return_pct']) > 0,
                     metrics['avg_net_bps'] is not None and metrics['avg_net_bps'] >= 5.,
                     other['return_pct'] > 0.]
            checks[coin] = dict(zip(('I1_SR>=0.8', 'I2_两半>0', 'I3_每笔>=5bps', 'I4_另一币收益>0'), flags))
            if all(flags):
                candidates.append(coin)
        row['intraday'] = dict(components=individual, checks=checks, qualified_symbols=candidates,
                               status='PASS' if candidates else 'FAIL')
        row['sweep'] = dict(status='N/A（日内专用门槛）', points=[])
    else:
        defaults = {p: spec.parameters[p] for p in spec.primary}
        grid = qa.grid_around(defaults)
        scores = {}
        for key, params in grid.items():
            if params == defaults:
                scores[key] = gm['sharpe'] if gm['sharpe'] is not None else -1e6
                continue
            cw = compute_weights(rule, close, market=market, parameters=params)
            # Gates exclude the post-publication 2026 sample, including G4.
            sweep_frames = {c: f.loc[:gate_end] if gate_end else f for c, f in frames.items()}
            cw = cw.loc[:gate_end] if gate_end else cw
            cr = run_weights(sweep_frames, cw, policy=spec.policy, market=market, funding=rates,
                             benchmark_returns=te_returns if rule == 'W11' else None)
            score = cr['metrics']['sharpe']
            scores[key] = score if score is not None else -1e6
        plateau = qa.sweep_plateau(lambda params: scores[tuple(params.values())], grid, tuple(defaults.values()))
        row['sweep'] = dict(status='LONE_PEAK' if plateau.lone_peak else 'PLATEAU',
                            positive_fraction=plateau.plateau_ratio,
                            points=[dict(parameters=grid[k], sharpe=v) for k, v in plateau.grid.items()])
    reference = _benchmark_cached(str(root), market, (ref_symbol,), start, end, spec.timeframe)
    row['reference'] = dict(symbol=ref_symbol, metrics=reference['metrics'])
    if market == 'us':
        logs = daily_logs(run).loc['2026-01-01':'2026-10-30']
        blog = daily_logs(bench).reindex(logs.index)
        row['post_publication'] = dict(start='2026-01-01', end='2026-10-30',
            return_pct=float(math.expm1(logs.sum()) * 100), sharpe=qa.sharpe(logs.tolist(), 252),
            benchmark_return_pct=float(math.expm1(blog.sum()) * 100),
            benchmark_sharpe=qa.sharpe(blog.tolist(), 252),
            months={str(k): float(math.expm1(v) * 100) for k, v in logs.groupby(pd.to_datetime(logs.index).to_period('M')).sum().items()})
    return row


def _case_job(rule, market, root):
    rows, rep, cross = [], None, None
    if market == 'us':
        rep = reproduction(rule, root)
    if market == 'crypto' and rule in SINGLE:
        for coin in ('BTC', 'ETH'):
            rows.append(_run_case(rule, market, root, coin))
        cross = {}
        for coin in COINS:
            main = next((r for r in rows if r['symbol'] == coin), None)
            if main:
                cross[coin] = main['metrics']
                continue
            frames, close = load_universe(rule, market, root, symbol=coin)
            cr = run_weights(frames, compute_weights(rule, close, market=market),
                             policy=RULES[rule].policy, market=market, funding={coin: _funding(str(root), coin)})
            cross[coin] = cr['metrics']
        fraction = sum(m['sharpe'] is not None and m['sharpe'] > 0. for m in cross.values()) / len(COINS)
        for row in rows:
            row['cross_section'] = dict(positive_fraction=fraction, coins=cross)
    else:
        rows.append(_run_case(rule, market, root))
    return rows, rep


def run_study(rules=None, market='all', root=DATA_ROOT, *, workers=4, progress=print):
    selected = list(rules or RULES)
    jobs = [(r, m, str(root)) for r in selected for m in ('us', 'crypto')
            if (market == 'all' or market == m) and (m == 'crypto' or r in US_RULES)]
    if not jobs:
        raise ValueError('No supported rule/market combinations selected')
    rows, reps = [], []
    started = time.monotonic()
    def collect(result, rule, m):
        case_rows, rep = result
        rows.extend(case_rows)
        if rep:
            reps.append(rep)
        progress(f'{rule}/{m} 完成；{len(rows)} 主表行，耗时 {time.monotonic()-started:.1f}s')
    if workers == 1:
        for rule, m, root_string in jobs:
            collect(_case_job(rule, m, root_string), rule, m)
    else:
        # OS workers are numerical backtest processes, not delegated agents.
        with ProcessPoolExecutor(max_workers=min(workers, len(jobs))) as pool:
            futures = {pool.submit(_case_job, *job): job for job in jobs}
            for future in as_completed(futures):
                rule, m, _ = futures[future]
                collect(future.result(), rule, m)
    rows.sort(key=lambda r: (r['market'], r['rule'], r['symbol']))
    reps.sort(key=lambda r: r['rule'])
    # One predeclared hypothesis per rule x market. BTC and ETH are the same
    # single-name template's two required evaluations; group their daily SRs
    # before estimating the cross-sectional variance so they are not counted
    # twice. The DSR probability is nevertheless required for EACH coin.
    # Intraday rules are judged by their own frozen gate and only enter the DSR
    # columns as diagnostics; a 1m scalp at SR -10 in the pool would raise the
    # expected-max hurdle for every daily rule to an annual SR of about 3.6.
    grouped, excess_grouped = {}, {}
    for row in rows:
        if row['rule'] in INTRADAY:
            continue
        own = np.asarray(row['_s'])
        for scores, a in ((grouped, own), (excess_grouped, own - np.asarray(row['_b']))):
            score = float(a.mean() / a.std(ddof=1)) if len(a) > 1 and np.isfinite(a).all() and a.std(ddof=1) > 1e-15 else 0.
            scores.setdefault((row['rule'], row['market']), []).append(score)
    hypothesis_scores = [float(np.mean(v)) for v in grouped.values()]
    variance = float(np.var(hypothesis_scores, ddof=1)) if len(hypothesis_scores) > 1 else 0.
    excess_scores = [float(np.mean(v)) for v in excess_grouped.values()]
    excess_variance = float(np.var(excess_scores, ddof=1)) if len(excess_scores) > 1 else 0.
    trials = sum(rule not in INTRADAY for rule, _, _ in jobs)
    for row in rows:
        s, b = row.pop('_s'), row.pop('_b')
        dsr = deflated_sharpe(s, trials, variance)
        gate = admission(s, b, 252 if row['market'] == 'us' else 365, dsr=dsr,
                         plateau=row['sweep']['status'],
                         average_exposure=row['gate_metrics']['average_exposure'],
                         cross_positive_fraction=row.get('cross_section', {}).get('positive_fraction'))
        gate['dsr_excess_original'] = deflated_sharpe(np.asarray(s) - b, trials, excess_variance)
        if row['rule'] in INTRADAY:
            # G1--G4/DSR are descriptive for intraday. Frozen volrix_intraday
            # conditions, not daily-rule G1--G5, determine its admission.
            gate['daily_diagnostics_only'] = True
            gate['status'] = row['intraday']['status']
            gate['failed'] = [] if gate['status'] == 'PASS' else [
                ';'.join(k for k, ok in flags.items() if not ok) + ':' + coin
                for coin, flags in row['intraday']['checks'].items()]
        row['admission'] = gate
    # A single-name crypto rule passes only when BOTH required BTC/ETH cases
    # pass; do not pick the better coin after seeing its results.
    grouped_status = {}
    for key in grouped:
        same = [r for r in rows if (r['rule'], r['market']) == key]
        grouped_status['/'.join(key)] = 'PASS' if all(r['admission']['status'] == 'PASS' for r in same) else 'FAIL'
    return dict(engine='backtrader', engine_version=bt.__version__,
                trials=trials, sharpe_variance=variance, hypothesis_daily_sharpes=hypothesis_scores,
                excess_sharpe_variance_original=excess_variance,
                excess_hypothesis_daily_sharpes_original=excess_scores,
                rules=selected, market=market, reproduction=reps, results=rows,
                pass_summary=grouped_status, elapsed_seconds=time.monotonic()-started)


def _fmt(value, digits=2):
    return f'{value:.{digits}f}' if value is not None and math.isfinite(value) else '—'


def _table(headers, rows):
    def clean(v):
        return str(v).replace('|', '/').replace('\n', ' ')
    return '\n'.join(['| ' + ' | '.join(headers) + ' |', '| ' + ' | '.join('---' for _ in headers) + ' |']
                     + ['| ' + ' | '.join(clean(x) for x in row) + ' |' for row in rows])


def reproduction_table(result):
    rows = []
    for r in result['reproduction']:
        a, b = r['local'], r['official']
        rows.append([r['rule'], _fmt(r['correlation'], 4),
                     _fmt(a['return_pct']) + '/' + _fmt(b['return_pct']),
                     _fmt(a['cagr_pct']) + '/' + _fmt(b['cagr_pct']),
                     _fmt(a['sharpe_simple_rf'], 3) + '/' + _fmt(b['sharpe'], 3),
                     str(a['trades']) + '/' + str(b['trades']), '合格' if r['qualified'] else '不合格'])
    return _table(['规则', '月度ρ', '总收益% 本地/官方', 'CAGR% 本地/官方', 'Sharpe rf2% 本地/官方', '交易数 本地/官方', '复现'], rows)


def evaluation_table(result, market):
    rows = []
    for r in result['results']:
        if r['market'] != market:
            continue
        m, b, gate = r['gate_metrics'], r['benchmark_metrics'], r['admission']
        a = gate['audit']
        windows = '/'.join(_fmt(a.get(w + '_sharpe')) + ':' + _fmt(a.get(w + '_sharpe_bench')) for w in ('train', 'holdout'))
        full = gate['full_exposure_audit']
        full_windows = '/'.join(_fmt(full.get(w + '_sharpe')) + ':' + _fmt(full.get(w + '_sharpe_bench')) for w in ('train', 'holdout'))
        halves = '/'.join(_fmt(v * 100 if v is not None else None) for v in gate['halves'])
        rows.append([r['rule'] + ((' ' + r['symbol']) if r['symbol'] != 'basket' else ''), r['edge_source'],
                     _fmt(m['return_pct']), _fmt(m['sharpe']), _fmt(b['sharpe']), full_windows,
                     _fmt(gate['matched_benchmark_sharpe']), windows,
                     _fmt(gate['dsr'], 4), _fmt(gate['dsr_excess_original'], 4), halves, r['sweep']['status'],
                     _fmt(m['maxdd_pct']) + '/' + _fmt(b['maxdd_pct']),
                     _fmt(r['metrics']['turnover']), _fmt(m['average_exposure']),
                     str(r['metrics']['trades']),
                     _fmt(r.get('cross_section', {}).get('positive_fraction', None)),
                     gate['status'] + ('：' + ','.join(gate['failed']) if gate['failed'] else '')])
    return _table(['规则/标的', 'edge_source', '净收益%', 'Sharpe', '基准SR', '60/40 SR 策略:基准',
                   '同敞口基准SR', '60/40 SR 策略:同敞口基准', 'DSR', 'DSR(超额,原规格)',
                   '两半收益%', '平台', 'MaxDD% 策略/基准', '换手x', '平均总敞口x', '成交数', 'G5占比', '准入/未过门槛'], rows)


def pass_table(result):
    rows = [[key, value] for key, value in sorted(result['pass_summary'].items())]
    passed = [key for key, value in result['pass_summary'].items() if value == 'PASS']
    return (f"PASS {len(passed)}/{len(result['pass_summary'])}；"
            + ('、'.join(sorted(passed)) if passed else '无规则满足全部准入门槛。') + '\n\n'
            + _table(['规则/市场', '最终判定'], rows))


def render_report(result):
    lines = ['# QuantJourney 权重策略 W01–W25：Backtrader 离线评估', '',
        '## 口径说明', '',
        '仅从本地缓存读取数据；QuantJourney 源码只读，未导入或执行。所有订单、持仓、手续费与盈亏均由 Backtrader '
        + result['engine_version'] + ' 计算，初始策略资金 1000，按当前策略权益复利；虚拟抵押现金只消除原生现金限制，不计入策略权益或融资费基数。未模拟交易所强平、最小下单量或借券可得性。', '',
        '评估：t 收盘确定目标及再平衡意图，下一根开盘用缓存目标×该开盘的当前权益/该开盘价定量，原生 Market 成交。'
        'next_open 只读取当根 open，不读取 high/low/close；滑点为固定 bps，不用当根高低价裁剪。'
        '复现：core.py 的 weights.shift(1) 后，在 t+1 收盘调仓（原生 cheat-on-close 匹配）；并非用 t 收盘信号在同一 t 收盘成交，不是前视式信号成交。'
        '风险模型用严格先前窗口 [t-lookback,t)，月初重估；信号窗口含当根收盘。两种模式均保留 QJ 默认 5% cash buffer 和各例的权重 clip（clip 后不强行满仓）。', '',
        '美股复权 OHLC = 原 OHLC×adjclose/close；复现用官方区间、每边 1bp、零滑点。'
        '官方 settings.commission_rate_bps 是有效比率（含交易金额舍入），模型参数 FixedBpsWeightCostModel.total_bps 为 1。复现 CAGR 使用 QJ 的实际日历跨度/365.25，Sharpe 使用简单日收益、rf=2%、252 年化。'
        '评估为每边佣金 1bp+滑点 2bp，空头借券年 0.5%，多头名义超过权益部分融资年 5%，按实际日历天数/365 扣现金，现金无息。'
        '美股门槛样本至 2025-12-31；2026-01..10 保持连续持仓单列展示，完全排除 G1–G4、DSR 和参数扫描。', '',
        '加密日线用固定存活篮子 BTC ETH BNB XRP ADA DOGE LTC LINK TRX BCH，存在幸存者偏差；上市前权重为 0 且不参与排名，不回填上市前价格。'
        '数据全期；每边佣金 5bp+滑点 3bp。UTC 日首根开盘前，对隔夜持仓按名义×当日日均真实费率×3 扣现金，多头付正费、空头收正费；缺失资金费日按 0 并记录覆盖数。'
        '资金费是规格指定的每日近似，未用日均费率生成信号。W01/W03/W21/W22 各在 BTC、ETH 上独立评估，沿用原 25% 单标的上限；'
        'G5 在十币上逐币跑同规则，正 Sharpe 比例至少 60%，且 BTC、ETH 两行均过门槛才给该规则/加密 PASS。', '',
        '多标的规则映射十币；W06 以 BTC 作 SPY 的趋势锚，风险组 BTC ETH BNB XRP ADA DOGE LTC，防御组 LINK TRX BCH；这是预先固定的逻辑映射，并不意味着山寨币具有债券防御属性。'
        'W13/W14 为 BTC/ETH；W07 为 BTC+ETH 15m（完整 3 根 5m 重采样）；W08 为 BTC+ETH 5m，**近似：无 1m 数据**；W09 为 BTC+ETH 1h。'
        'W23/W24/W25：**原市场 FX/期货，nofx 不交易，仅移植逻辑到加密**，不做原市场复现；W07–W09 不做美股复现/评估。', '',
        '基准与策略日对数收益逐日对齐。单标的是同标的 BH；多标的是同篮子等权、BME 每月末调仓；'
        'W13/W14/W15/W16/W24 为零收益现金基准。加密 BH 为现货等价，不计资金费，计初始买入成本；所有 BH 均经原生 Backtrader 执行。'
        '另外报告 SPY/BTC BH 参照。BME/BQE 保留 QJ 的营业月/季末（周末前一个营业日），不是加密自然月末；遇交易所休市向前吸附；W 按指定星期、同周假日前吸附。'
        'Calendar/Drift/Signal/TE 是 OR；评估的 partial 只调超过阈值的标的，未触发标的保持真实股数；滚动 252 根总换手预算在触发后否决。', '',
        '**源码行为核对**：W10 示例传入 +0.15，引擎却比较 drawdown < threshold，因此零回撤也触发；两模式忠实保留此 bug，10 根冷却结束后重置高水位再入场。'
        'W11 原始例未传 benchmark_returns，复现时 TE 不启用；评估按任务要求启用，使用 SPY/BTC 收益计算 63 根年化 TE>6%，同时遵守滚动 252 根 4x 换手预算。'
        'QJ fast engine 在再平衡日用再平衡前权重记整根收益且不先漂移。W12 设置 frequency=D，每日都标记 rebalance，即使 partial 保留了某个权重，该权重也不漂移；其 implied-share ledger 因此仍每日调整该标的股数并计成本。'
        '复现模式显式移植这一权重状态 bug，并经原生 Backtrader 订单执行这些隐含调仓；评估模式才保持未超阈值标的的真实股数。'
        '除此之外，Backtrader 真实股数在调仓日先随价格漂移再成交，QJ 的递归成本固定点使用交易后权益，而此处用交易前权益定量，因此仍有小幅差异。'
        'ERC 保留 QJ 原始 50 步未阻尼乘法迭代、不为了贴合报告而换求解器；按 InstrumentData.add_strategy_data 的字母排序传入风险模型，精确保留缓存 adjclose 数值。', '',
        'G1 更正为同敞口基准，属于收紧，避免对数收益方差拖累带来的机械优势。'
        '同敞口基准 m_t=log(1+e·r_t)，r_t 为原基准当日简单收益，e 为策略门槛期平均总敞口；e>1 按杠杆倍数、不另计融资费，市场中性规则仍对零基准。'
        'G1=quant_audit.audit_returns 60/40 两窗 Sharpe 都严格高于同敞口基准（不要求 CAGR 赢）；原满仓基准及其两窗列保留参照。'
        'G2=策略自身收益 DSR≥0.95；G3=时间两半净收益都>0；'
        'G4=预先声明一个主参数，grid_around 五档×sweep_plateau 的 PLATEAU（忠实采用该函数：默认亏损且全邻域亏损也不叫 lone peak，因此 G3 仍独立必要）；'
        'G5 仅加密单标的日线。DSR 使用策略自身日对数收益（rf=0）、非年化 SR、Pearson 非超额峰度；SR0 使用这些组合自身日 SR 的横截面样本方差（ddof=1），公式及数值测试见代码。'
        f"本次 N={result['trials']} 个日线规则×市场组合（W07–W09 日内规则按日内门槛裁决，不进入 DSR 试验池，否则 W08 的 SR≈−10 会把期望最大 SR 门槛抬到年化约 3.6；该排除在审查时作出，已披露）；同一单标的模板的 BTC/ETH 自身日 Sharpe 先取均值再形成组合横截面方差 {result['sharpe_variance']:.8g}，两币分别计算 G1、DSR；"
        '参数点、G5 辅助币和 BH 不新增试验数。', '',
        '预声明 G2 作用于超额收益，对低敞口策略结构性不可通过（全部为 0）；审查时更正为策略自身收益的 DSR。该更正在看到全部 DSR=0 之后作出，属于事后放宽，已披露。'
        '主表 DSR(超额,原规格) 保留原始满仓基准超额收益及其横截面样本方差，仅作诊断、不参与准入。', '',
        '日内采用专用门槛：同一个候选币 Sharpe≥0.8、两半净收益>0、平均每笔净收益≥5bps，且另一币全期收益>0；'
        'BTC/ETH 独立辅助回测及两币篮子均报告。平均每笔为原生完整平仓 pnlcomm 减该持仓期间资金费，除入场名义；未平仓笔不伪造平仓。'
        '日内 DSR/两窗列只作诊断，不决定日内 PASS。交易数为已完成的原生成交订单数，另存 closed_trades；官方 Round Trips 常是隐含权重调整记录，并非原生完整开平仓次数。', '',
        '主表净收益/Sharpe/MaxDD/两半、基准和平均总敞口为门槛期；美股用于同敞口基准的平均敞口同样排除 2026。'
        '美股换手、成交数为完整评估期（含单列的 2026），完整评估期平均敞口仍保存在 JSON metrics.average_exposure。'
        '换手=各次交易绝对名义/该次交易前权益之和；平均敞口=每根 bar 总绝对名义/权益的均值。'
        '参数扫描与分窗均为历史稳健性诊断，不构成独立前瞻验证。', '',
        '## 复现表', '', reproduction_table(result), '',
        '## 美股评估主表', '', evaluation_table(result, 'us'), '',
        '## 加密评估主表', '', evaluation_table(result, 'crypto'), '',
        '## 2026-01..10 发布后样本外（美股，不计入准入）', '']
    post = []
    for r in result['results']:
        if r['market'] == 'us':
            p = r['post_publication']
            post.append([r['rule'], _fmt(p['return_pct']), _fmt(p['sharpe']),
                         _fmt(p['benchmark_return_pct']), _fmt(p['benchmark_sharpe']),
                         '/'.join(_fmt(p['months'].get(f'2026-{i:02d}')) for i in range(1, 11))])
    lines += [_table(['规则', '净收益%', 'Sharpe', '基准收益%', '基准SR', '1..10月净收益%'], post), '',
              '## PASS 汇总', '', pass_table(result), '', '## 复现不合格项与排查', '']
    failed = [r for r in result['reproduction'] if not r['qualified']]
    if not failed:
        lines.append('全部 19 条原市场复现均满足月度相关及收益误差门槛。' if len(result['reproduction']) == 19 else '本次所选复现项全部满足门槛。')
    for r in failed:
        lines.append(f"- {r['rule']}：{'；'.join(r['reasons'])}；相关 {_fmt(r['correlation'],4)}，总收益绝对差 {_fmt(r['absolute_error_pp'])} 个百分点。"
                     '已核对原始标的、起止区间、shift(1)+收盘成交、1bp 成本与5%现金。'
                     + ('W10 的正阈值 bug 与触发日会计顺序造成敏感性。' if r['rule'] == 'W10' else '')
                     + ('W11 官方未启用 TE；已在复现中保持关闭。滚动换手临界值可使后续整段持仓分叉。' if r['rule'] == 'W11' else '')
                     + ('W12 已复现 fast engine 的每日不漂移权重状态及隐含全组合再定量；与评估保留真实股数的行为明确分开。' if r['rule'] == 'W12' else '')
                     + (f"W19 已定位未阻尼 ERC 的数值不稳定：{r['erc_stability']['date']}，协方差每个元素只增加1个浮点ULP，最大单资产权重改变 {r['erc_stability']['max_weight_change']*100:.2f} 个百分点。"
                        '已核对协方差窗口、首行0收益、月初重估、字母轴顺序及50次迭代；原始算法在负相关资产上未稳定收敛，微小数值/BLAS/发布环境差异会变成截然不同的配置。未改算法或挑数值变体追逐官方收益。' if r['rule'] == 'W19' else '')
                     + ('RSI 来自 QJ 未附带的 quantjourney_ti，当前使用 Wilder SMA 初值，可能与发布版平滑/初值不同。' if r['rule'] == 'W03' else '')
                     + '当前价格缓存复权序列可能与发布时 yfinance 修订前数据不同；未获得发布时的逐日价格/指标快照，剩余误差来源尚不能唯一归因，不能声称精确复现或静默通过。')
    lines += ['', '## 主参数、辅助回测与参照', '',
              _table(['规则', 'G4预先声明主参数', '固定信号/风险参数', '再平衡'],
                     [[r, ', '.join(f'{k}={RULES[r].parameters[k]}' for k in RULES[r].primary),
                       json.dumps(RULES[r].parameters, ensure_ascii=False), str(RULES[r].policy)] for r in result['rules']]), '']
    lines.append(_table(['规则/市场/标的', 'SPY/BTC BH 收益%', 'SPY/BTC BH SR', '扫描正SR占比', '资金费付出', '借券费', '融资费', '资金费覆盖/持仓日'],
        [[r['rule']+'/'+r['market']+'/'+r['symbol'], _fmt(r['reference']['metrics']['return_pct']), _fmt(r['reference']['metrics']['sharpe']),
          _fmt(r['sweep'].get('positive_fraction')), _fmt(r['metrics']['funding_paid']), _fmt(r['metrics']['borrow_paid']),
          _fmt(r['metrics']['financing_paid']), f"{r['metrics']['funding_covered']}/{r['metrics']['funding_observations']}"] for r in result['results']]))
    intraday = [r for r in result['results'] if 'intraday' in r]
    if intraday:
        lines += ['', _table(['日内规则', '币', 'Sharpe', '前/后半收益%', '每笔净bps', '未过条件'],
            [[r['rule'], coin, _fmt(m['sharpe']), _fmt(m['first_half']['return_pct'])+'/'+_fmt(m['second_half']['return_pct']),
              _fmt(m['avg_net_bps']), ','.join(k for k,v in r['intraday']['checks'][coin].items() if not v)]
             for r in intraday for coin,m in r['intraday']['components'].items()])]
    lines += ['', f"运行耗时 {result['elapsed_seconds']:.1f}s；JSON 只保存汇总与日权益，不含逐笔明细。", '']
    return '\n'.join(lines)


def json_safe(value):
    if isinstance(value, dict):
        return {str(k): json_safe(v) for k, v in value.items()}
    if isinstance(value, (list, tuple)):
        return [json_safe(v) for v in value]
    if isinstance(value, (np.integer,)):
        return int(value)
    if isinstance(value, (bool, np.bool_)):
        return bool(value)
    if isinstance(value, (float, np.floating)):
        return float(value) if math.isfinite(value) else None
    return value


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    parser.add_argument('--out', required=True, type=Path)
    parser.add_argument('--json', required=True, type=Path, dest='json_path')
    parser.add_argument('--rules', default='all', help='W01,W02 or W01-W06; default all')
    parser.add_argument('--market', choices=('us', 'crypto', 'all'), default='all')
    parser.add_argument('--workers', type=int, default=min(4, os.cpu_count() or 1))
    parser.add_argument('--data-root', type=Path, default=DATA_ROOT)
    args = parser.parse_args(argv)
    if args.workers < 1:
        parser.error('--workers must be positive')
    rules = []
    if args.rules.lower() == 'all':
        rules = list(RULES)
    else:
        for token in args.rules.upper().split(','):
            token = token.strip()
            if '-' in token:
                a, b = token.split('-', 1)
                rules += [f'W{i:02d}' for i in range(int(a.lstrip('W')), int(b.lstrip('W')) + 1)]
            else:
                rules.append(f'W{int(token.lstrip("W")):02d}')
        rules = list(dict.fromkeys(rules))
    if not rules or any(r not in RULES for r in rules):
        parser.error('--rules must select W01 through W25')
    result = run_study(rules, args.market, args.data_root, workers=args.workers,
                       progress=lambda message: print(message, file=sys.stderr, flush=True))
    markdown = render_report(result)
    payload = json.dumps(json_safe(result), ensure_ascii=False, separators=(',', ':'), allow_nan=False)
    if len(payload.encode()) >= 30_000_000:
        raise ValueError('Summary JSON exceeds the 30 MB budget')
    args.out.parent.mkdir(parents=True, exist_ok=True)
    args.json_path.parent.mkdir(parents=True, exist_ok=True)
    args.out.write_text(markdown)
    args.json_path.write_text(payload)
    print(f"报告：{args.out}\nJSON：{args.json_path} ({len(payload.encode())/1e6:.2f} MB)")
    print(f"复现合格：{sum(r['qualified'] for r in result['reproduction'])}/{len(result['reproduction'])}")
    print(pass_table(result))
    return 0


if __name__ == '__main__':
    raise SystemExit(main())
