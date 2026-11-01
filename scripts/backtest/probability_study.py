"""离线概率研究：鞅性、双障碍命中与 Kelly 增长；不产生交易信号。

研究动机：概率论的可选停止定理说明，若对数价格（扣除成本后）是
鞅/上鞅，则任何有界的停止规则、止盈止损组合、加减仓方案都无法产生
正期望。因此“优势”只能来自市场对鞅性质的偏离。本模块先检验偏离
是否存在（P1、P2），再用 Kelly 定理分析杠杆/仓位对增长率的影响（P3）。
这里的结论以可选停止、可积性、可预测且受约束的暴露等条件为前提；
对数价格鞅不等同于价格鞅，拒绝某项原假设也不证明存在可交易优势。

预声明：日线 q={2,5,10,20,60}，小时 q={2,4,8,24,72,168}；各序列
前60%/后40%；全样本全部 P1 检验统一 Holm 校正，全部 P2（资产、频率、
方向、障碍对）对无漂移理论的检验另统一 Holm 校正。显著且稳定须全样本
Holm p<0.05，前后偏离同号且各自 |z|>1.96；P1 使用异方差稳健 z*。
篮子单侧显著比例使用未校正双侧 p<0.05，预期假阳性率2.5%/侧。
障碍对固定为(1,1),(2,1),(1,2),(3,1),(1,3),(2,2)；入场间隔日线5根、
小时24根，最长30自然日；波动仅用入场前30自然日的日对数收益。
所有检验及 P3 的样本截止2025-12-31，2026起只描述，绝不参与估计或检验。
按自然月整块有放回 bootstrap 默认1000次，固定种子20261031；正态尾概率
用 math.erfc，无 scipy、网络或安装步骤。门槛不会根据结果更改。

VR 使用 Lo–MacKinlay (1988) 无偏重叠估计量，n 为一期收益数，
m=q(n-q+1)(1-q/n)。本实现 delta_j=Σe_t²e_(t-j)²/(Σe_t²)²，
theta=Σ[2(q-j)/q]² delta_j，z*=(VR-1)/sqrt(theta)；等价于把 delta
乘 n 后再用 sqrt(n)(VR-1)/sqrt(theta) 的常见写法。

P2 保留连续对数布朗模型的命中统计，另以跳空感知退出收益作期望检验，
不是订单回测；命中率检验对跳空不对称敏感，期望检验为主。双触发
只在5m完整覆盖粗 K 线时细分，5m仍双触发或无完整覆盖则 ambiguous。
timeout计入期望而不计命中率；不足完整30天的入场右删失；前段统计同样
要求完整30天观察窗口，不跨60%边界。有限期限、离散采样及剔除 ambiguous 会使命中率与
无限期限理论不同，报告必须同时给出这些数量，不能把拒绝归因于单一成因。

P3 唯一的简单收益例外是题定的 μ、σ 估计；其余收益/Sharpe使用对数收益。
每日收盘按 f×当前策略权益提交目标数量，下一根开盘由 Backtrader 撮合；
无终点虚构平仓。沿用 volrix 虚拟大现金隔离杠杆拒单及 FundingBroker，
策略权益<=0永久停止开仓。每日调仓决策次数与实际成交次数分开报告。
"""
from __future__ import annotations

import argparse
import json
import math
import time
from pathlib import Path

import numpy as np
import pandas as pd

from . import volrix

bt = volrix.bt
CRYPTO_MAIN = ('BTCUSDT', 'ETHUSDT')
US = ('SPY', 'QQQ', 'IWM', 'EFA', 'EEM', 'TLT', 'IEF', 'GLD', 'DBC', 'VNQ')
KELLY_ASSETS = (*CRYPTO_MAIN, 'SPY', 'QQQ', 'TLT', 'GLD')
Q_DAILY = (2, 5, 10, 20, 60)
Q_HOURLY = (2, 4, 8, 24, 72, 168)
BARRIERS = ((1, 1), (2, 1), (1, 2), (3, 1), (1, 3), (2, 2))
CUTOFF = pd.Timestamp('2026-01-01')
SEED = 20261031
THRESHOLDS = """P1：日线 q ∈ {2,5,10,20,60}；1h q ∈ {2,4,8,24,72,168}。
多重检验：对全样本的全部 P1 检验做 Holm 校正。
判定“显著且稳定的偏离”：全样本 Holm p<0.05 且 前后两段 VR−1 同号且 |z*|>1.96。VR>1 记为“趋势型”，VR<1 记为“均值回归型”。
篮子：每个币单独检验，另报告 CRYPTO_BASKET 中 VR(q)>1 显著的比例与 VR<1 显著的比例（未校正 p<0.05，预期假阳性率 2.5%/侧）。
P2：(k_up,k_dn) ∈ {(1,1),(2,1),(1,2),(3,1),(1,3),(2,2)}。
多重检验：全部（资产,方向,障碍对）对 (i) 的检验做 Holm 校正；“显著且稳定”=全样本 Holm p<0.05 且前后两段偏离同号且各自 |z|>1.96。
P2期望检验：平均gap_value−平均(direction·ν·实际持有天数/s)，月块bootstrap标准误；全部P2行另做Holm，稳定定义同命中率检验。命中率检验对跳空不对称敏感，期望检验为主。
P3：f ∈ {0.25,0.5,1,f̂*/2,f̂*,2f̂*}（f̂* 截断到 [0,5]）。
所有收益用对数收益；时间切分统一“前 60% / 后 40%”（按各序列自身长度）。
标准误用按自然月分块的块 bootstrap（1000 次，固定随机种子）。"""


def normal_p(z):
    """Two-sided standard-normal p, including deterministic zero-SE limits."""
    return math.erfc(abs(float(z))/math.sqrt(2)) if not math.isnan(z) else math.nan


def _z(difference, se):
    if not math.isfinite(se):
        return math.nan
    if se == 0:
        return 0. if abs(difference) < 1e-14 else math.copysign(math.inf, difference)
    return difference/se


def variance_ratio(returns, q):
    r = np.asarray(returns, dtype=float)
    if r.ndim != 1 or not np.isfinite(r).all() or q < 2 or len(r) <= q+1:
        raise ValueError('VR需要有限一维收益、q>=2和足够样本')
    n = len(r)
    e = r-r.mean()
    ss = float(e@e)
    if ss <= 0:
        return dict(n=n, vr=math.nan, z=math.nan, p=math.nan, theta=math.nan)
    cs = np.r_[0., np.cumsum(r)]
    overlapping = cs[q:]-cs[:-q]-q*r.mean()
    m = q*(n-q+1)*(1-q/n)
    vr = float(overlapping@overlapping/m/(ss/(n-1)))
    sq = e*e
    theta = sum((2*(q-j)/q)**2*float(sq[j:]@sq[:-j])/ss**2 for j in range(1, q))
    z = _z(vr-1, math.sqrt(theta))
    return dict(n=n, vr=vr, z=z, p=normal_p(z), theta=theta)


def holm_adjust(p_values):
    """Holm step-down adjusted p, preserving order; invalid tests count in family."""
    p = np.asarray(p_values, dtype=float)
    finite = np.isfinite(p)
    safe = np.where(finite, p, 1.)
    order = np.argsort(safe, kind='stable')
    out = np.empty(len(p))
    out[order] = np.minimum(1., np.maximum.accumulate(safe[order]*(len(p)-np.arange(len(p)))))
    out[~finite] = np.nan
    return out


def p_up(nu, sigma, a, b):
    """Stable two-barrier Brownian probability; nu/sigma use the same time unit."""
    if sigma <= 0 or a <= 0 or b <= 0 or not all(map(math.isfinite, (nu, sigma, a, b))):
        raise ValueError('sigma/a/b须正且参数有限')
    w = 2*nu/(sigma*sigma)
    if abs(w)*(a+b) < 1e-10:
        return b/(a+b)
    if w > 0:
        return math.expm1(-w*b)/math.expm1(-w*(a+b))
    return math.exp(w*a)*math.expm1(w*b)/math.expm1(w*(a+b))


def month_weights(dates, repetitions=1000, seed=SEED):
    if repetitions < 2:
        raise ValueError('bootstrap次数须>=2')
    idx = pd.DatetimeIndex(dates)
    codes, months = pd.factorize(idx.to_period('M'), sort=True)
    m = len(months)
    if m == 0:
        return codes, np.empty((repetitions, 0))
    # Multinomial counts exactly equal resampling m whole calendar months.
    return codes, np.random.default_rng(seed).multinomial(m, np.full(m, 1/m), size=repetitions)


def _bootstrap_ratio(numerator, denominator, dates, repetitions, seed):
    codes, weights = month_weights(dates, repetitions, seed)
    if not len(codes) or weights.shape[1] < 2:
        return math.nan
    a = np.bincount(codes, weights=numerator, minlength=weights.shape[1])
    b = np.bincount(codes, weights=denominator, minlength=weights.shape[1])
    den = weights@b
    values = np.divide(weights@a, den, out=np.full(len(weights), np.nan), where=den > 0)
    valid = values[np.isfinite(values)]
    return float(valid.std(ddof=1)) if len(valid) > 1 else math.nan


def load_us_frame(path):
    rows = json.loads(Path(path).read_text())
    df = pd.DataFrame([r[:7] for r in rows], columns=['date', 'open', 'high', 'low', 'close', 'adjclose', 'volume'])
    df.index = pd.DatetimeIndex(pd.to_datetime(df.pop('date'), utc=True)).tz_localize(None)
    df = df.astype(float)
    ratio = df.adjclose/df.close
    df[['open', 'high', 'low', 'close']] = df[['open', 'high', 'low', 'close']].mul(ratio, axis=0)
    df = df.drop(columns='adjclose')
    volrix.validate_frame(df)
    return df


def load_data(root, parts, progress=print):
    root = Path(root)
    paths = sorted((root/'factor_mining/crypto/daily').glob('*.json'))
    # Membership uses the declared current-survivor rule, including 2026 only
    # for coverage. Prices/returns in 2026 never enter a hypothesis test.
    bounds = []
    for path in paths:
        rows = json.loads(path.read_text())
        if rows:
            bounds.append((path, str(rows[0][0]), str(rows[-1][0])))
    latest = max(x[2] for x in bounds)
    basket = [p.stem for p, first, last in bounds if first < '2022-01-01' and last == latest]
    progress(f'CRYPTO_BASKET n={len(basket)}, 最新日={latest}: '+', '.join(basket))
    frames, post, info = {}, [], []
    for sym in sorted(set(basket) | set(CRYPTO_MAIN) | set(US)):
        crypto = sym not in US
        path = root/('factor_mining/crypto/daily' if crypto else 'quantjourney/prices')/f'{sym}.json'
        full = volrix.load_frame(path, '1d') if crypto else load_us_frame(path)
        frames[sym, '1d'] = full.loc[full.index < CUTOFF]
        post.append(_post_summary(sym, '1d', full.loc[full.index >= CUTOFF]))
        info.append(_coverage(sym, '1d', frames[sym, '1d']))
    fine = {}
    if {'P1', 'P2'} & set(parts):
        for sym in CRYPTO_MAIN:
            full = volrix.load_frame(root/'volrix/klines'/f'{sym}_1h.json', '1h')
            frames[sym, '1h'] = full.loc[full.index < CUTOFF]
            post.append(_post_summary(sym, '1h', full.loc[full.index >= CUTOFF]))
            info.append(_coverage(sym, '1h', frames[sym, '1h']))
            if 'P2' in parts:
                path = root/'volrix/klines'/f'{sym}_5m.json'
                if path.is_file():
                    fine[sym] = volrix.load_frame(path, '5m').loc[lambda d: d.index < CUTOFF]
    return frames, fine, dict(basket=basket, latest=latest, coverage=info, post_2026=post)


def _coverage(sym, tf, df):
    cut = int(len(df)*.6)
    return dict(asset=sym, frequency=tf, n=len(df), start=str(df.index[0]), end=str(df.index[-1]), split=str(df.index[cut]))


def _post_summary(sym, tf, df):
    return dict(asset=sym, frequency=tf, n=len(df), start=str(df.index[0]) if len(df) else None,
                end=str(df.index[-1]) if len(df) else None,
                log_return=float(math.log(df.close.iloc[-1]/df.close.iloc[0])) if len(df) else None)


def _groups(asset, frequency, basket):
    if frequency == '1h':
        return ['CRYPTO_MAIN/1h']
    out = ['US/1d'] if asset in US else ['CRYPTO_BASKET/1d'] if asset in basket else []
    if asset in CRYPTO_MAIN:
        out.append('CRYPTO_MAIN/1d')
    return out


def study_p1(frames, basket):
    records = []
    for (asset, tf), df in frames.items():
        r = np.diff(np.log(df.close.to_numpy()))
        cut = int(len(r)*.6)
        for q in Q_DAILY if tf == '1d' else Q_HOURLY:
            segments = {name: variance_ratio(x, q) for name, x in [('full', r), ('front', r[:cut]), ('back', r[cut:])]}
            records.append(dict(asset=asset, frequency=tf, q=q, groups=_groups(asset, tf, basket), **segments))
    adjusted = holm_adjust([x['full']['p'] for x in records])
    for row, p in zip(records, adjusted):
        row['holm_p'] = float(p)
        a, b = row['front'], row['back']
        row['stable'] = bool(p < .05 and (a['vr']-1)*(b['vr']-1) > 0 and abs(a['z']) > 1.96 and abs(b['z']) > 1.96)
        row['kind'] = '趋势型' if row['full']['vr'] > 1 else '均值回归型'
    return dict(records=records, family_size=len(records))


def entry_scales(df, daily, frequency):
    """Strictly before entry close; rolling calendar days, no current-day return."""
    daily_r = np.log(daily.close).diff()
    rv = daily_r.rolling('30D', min_periods=2).std(ddof=1)
    rv = rv.where(daily.index >= daily.index[0]+pd.Timedelta(30*86400, unit='s'))
    available = daily.index+pd.Timedelta(86400, unit='s')
    entry_times = df.index+pd.Timedelta(86400 if frequency == '1d' else 3600, unit='s')
    loc = available.searchsorted(entry_times, side='left')-1
    result = np.full(len(df), np.nan)
    valid = loc >= 0
    result[valid] = rv.to_numpy()[loc[valid]]
    return result


def _resolve_fine(fine_arrays, start, duration, entry, upper, lower):
    if fine_arrays is None:
        return 2, math.nan, None
    t, o, h, l = fine_arrays
    left, right = np.searchsorted(t, [start, start+duration])
    # Coverage must include every five-minute bar in the parent interval.
    if right-left != duration//300_000_000_000 or left == right:
        return 2, math.nan, None
    if t[left] != start or t[right-1]+300_000_000_000 != start+duration:
        return 2, math.nan, None
    if not np.all(np.diff(t[left:right]) == 300_000_000_000):
        return 2, math.nan, None
    up = np.log(h[left:right]/entry) >= upper
    dn = np.log(l[left:right]/entry) <= -lower
    hits = np.flatnonzero(up | dn)
    if not len(hits):
        return 2, math.nan, None
    j = hits[0]
    opening = math.log(o[left+j]/entry)
    outcome = (1 if opening >= upper else -1 if opening <= -lower else
               2 if up[j] and dn[j] else 1 if up[j] else -1)
    return outcome, o[left+j], t[left+j]+300_000_000_000


def barrier_events(df, scales, k_up, k_dn, *, direction=1, frequency='1d', fine=None,
                   interval=None, max_days=30):
    """Return independent entry observations, not trades or simulated broker P&L.

    outcome=+1 profit barrier, -1 loss barrier, 0 timeout, 2 ambiguous.
    Entry phase is fixed from row zero. Right-censoring is pre-entry eligibility.
    """
    if direction not in (-1, 1) or k_up <= 0 or k_dn <= 0:
        raise ValueError('方向须±1，障碍须正')
    scales = np.asarray(scales, dtype=float)
    if scales.shape != (len(df),):
        raise ValueError('波动尺度须与K线等长')
    stride = interval or (5 if frequency == '1d' else 24)
    duration = (86400 if frequency == '1d' else 3600)*1_000_000_000
    times = df.index.asi8
    close_times = times+duration
    closes, opens, highs, lows = (df[x].to_numpy() for x in ('close', 'open', 'high', 'low'))
    fine_arrays = None if fine is None else (fine.index.asi8, fine.open.to_numpy(), fine.high.to_numpy(), fine.low.to_numpy())
    records, censored, warmup = [], 0, 0
    for i in range(0, len(df), stride):
        s = scales[i]
        if not np.isfinite(s) or s <= 0:
            warmup += 1
            continue
        deadline = close_times[i]+int(max_days*86400*1_000_000_000)
        # Weekend deadline: the last already observed close before it is used.
        if close_times[-1] < deadline:
            censored += 1
            continue
        end = int(np.searchsorted(close_times, deadline, side='right')-1)
        if end <= i:
            censored += 1
            continue
        up_size, dn_size = (k_up*s, k_dn*s) if direction == 1 else (k_dn*s, k_up*s)
        up = np.log(highs[i+1:end+1]/closes[i]) >= up_size
        dn = np.log(lows[i+1:end+1]/closes[i]) <= -dn_size
        hits = np.flatnonzero(up | dn)
        outcome, exit_index = 0, end
        exit_close = close_times[end]
        exit_open = math.nan
        if len(hits):
            j = int(hits[0])
            exit_index = i+1+j
            exit_close, exit_open = close_times[exit_index], opens[exit_index]
            opening = math.log(exit_open/closes[i])
            price_outcome = (1 if opening >= up_size else -1 if opening <= -dn_size else
                             2 if up[j] and dn[j] else 1 if up[j] else -1)
            if price_outcome == 2:
                price_outcome, fine_open, fine_close = _resolve_fine(
                    fine_arrays, times[exit_index], duration, closes[i], up_size, dn_size)
                if price_outcome != 2:
                    exit_open, exit_close = fine_open, fine_close
            outcome = price_outcome if price_outcome == 2 else price_outcome*direction
        value = (k_up if outcome == 1 else -k_dn if outcome == -1 else
                 direction*math.log(closes[end]/closes[i])/s if outcome == 0 else math.nan)
        gap_value = value
        if outcome in (-1, 1):
            opening_value = direction*math.log(exit_open/closes[i])/s
            if (outcome == 1 and opening_value >= k_up) or (outcome == -1 and opening_value <= -k_dn):
                gap_value = opening_value
        records.append(dict(entry=i, exit=exit_index, date=pd.Timestamp(int(close_times[i]), unit='ns'),
                            deadline=pd.Timestamp(int(deadline), unit='ns'), s=float(s),
                            exit_date=pd.Timestamp(int(exit_close), unit='ns'),
                            holding_days=float((exit_close-close_times[i])/86400e9),
                            outcome=outcome, value=float(value), gap_value=float(gap_value)))
    return dict(events=records, right_censored=censored, warmup=warmup)


def summarize_barriers(events, k_up, k_dn, nu, sigma, direction, roundtrip_cost,
                       repetitions=1000, seed=SEED):
    n = len(events)
    out = np.array([x['outcome'] for x in events])
    hit = np.isin(out, [-1, 1])
    wins = out == 1
    p0 = k_dn/(k_up+k_dn)
    phat = float(wins.sum()/hit.sum()) if hit.any() else math.nan
    dates = [x['date'] for x in events]
    drift = np.array([p_up(direction*nu, x['s'], k_up*x['s'], k_dn*x['s']) for x in events])
    drift_global = np.array([p_up(direction*nu, sigma, k_up*x['s'], k_dn*x['s']) for x in events])
    pd_mean = float(drift[hit].mean()) if hit.any() else math.nan
    se = _bootstrap_ratio(wins.astype(float), hit.astype(float), dates, repetitions, seed)
    drift_se = _bootstrap_ratio(wins-hit*drift, hit.astype(float), dates, repetitions, seed)
    z = _z(phat-p0, se)
    zd = _z(phat-pd_mean, drift_se)
    valid = out != 2
    values = np.array([x['value'] for x in events])
    costs = np.array([roundtrip_cost/x['s'] for x in events])
    mean = float(values[valid].mean()) if valid.any() else math.nan
    net = float((values-costs)[valid].mean()) if valid.any() else math.nan
    gap_values = np.array([x['gap_value'] for x in events])
    drift_values = np.array([direction*nu*x['holding_days']/x['s'] for x in events])
    residual = gap_values-drift_values
    gap_mean = float(gap_values[valid].mean()) if valid.any() else math.nan
    excess = float(residual[valid].mean()) if valid.any() else math.nan
    expectation_se = _bootstrap_ratio(np.where(valid, residual, 0.), valid.astype(float),
                                      dates, repetitions, seed)
    expectation_z = _z(excess, expectation_se)
    return dict(n=n, hits=int(hit.sum()), up=int(wins.sum()), ambiguous=int((out == 2).sum()),
                timeout=int((out == 0).sum()), timeout_rate=float((out == 0).sum()/n) if n else math.nan,
                p_hat=phat, p_martingale=p0, p_drift=pd_mean,
                p_drift_global=float(drift_global[hit].mean()) if hit.any() else math.nan,
                se=se, z=z, p=normal_p(z),
                se_drift=drift_se, z_drift=zd, p_vs_drift=normal_p(zd), expectation_s=mean,
                net_expectation_s=net, hit_expectation_s=phat*k_up-(1-phat)*k_dn,
                cost_s=float(costs[valid].mean()) if valid.any() else math.nan,
                gap_expectation_s=gap_mean,
                gap_net_expectation_s=float((gap_values-costs)[valid].mean()) if valid.any() else math.nan,
                drift_expectation_s=float(drift_values[valid].mean()) if valid.any() else math.nan,
                expectation_excess_s=excess, expectation_se=expectation_se,
                expectation_z=expectation_z, expectation_p=normal_p(expectation_z))


def study_p2(frames, fine, basket, repetitions, progress=print):
    rows = []
    for (asset, tf), df in frames.items():
        progress(f'P2 {asset}/{tf}')
        cut = int(len(df)*.6)
        r = np.diff(np.log(df.close.iloc[:cut].to_numpy()))
        multiplier = 24 if tf == '1h' else 1
        nu, sigma = float(r.mean()*multiplier), float(r.std(ddof=1)*math.sqrt(multiplier))
        scales = entry_scales(df, frames[asset, '1d'], tf)
        costs = .0006 if asset in US else .0016
        for direction in (1, -1):
            for ku, kd in BARRIERS:
                found = barrier_events(df, scales, ku, kd, direction=direction, frequency=tf, fine=fine.get(asset))
                full = found['events']
                # Apply the SAME complete-horizon eligibility in every segment,
                # even to observations which happened to touch a barrier early.
                front_end = df.index[cut-1]+pd.Timedelta(86400 if tf == '1d' else 3600, unit='s')
                front = [x for x in full if x['entry'] < cut and x['deadline'] <= front_end]
                back = [x for x in full if x['entry'] >= cut]
                segments = {name: summarize_barriers(events, ku, kd, nu, sigma, direction, costs, repetitions)
                            for name, events in [('full', full), ('front', front), ('back', back)]}
                rows.append(dict(asset=asset, frequency=tf, direction='多头' if direction == 1 else '空头',
                                 k_up=ku, k_dn=kd, nu_train=nu, sigma_train=sigma,
                                 groups=_groups(asset, tf, basket), right_censored=found['right_censored'],
                                 warmup=found['warmup'], boundary_censored=len(full)-len(front)-len(back), **segments))
    for row, p in zip(rows, holm_adjust([x['full']['p'] for x in rows])):
        row['holm_p'] = float(p)
        a, b = row['front'], row['back']
        row['stable'] = bool(p < .05 and (a['p_hat']-a['p_martingale'])*(b['p_hat']-b['p_martingale']) > 0
                             and abs(a['z']) > 1.96 and abs(b['z']) > 1.96)
    for row, p in zip(rows, holm_adjust([x['full']['expectation_p'] for x in rows])):
        row['expectation_holm_p'] = float(p)
        a, b = row['front'], row['back']
        row['expectation_stable'] = bool(
            p < .05 and a['expectation_excess_s']*b['expectation_excess_s'] > 0
            and abs(a['expectation_z']) > 1.96 and abs(b['expectation_z']) > 1.96)
    return dict(records=rows, family_size=len(rows))


def kelly_fraction(mu, sigma):
    return mu/(sigma*sigma) if sigma > 0 else math.nan


def kelly_growth(f, mu, sigma):
    return f*mu-.5*f*f*sigma*sigma


def estimate_kelly(returns, dates, annual_days, repetitions=1000, seed=SEED):
    r = np.asarray(returns, dtype=float)
    mu, sigma = float(r.mean()*annual_days), float(r.std(ddof=1)*math.sqrt(annual_days))
    codes, weights = month_weights(dates, repetitions, seed)
    m = weights.shape[1]
    counts = np.bincount(codes, minlength=m)
    sums = np.bincount(codes, weights=r, minlength=m)
    squares = np.bincount(codes, weights=r*r, minlength=m)
    n, total, sq = weights@counts, weights@sums, weights@squares
    means = total/n
    variances = (sq-total*total/n)/(n-1)
    fs = np.divide(means, variances, out=np.full(len(n), np.nan), where=variances > 0)
    valid = fs[np.isfinite(fs)]
    ci = np.quantile(valid, [.025, .975]).tolist() if len(valid) else [math.nan, math.nan]
    raw = kelly_fraction(mu, sigma)
    return dict(mu=mu, sigma=sigma, f_raw=raw, f_star=float(np.clip(raw, 0, 5)), ci95=ci,
                unit_geometric=kelly_growth(1., mu, sigma))


class DailyRebalanceStrategy(volrix.TargetStrategy):
    """Native Market orders at every close; no changed-target shortcut."""
    params = (('fraction', 1.),)

    def __init__(self):
        super().__init__()
        self.rebalance_count = 0

    def next(self):
        self.rebalance_count += 1
        equity = self.broker.getvalue()-self.p.initial_cash+self.p.notional
        if equity <= 0:
            self.bankrupt = True
        size = 0. if self.bankrupt else self.p.fraction*equity/float(self.data.close[0])
        if not math.isclose(size, self.position.size, rel_tol=1e-12, abs_tol=1e-14):
            order = self.order_target_size(target=size, exectype=bt.Order.Market)
            if order:
                order.addinfo(signal_time=self.data.datetime.datetime(0).isoformat())


def run_daily_rebalance(df, fraction, *, commission, slippage, funding=None, notional=1000., annual_days=365):
    volrix.validate_frame(df)
    if not math.isfinite(fraction) or not 0 <= fraction <= 10:
        raise ValueError('Kelly实验仓位须在[0,10]')
    if not 0 <= commission < 1 or not 0 <= slippage < 1 or notional <= 0:
        raise ValueError('费用或初始资本不合法')
    rates = None if funding is None else {pd.Timestamp(k).date(): float(v) for k, v in funding.items()}
    cerebro = bt.Cerebro(stdstats=False)
    feed = bt.feeds.PandasData(dataname=df, timeframe=bt.TimeFrame.Days)
    cerebro.adddata(feed)
    initial_cash = 1e9
    broker = volrix.FundingBroker(funding_rates=rates, funding_data=feed)
    broker.setcash(initial_cash)
    broker.setcommission(commission=commission)
    broker.set_slippage_perc(slippage, slip_open=True, slip_match=True)
    broker.set_coc(False)
    broker.set_coo(False)
    cerebro.setbroker(broker)
    cerebro.addstrategy(DailyRebalanceStrategy, fraction=fraction, notional=notional, initial_cash=initial_cash)
    cerebro.addanalyzer(volrix.CapitalCurve, _name='capital', notional=notional, initial_cash=initial_cash)
    strategy = cerebro.run()[0]
    equity = strategy.analyzers.capital.get_analysis()
    values = np.array(list(equity.values()))
    previous = np.r_[notional, values[:-1]]
    bankrupt = strategy.bankrupt or bool((values <= 0).any())
    logs = np.log(values/previous) if not bankrupt else np.full(len(values), np.nan)
    std = float(logs.std(ddof=1))
    elapsed = (df.index[-1]-df.index[0]).total_seconds()/86400+1
    annual_log = float(logs.sum()*365/elapsed) if not bankrupt else None
    sharpe = float(logs.mean()/std*math.sqrt(annual_days)) if std > 1e-14 else 0. if not bankrupt else None
    peaks = np.maximum.accumulate(np.r_[notional, values])[1:]
    maxdd = float(np.max(1-values/peaks))
    return dict(engine='backtrader', engine_version=bt.__version__, fraction=fraction,
                annual_log_growth=annual_log, sharpe=sharpe, maxdd=maxdd, bankrupt=bankrupt,
                final_equity=float(values[-1]), commission_paid=strategy.commission_paid,
                funding_paid=broker.funding_paid, funding_days=broker.funding_days,
                rebalance_decisions=strategy.rebalance_count, fill_count=len(strategy.fills),
                daily_equity=equity, fills=strategy.fills)


def study_p3(frames, root, repetitions, progress=print):
    records = []
    for asset in KELLY_ASSETS:
        progress(f'P3 {asset}')
        df = frames[asset, '1d']
        cut = int(len(df)*.6)
        train, test = df.iloc[:cut], df.iloc[cut:]
        annual_days = 252 if asset in US else 365
        r = train.close.pct_change(fill_method=None).dropna()
        est = estimate_kelly(r, r.index, annual_days, repetitions)
        funding = None if asset in US else volrix.load_funding(root, asset)
        commission, slippage = (.0001, .0002) if asset in US else (.0005, .0003)
        labels = [('0.25', .25), ('0.5', .5), ('1', 1.), ('半Kelly', est['f_star']/2),
                  ('全Kelly', est['f_star']), ('双Kelly', est['f_star']*2)]
        runs, cached = [], {}
        for label, f in labels:
            if f not in cached:
                cached[f] = run_daily_rebalance(test, f, commission=commission, slippage=slippage,
                                               funding=funding, annual_days=annual_days)
            runs.append(dict(label=label, theory_growth=kelly_growth(f, est['mu'], est['sigma']), **cached[f]))
        valid = [r for r in runs if r['annual_log_growth'] is not None]
        best = max(valid, key=lambda x: x['annual_log_growth']) if valid else None
        full = next(r for r in runs if r['label'] == '全Kelly')
        half = next(r for r in runs if r['label'] == '半Kelly')
        gap = best['annual_log_growth']-full['annual_log_growth'] if best and full['annual_log_growth'] is not None else None
        records.append(dict(asset=asset, estimation=est, runs=runs,
                            train_start=str(train.index[0]), train_end=str(train.index[-1]),
                            test_start=str(test.index[0]), test_end=str(test.index[-1]),
                            annual_days=annual_days, best_label=best['label'] if best else None,
                            best_fraction=best['fraction'] if best else None, growth_gap=gap,
                            dd_full_minus_half=full['maxdd']-half['maxdd'],
                            funding_missing_days=sum(t.date() not in funding for t in test.index) if funding is not None else 0))
    return dict(records=records)


def fmt(value):
    if value is None or (isinstance(value, (float, np.floating)) and math.isnan(value)):
        return 'NA'
    if isinstance(value, (float, np.floating)):
        if math.isinf(value):
            return '+∞' if value > 0 else '−∞'
        return f'{value:.3f}'
    return str(value)


def table(headers, rows):
    return '\n'.join(['| '+' | '.join(headers)+' |', '| '+' | '.join(['---']*len(headers))+' |']+
                     ['| '+' | '.join(fmt(v) for v in row)+' |' for row in rows])


def stable_lines(part, result):
    lines = []
    for r in result['records']:
        stable_key = 'expectation_stable' if part == 'P2_expectation' else 'stable'
        if not r[stable_key]:
            continue
        f, a, b = (r[x] for x in ('full', 'front', 'back'))
        if part == 'P1':
            lines.append(f"- {r['asset']}/{r['frequency']} q={r['q']}：{r['kind']}；VR={fmt(f['vr'])}，Holm p={fmt(r['holm_p'])}；前段 VR/z*={fmt(a['vr'])}/{fmt(a['z'])}，后段={fmt(b['vr'])}/{fmt(b['z'])}。")
        elif part == 'P2_expectation':
            lines.append(f"- {r['asset']}/{r['frequency']} {r['direction']} ({r['k_up']},{r['k_dn']})：跳空E/漂移基准/偏离（s）={fmt(f['gap_expectation_s'])}/{fmt(f['drift_expectation_s'])}/{fmt(f['expectation_excess_s'])}，Holm p={fmt(r['expectation_holm_p'])}；前段偏离/z={fmt(a['expectation_excess_s'])}/{fmt(a['expectation_z'])}，后段={fmt(b['expectation_excess_s'])}/{fmt(b['expectation_z'])}。")
        else:
            lines.append(f"- {r['asset']}/{r['frequency']} {r['direction']} ({r['k_up']},{r['k_dn']})：p̂={fmt(f['p_hat'])}，p₀={fmt(f['p_martingale'])}，Holm p={fmt(r['holm_p'])}；前段偏离/z={fmt(a['p_hat']-a['p_martingale'])}/{fmt(a['z'])}，后段={fmt(b['p_hat']-b['p_martingale'])}/{fmt(b['z'])}。")
    return lines or ['- 无。']


def render_report(result):
    meta = result['data']
    lines = ['# 市场“鞅性”检验与 Kelly 增长分析', '',
             '本研究离线读取缓存，不生成交易策略。可选停止定理的动机与可积性、可预测暴露等条件见模块 docstring。拒绝某项原假设只说明与该统计模型不一致。', '',
             f"检验截止：2025-12-31；bootstrap={result['bootstrap']}，种子={SEED}；耗时={fmt(result['elapsed_seconds'])} 秒。",
             f"CRYPTO_BASKET（{len(meta['basket'])} 个，要求2022-01-01前上市且最新日 {meta['latest']} 仍有数据）："+', '.join(meta['basket'])+'。', '',
             '该篮子按当前存续状态筛选，存在幸存者偏差。BTC/ETH 同时属于 MAIN 和 BASKET，推断时只计算一次；汇总展示有交集。所有序列2026起均排除。', '',
             '## 预声明门槛原文', '', '```text', THRESHOLDS, '```', '',
             '补充固定口径：P2 的频率也属于检验标识；P1按一期收益数60%切分，P2/P3按K线数60%切分。P2使用过去30自然日日收益标准差，要求已具备完整30天历史，当前日收益不参与；美股窗口只含其中的交易日。入场收盘与UTC月块对齐。月块长度不一，重抽后按观察数加权。前段与全样本采用相同的完整30天持有窗口资格，窗口跨前段末端的观察从前段剔除，即使曾在段内提前触障；后段可使用当时已知的前段日波动；所有末端不足30天观察右删失。', '']
    sections = {}
    if 'P1' in result:
        data = result['P1']
        rows = []
        for group in ('CRYPTO_MAIN/1d', 'CRYPTO_MAIN/1h', 'CRYPTO_BASKET/1d', 'US/1d'):
            qs = Q_HOURLY if group.endswith('1h') else Q_DAILY
            for q in qs:
                records = [r for r in data['records'] if group in r['groups'] and r['q'] == q]
                n = len(records)
                trend = sum(r['full']['p'] < .05 and r['full']['vr'] > 1 for r in records)
                reversion = sum(r['full']['p'] < .05 and r['full']['vr'] < 1 for r in records)
                rows.append([group, q, n, float(np.median([r['full']['vr'] for r in records])),
                             100*trend/n, 100*reversion/n, sum(r['holm_p'] < .05 for r in records), sum(r['stable'] for r in records)])
        main = table(['集合/频率', 'q', '资产数', 'VR中位数', '未校正趋势显著%', '未校正回归显著%', 'Holm显著数', '稳定数'], rows)
        stable = '\n'.join(stable_lines('P1', data))
        sections['P1'] = dict(main_table=main, stable_list=stable)
        lines += ['## P1 方差比', '', f"Holm 检验族共 {data['family_size']} 项。下表为主表；篮子两侧比例的无偏离参考各为2.5%。每一项完整 VR/z*/双侧p 见其后的逐项记录及 JSON。", '', main, '', '### 显著且稳定', '', stable, '', '<details><summary>P1 全部逐项数值</summary>', '']
        for r in data['records']:
            triples = ['/'.join(fmt(r[s][k]) for k in ('vr', 'z', 'p')) for s in ('full', 'front', 'back')]
            lines.append(f"- {r['asset']}/{r['frequency']} q={r['q']}；全/前/后 VR,z*,p："+'；'.join(triples)+f"；Holm p={fmt(r['holm_p'])}。")
        lines += ['', '</details>', '']
    if 'P2' in result:
        data = result['P2']
        rows = []
        for group in ('CRYPTO_MAIN/1d', 'CRYPTO_MAIN/1h', 'CRYPTO_BASKET/1d', 'US/1d'):
            for direction in ('多头', '空头'):
                rs = [r for r in data['records'] if group in r['groups'] and r['direction'] == direction]
                ns = sum(r['full']['n'] for r in rs)
                rows.append([group, direction, len(rs), sum(r['full']['hits'] for r in rs),
                             sum(r['full']['ambiguous'] for r in rs), 100*sum(r['full']['timeout'] for r in rs)/ns,
                             float(np.median([r['full']['net_expectation_s'] for r in rs])),
                             sum(r['holm_p'] < .05 for r in rs), sum(r['stable'] for r in rs)])
        main = table(['集合/频率', '方向', '检验数', '命中观察数', 'ambiguous数', 'timeout%', '净期望s中位数', 'Holm显著数', '稳定数'], rows)
        stable = '\n'.join(stable_lines('P2', data))
        sections['P2'] = dict(main_table=main, stable_list=stable)
        expectation_rows = []
        for group in ('CRYPTO_MAIN/1d', 'CRYPTO_MAIN/1h', 'CRYPTO_BASKET/1d', 'US/1d'):
            for direction in ('多头', '空头'):
                rs = [r for r in data['records'] if group in r['groups'] and r['direction'] == direction]
                expectation_rows.append([group, direction, len(rs),
                    *[float(np.median([r['full'][key] for r in rs])) for key in
                      ('gap_expectation_s', 'gap_net_expectation_s', 'drift_expectation_s', 'expectation_excess_s')],
                    sum(r['expectation_holm_p'] < .05 for r in rs), sum(r['expectation_stable'] for r in rs)])
        expectation_main = table(['集合/频率', '方向', '检验数', '跳空E(s)中位数', '跳空净E(s)中位数',
                                  '漂移基准(s)中位数', '偏离(s)中位数', 'Holm显著数', '稳定数'], expectation_rows)
        expectation_stable = '\n'.join(stable_lines('P2_expectation', data))
        sections['P2'].update(expectation_main_table=expectation_main, expectation_stable_list=expectation_stable)
        lines += ['## P2 双障碍命中', '',
                  '**命中率检验对跳空不对称敏感，期望检验为主。**', '',
                  'Xₜ=νt+σWₜ 时，P_up=(1−exp(2νb/σ²))/(exp(−2νa/σ²)−exp(2νb/σ²))；ν→0 为 b/(a+b)。连续路径在零漂移下的障碍收益期望为0，成本后为负。短仓将价格上下障碍与漂移方向互换，p̂统一指先触盈利障碍。', '',
                  f"Holm 检验族共 {data['family_size']} 项。下表的观察数跨障碍对重复计数，不能当作独立样本数；净期望列为逐项估计的中位数。", '', main, '',
                  '日线每5根、小时每24根入场，入场价为收盘。漂移ν̂仅用前60%估计，小时换成每日漂移。p_drift逐笔用本地s作σ和障碍尺度：P_up(direction·ν̂,s,k_up·s,k_dn·s)，再在命中观察中平均；旧训练段全局σ口径保留为p_drift_global。标准误按入场所在自然月整块bootstrap；漂移比较标准误重抽“命中指示−逐观察理论概率”的月块。', '',
                  '期望s为非ambiguous观察的障碍收益和timeout实际方向对数收益/s的均值；仅命中部分另给出 p̂·k_up−(1−p̂)·k_dn。净期望逐观察扣往返成本，加密16bp、美股6bp，均除以该观察s；P2此成本不含资金费。无漂移理论为无限期限，有限30天、跳空、5m离散性、ambiguous剔除及月际相关均影响解释。标准误没有覆盖跨月的所有重叠相关。', '',
                  '### 命中率显著且稳定', '', stable, '',
                  '### 跳空感知期望检验（主检验）', '',
                  '开盘越过障碍按开盘对数收益/s，否则按障碍；开盘已触障时先于盘中高低价判定，5m消歧使用实际命中5m开盘及收盘时间。timeout按实际退出收盘收益，ambiguous剔除。漂移基准逐笔为direction·ν̂·持有自然天数/s，持有时间自入场收盘至实际退出K线收盘。统计量为平均gap_value减平均漂移基准，月块bootstrap标准误、双侧z/p；全部P2行单独Holm校正，稳定定义同命中率检验。检验统计量未扣成本，净期望列扣原定往返成本；未含资金费。ν̂视为固定训练估计，标准误不含其估计不确定性。各列为逐项中位数，故中位数间不必相减相等。', '',
                  expectation_main, '', '### 期望检验显著且稳定', '', expectation_stable, '',
                  '<details><summary>P2 全部逐项数值（全/前/后）</summary>', '']
        for r in data['records']:
            lines.append(f"- {r['asset']}/{r['frequency']} {r['direction']} ({r['k_up']},{r['k_dn']})：命中/期望Holm p={fmt(r['holm_p'])}/{fmt(r['expectation_holm_p'])}；训练ν/σ={fmt(r['nu_train'])}/{fmt(r['sigma_train'])}；末端右删失={r['right_censored']}，预热={r['warmup']}，前段边界剔除={r['boundary_censored']}。")
            for key, name in [('full', '全'), ('front', '前'), ('back', '后')]:
                s = r[key]
                lines.append(f"  {name}：n={s['n']}，hits={s['hits']}，ambiguous={s['ambiguous']}，timeout={s['timeout']}({fmt(100*s['timeout_rate'])}%)；p̂/p₀/P_up(ν̂)={fmt(s['p_hat'])}/{fmt(s['p_martingale'])}/{fmt(s['p_drift'])}；SE/z/p={fmt(s['se'])}/{fmt(s['z'])}/{fmt(s['p'])}；漂移SE/z/p={fmt(s['se_drift'])}/{fmt(s['z_drift'])}/{fmt(s['p_vs_drift'])}；命中E/含timeout E/成本/净E（s）={fmt(s['hit_expectation_s'])}/{fmt(s['expectation_s'])}/{fmt(s['cost_s'])}/{fmt(s['net_expectation_s'])}。")
                lines.append(f"  {name}跳空：p_drift_global={fmt(s['p_drift_global'])}；E/净E/漂移基准/偏离（s）={fmt(s['gap_expectation_s'])}/{fmt(s['gap_net_expectation_s'])}/{fmt(s['drift_expectation_s'])}/{fmt(s['expectation_excess_s'])}；期望SE/z/p={fmt(s['expectation_se'])}/{fmt(s['expectation_z'])}/{fmt(s['expectation_p'])}。")
        lines += ['', '</details>', '']
    if 'P3' in result:
        rows, conclusions = [], []
        for r in result['P3']['records']:
            est = r['estimation']
            full = next(x for x in r['runs'] if x['label'] == '全Kelly')
            half = next(x for x in r['runs'] if x['label'] == '半Kelly')
            rows.append([r['asset'], est['mu'], est['sigma'], est['f_star'], '/'.join(map(fmt, est['ci95'])),
                         full['annual_log_growth'], half['annual_log_growth'], r['best_fraction'], r['growth_gap'],
                         100*full['maxdd'], 100*half['maxdd']])
            conclusions.append(f"- {r['asset']}：前段 f̂*={fmt(est['f_star'])}；后段已测试集合中最优 f={fmt(r['best_fraction'])}（{r['best_label']}），全Kelly较该点年化对数增长差={fmt(r['growth_gap'])}；全/半Kelly MaxDD={fmt(100*full['maxdd'])}%/{fmt(100*half['maxdd'])}%，差={fmt(100*r['dd_full_minus_half'])}个百分点。")
        main = table(['资产', 'μ年化', 'σ年化', '截断f*', '原始f*区间95%', '全Kelly g实现', '半Kelly g实现', '样本后段最优f', '最优−全g', '全MaxDD%', '半MaxDD%'], rows)
        stable = '- 不适用：P3为前段估计、后段增长与回撤比较，不进行“显著且稳定”假设检验。'
        sections['P3'] = dict(main_table=main, stable_list=stable, conclusions='\n'.join(conclusions))
        lines += ['## P3 Kelly 与方差拖累', '',
                  '连续近似：g(f)=fμ−f²σ²/2，f*=μ/σ²；当μ>0时，f>2f*对应g<0；单位杠杆几何增长为μ−σ²/2。μ、σ使用前段简单日收益估计，加密按365、美股按252年化；区间为未截断f*的自然月块bootstrap百分位区间，实际实验点使用[0,5]截断值。理论未计费用/资金费，也未代入后段参数。', '', main, '',
                  '### 显著且稳定', '', stable, '',
                  '### 前段 f* 与后段结果、全/半 Kelly 回撤', '',
                  '以下以增长差连续量回答接近程度，不增设事后“接近”门槛；最优仅限预声明的六个测试点，不是连续仓位最优。', '', *conclusions, '',
                  '每日收盘决策、次日开盘成交；末根决策没有后续撮合，不虚构终点平仓。实现年化对数增长按实际自然日长度；Sharpe为日对数收益均值/标准差×√年交易日数，无风险利率0。资金费沿用volrix当日首根开盘前隔夜持仓×open×当日费率×3，缺失日不收费并计数。权益<=0标记破产，增长与Sharpe记NA。', '',
                  '<details><summary>P3 全部f的实现、理论与执行记录</summary>', '']
        for r in result['P3']['records']:
            e = r['estimation']
            lines.append(f"- {r['asset']}：训练 {r['train_start']} 至 {r['train_end']}；后段 {r['test_start']} 至 {r['test_end']}；原始f*={fmt(e['f_raw'])}，单位杠杆理论几何增长={fmt(e['unit_geometric'])}，资金费缺失日={r['funding_missing_days']}。")
            for x in r['runs']:
                lines.append(f"  {x['label']} f={fmt(x['fraction'])}：实现g={fmt(x['annual_log_growth'])}，理论g={fmt(x['theory_growth'])}，Sharpe={fmt(x['sharpe'])}，MaxDD={fmt(100*x['maxdd'])}%，破产={'是' if x['bankrupt'] else '否'}，每日决策/成交={x['rebalance_decisions']}/{x['fill_count']}，手续费/资金费={fmt(x['commission_paid'])}/{fmt(x['funding_paid'])}。")
        lines += ['', '</details>', '']
    lines += ['## 数据边界与2026起发布后样本外（仅描述）', '',
              '当前缓存中的2026数据只有覆盖和首末收盘对数变动的描述；本研究尚未发布，日历划出的该段并不自动构成真正前瞻样本外。未在该段运行P1/P2/P3，也未据其结果选择门槛。', '',
              '<details><summary>每序列检验覆盖、切分与2026描述</summary>', '']
    for row in meta['coverage']:
        lines.append(f"- 检验 {row['asset']}/{row['frequency']}：n={row['n']}，{row['start']} 至 {row['end']}；K线60%边界={row['split']}。")
    for row in meta['post_2026']:
        lines.append(f"- 2026起 {row['asset']}/{row['frequency']}：n={row['n']}，{row['start']} 至 {row['end']}，首末收盘对数变动={fmt(row['log_return'])}。")
    lines += ['', '</details>', '']
    result['report_sections'] = sections
    return '\n'.join(lines)


def json_safe(value):
    if isinstance(value, dict):
        return {str(k): json_safe(v) for k, v in value.items()}
    if isinstance(value, (list, tuple)):
        return [json_safe(v) for v in value]
    if isinstance(value, (np.floating, float)):
        return float(value) if math.isfinite(value) else None
    if isinstance(value, np.integer):
        return int(value)
    if isinstance(value, np.bool_):
        return bool(value)
    return value


def _output_path(path, data_root):
    resolved = Path(path).resolve()
    if any(resolved.is_relative_to(Path(root).resolve()) for root in (data_root, volrix.DATA_ROOT)):
        raise ValueError('输出路径不能位于只读data目录')
    return resolved


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--out', default='.prob_out/report.md')
    parser.add_argument('--json', default='.prob_out/report.json')
    parser.add_argument('--parts', default='P1,P2,P3')
    parser.add_argument('--bootstrap', type=int, default=1000)
    parser.add_argument('--data-root', type=Path, default=volrix.DATA_ROOT)
    args = parser.parse_args(argv)
    parts = [p.strip().upper() for p in args.parts.split(',')]
    if not parts or any(p not in ('P1', 'P2', 'P3') for p in parts) or len(set(parts)) != len(parts):
        parser.error('--parts须为P1,P2,P3的非空不重复子集')
    if args.bootstrap < 2:
        parser.error('--bootstrap须>=2')
    out, json_path = (_output_path(x, args.data_root) for x in (args.out, args.json))
    if out == json_path:
        parser.error('--out和--json须为不同文件')
    started = time.perf_counter()
    frames, fine, meta = load_data(args.data_root, parts)
    result = dict(parts=parts, bootstrap=args.bootstrap, seed=SEED, thresholds=THRESHOLDS, data=meta)
    if 'P1' in parts:
        print('P1 方差比', flush=True)
        result['P1'] = study_p1(frames, meta['basket'])
    if 'P2' in parts:
        result['P2'] = study_p2(frames, fine, meta['basket'], args.bootstrap, lambda s: print(s, flush=True))
    if 'P3' in parts:
        result['P3'] = study_p3(frames, args.data_root, args.bootstrap, lambda s: print(s, flush=True))
    result['elapsed_seconds'] = time.perf_counter()-started
    report = render_report(result)
    for path in (out, json_path):
        path.parent.mkdir(parents=True, exist_ok=True)
    out.write_text(report, encoding='utf-8')
    json_path.write_text(json.dumps(json_safe(result), ensure_ascii=False, indent=2, allow_nan=False)+'\n', encoding='utf-8')
    print(f'完成 parts={",".join(parts)} bootstrap={args.bootstrap} elapsed={result["elapsed_seconds"]:.3f}s', flush=True)
    for p in ('P1', 'P2'):
        if p in result:
            print(f'{p}: tests={result[p]["family_size"]} stable={sum(r["stable"] for r in result[p]["records"])}', flush=True)
    if 'P2' in result:
        print(f'P2 expectation: tests={result["P2"]["family_size"]} stable={sum(r["expectation_stable"] for r in result["P2"]["records"])}', flush=True)
    print(f'Markdown: {out}\nJSON: {json_path}', flush=True)
    return 0


if __name__ == '__main__':
    raise SystemExit(main())
