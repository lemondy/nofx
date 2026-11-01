"""Preregistered offline factor-quant crypto stop / partial take-profit study.

Run with the project venv: python -m scripts.backtest.fq_stop_tp_study --out .fq_stop_out
Only local files are read. All executions and portfolio accounting use Backtrader.
"""
from __future__ import annotations

import argparse
from dataclasses import asdict
import hashlib
from pathlib import Path

import numpy as np
import pandas as pd

from scripts.factor_mining import panel, quant_targets, store
from scripts.factor_mining.ensemble import ensemble_scores
from .quantjourney import CAPITAL, PerNameExitPolicy, RebalancePolicy, run_weights

ROOT = Path('/Users/zhangyun/workspace/nofx/data/factor_mining')
PREREG = Path(__file__).resolve().parents[2] / 'docs/research/2026-11-01-fq-stop-tp-prereg.md'
SEED, BLOCK, BOOTSTRAPS = 20261101, 5, 2000
VARIANTS = {
    'S0': PerNameExitPolicy(),
    'B0': PerNameExitPolicy(short_stop=.50),
    'S30': PerNameExitPolicy(long_stop=.30, short_stop=.30),
    'S20': PerNameExitPolicy(long_stop=.20, short_stop=.20),
    'S15': PerNameExitPolicy(long_stop=.15, short_stop=.15),
    'S10': PerNameExitPolicy(long_stop=.10, short_stop=.10),
    'SV': PerNameExitPolicy(volatility_stop=True),
    'T1': PerNameExitPolicy(long_stop=.20, short_stop=.20, take_profit=((.10, .5),)),
    'T2': PerNameExitPolicy(long_stop=.20, short_stop=.20,
                           take_profit=((.10, 1/3), (.20, 1/3))),
    'T3': PerNameExitPolicy(long_stop=.20, short_stop=.20,
                           trailing_activation=.10, trailing_distance=.10),
    'T4': PerNameExitPolicy(long_stop=.20, short_stop=.20,
                           take_profit=((.15, .5),), breakeven_after_tier=True),
}
STOP_CANDIDATES = ('S0', 'B0', 'S30', 'S15', 'S10', 'SV')
TP_CANDIDATES = ('T1', 'T2', 'T3', 'T4')

EXECUTION_NOTES = [
    '订单类型细节（相对字面触发价的偏离）：先用触发价/跳空开盘作为未计滑点参考价，'
    '再按原生 Backtrader 和 run_weights 的 0.03% 滑点成交。Stop（含盘中触发）及跳空 Limit '
    '有不利滑点；盘中触及 Limit 按限价成交。沿用 slip_out=True，不用未来高低价裁剪滑点。'
    'slippage_paid 为实际成交相对参考价的不利差额，已包含于净值，不重复扣除。',
    '日内路径约定：先检查已有止损（包括上一日已形成的移动/保本止损），'
    '然后更新本日最优 high/low 并检查新形成的移动止损，最后检查止盈档位。'
    'T3 当日启动/上移后用本日不利极值检查；T4 止盈后立即检查余仓保本。'
    '新形成的止损不会使用早于其形成的开盘价，按新触发价作参考。此为日线未知路径的保守消歧。',
    '每次组合目标阶梯实际变化后，在次日开盘恢复各币目标并重置止盈档位/保本状态；'
    '同向连续持仓保留持仓期最优价和已启动的移动止损。加仓后使用原生持仓新均价重新计算价位。'
    '止盈数量以该次调仓恢复后的数量为分母；T2 两档各为该数量的 1/3。',
    'SV 在每次从空仓/反向转入该方向时冻结阈值：仅用开仓前一根 bar 已知的 30 个简单日收益，'
    '样本标准差 ddof=1；同方向连续持有不重估 sigma。完整面板历史用于区间首笔的预热。',
]


def simple_returns(run):
    equity = pd.Series(run['daily_equity'], dtype=float).sort_index()
    returns = equity / equity.shift(1, fill_value=CAPITAL) - 1
    if not np.isfinite(returns.to_numpy()).all():
        raise ValueError('Nonfinite daily returns; preregistered comparison cannot proceed')
    return returns


def sharpe(returns, axis=-1):
    values = np.asarray(returns, dtype=float)
    sd = values.std(axis=axis, ddof=1)
    return np.divide(values.mean(axis=axis), sd, out=np.zeros_like(sd), where=sd > 0) * np.sqrt(365)


def bootstrap_diffs(returns):
    """Paired overlapping moving blocks of (reference, daily-return difference).

    Same sampled dates for both arms, recomputing SR(variant)-SR(reference) on
    every draw. Non-circular length-5 blocks, concatenate and truncate to n.
    The reference and its difference must remain paired to preserve covariance.
    """
    n = len(returns)
    if n < BLOCK or not np.isfinite(returns.to_numpy()).all():
        raise ValueError('Need at least five finite aligned daily returns')
    rng = np.random.default_rng(SEED)
    starts = rng.integers(0, n - BLOCK + 1, size=(BOOTSTRAPS, (n + BLOCK - 1) // BLOCK))
    indices = (starts[:, :, None] + np.arange(BLOCK)).reshape(BOOTSTRAPS, -1)[:, :n]
    comparisons = {}
    for reference in ('B0', 'S20'):
        base = returns[reference].to_numpy()
        sampled_base = base[indices]
        base_sharpe = sharpe(sampled_base, axis=1)
        comparisons[reference] = {}
        for variant in returns:
            difference = returns[variant].to_numpy() - base
            draws = sharpe(sampled_base + difference[indices], axis=1) - base_sharpe
            comparisons[reference][variant] = dict(
                sharpe_diff=float(sharpe(returns[variant]) - sharpe(base)),
                p_diff_gt_zero=float(np.mean(draws > 0)),
                p_diff_lt_zero=float(np.mean(draws < 0)),
                ci95=np.quantile(draws, [.025, .975]).tolist())
    return comparisons


def decision_rules(intervals):
    """Apply only the preregistered gates; validation is direction-only."""
    val, forward = intervals['val'], intervals['forward']
    warning_f = forward['bootstrap']['B0']['S20']
    warning_v = val['bootstrap']['B0']['S20']['sharpe_diff']
    warned = (warning_f['sharpe_diff'] < 0 and warning_f['p_diff_lt_zero'] >= .90 and warning_v < 0)
    decisions = [dict(
        rule='20pct_harm_warning', variant='S20', passed=warned,
        checks=dict(forward_sharpe_negative=warning_f['sharpe_diff'] < 0,
                    probability_at_least_90pct=warning_f['p_diff_lt_zero'] >= .90,
                    val_same_direction=warning_v < 0),
        line=f'VERDICT 20%警示 S20 vs B0: {"20% 止损在本组合上有害；保留用户 20% 选择，仅报告证据" if warned else "证据不足，不改"}'
             f' (forward ΔSharpe={warning_f["sharpe_diff"]:.6f}, P(差<0)={warning_f["p_diff_lt_zero"]:.4f},'
             f' val ΔSharpe={warning_v:.6f})')]
    s20 = forward['variants']['S20']['metrics']
    for category, variants in (('stop', STOP_CANDIDATES), ('take_profit', TP_CANDIDATES)):
        for variant in variants:
            f = forward['bootstrap']['S20'][variant]
            v = val['bootstrap']['S20'][variant]['sharpe_diff']
            m = forward['variants'][variant]['metrics']
            checks = dict(forward_sharpe_positive=f['sharpe_diff'] > 0,
                          probability_at_least_90pct=f['p_diff_gt_zero'] >= .90,
                          val_same_direction=v > 0)
            if category == 'stop':
                checks['drawdown_no_worse'] = m['maxdd_pct'] <= s20['maxdd_pct']
                extra = f'MDD={m["maxdd_pct"]:.4f}% <= S20 {s20["maxdd_pct"]:.4f}%: {checks["drawdown_no_worse"]}'
                label, action = '替代止损', f'建议用 {variant} 替代 S20'
            else:
                checks['net_return_no_lower'] = m['return_pct'] >= s20['return_pct']
                extra = f'净收益={m["return_pct"]:.4f}% >= S20 {s20["return_pct"]:.4f}%: {checks["net_return_no_lower"]}'
                label, action = '接入止盈', f'建议接入 {variant}'
            passed = all(checks.values())
            decisions.append(dict(rule=category, variant=variant, passed=passed, checks=checks,
                                  line=f'VERDICT {label} {variant} vs S20: {action if passed else "证据不足，不改"}'
                                       f' (forward ΔSharpe={f["sharpe_diff"]:.6f}, P(差>0)={f["p_diff_gt_zero"]:.4f},'
                                       f' val ΔSharpe={v:.6f}; {extra})'))
    return decisions


def study(root, progress=print):
    factors, frozen = quant_targets._load_frozen(root)
    panels = panel.load_panels('crypto', root)
    scores = ensemble_scores(factors, panels)
    # Shift BEFORE interval slicing: sigma at an opening entry excludes that
    # entry day's close and retains all pre-interval warmup observations.
    sigma = panels.fields['close'].pct_change(fill_method=None).rolling(30, min_periods=30).std(ddof=1).shift(1)
    result = dict(version=1, market='crypto', horizon=3, engine='backtrader',
                  prereg=dict(path=str(PREREG.relative_to(PREREG.parents[2])),
                              sha256=hashlib.sha256(PREREG.read_bytes()).hexdigest()),
                  root=str(Path(root).resolve()), factors=factors, inputs=frozen, panel=panels.meta,
                  initial_capital=CAPITAL, commission=.0005, slippage=.0003,
                  trials=11, variants={v: asdict(p) for v, p in VARIANTS.items()},
                  bootstrap_protocol=dict(block=BLOCK, runs=BOOTSTRAPS, seed=SEED,
                                          method='paired non-circular overlapping moving blocks; difference of simple-return Sharpes'),
                  execution_notes=EXECUTION_NOTES, intervals={})
    for name, start, stop in (('val', '2025-05-04', '2026-04-30'),
                              ('forward', '2026-05-04', panels.mask.index[-1])):
        dates = panels.mask.index[(panels.mask.index >= pd.Timestamp(start)) &
                                  (panels.mask.index <= pd.Timestamp(stop))]
        if len(dates) < BLOCK:
            raise ValueError(f'Insufficient data for preregistered interval {name}')
        targets, _, signals = quant_targets._historical_targets(panels, scores, factors, dates)
        frames, rates, targets = quant_targets._frames_rates(panels, targets)
        interval = dict(start=dates[0].strftime('%Y-%m-%d'), end=dates[-1].strftime('%Y-%m-%d'),
                        selection_bias=name == 'val', strict_out_of_sample=False,
                        signals=signals, variants={})
        daily = {}
        for variant, exit_policy in VARIANTS.items():
            run = run_weights(frames, targets, market='crypto', commission=.0005,
                              slippage=.0003, funding=rates,
                              policy=RebalancePolicy(frequency=None, rebalance_on_signal_change=True),
                              exit_policy=exit_policy, exit_volatility=sigma)
            daily[variant] = simple_returns(run)
            m = run['metrics']
            m['sharpe_log'], m['sharpe'] = m['sharpe'], m['sharpe_simple_rf']
            three_day = (1 + daily[variant]).rolling(3).apply(np.prod, raw=True) - 1
            m['worst_3d_return_pct'] = float(three_day.min() * 100)
            m['total_cost_paid'] = m['commission_paid'] + m['slippage_paid'] + m['funding_paid']
            run['daily_returns'] = daily[variant].to_dict()
            interval['variants'][variant] = run
            progress(f'{name} {variant}: return={m["return_pct"]:.4f}% Sharpe={m["sharpe"]:.6f}'
                     f' MDD={m["maxdd_pct"]:.4f}% stops={m["stop_trigger_count"]} TP={m["tp_trigger_count"]}', flush=True)
        interval['bootstrap'] = bootstrap_diffs(pd.DataFrame(daily))
        result['intervals'][name] = interval
    result['decisions'] = decision_rules(result['intervals'])
    return result


def report(result):
    lines = ['# 因子量化 crypto：单币止损与分批止盈预注册研究', '',
             f'预注册 SHA256：`{result["prereg"]["sha256"]}`。冻结 crypto h=3 ensemble；11 个变体，K=11，无重选、调参。', '',
             '**val 有成分选择偏差，仅检查方向；forward 曾被 ensemble 研究读取，不是严格样本外。判定以 forward 为主。**', '',
             '初始资金 1000；手续费 0.05%，滑点参数 0.03%，计入日 funding。所有订单、数量、均价、交易 P&L 和净值由 Backtrader 计算。', '',
             '## 两个区间的指标', '']
    for name, interval in result['intervals'].items():
        lines += [f'### {name}: {interval["start"]} ～ {interval["end"]}', '',
                  '| 变体 | 总收益 % | 年化 % | Sharpe | 最大回撤 % | 最差 3 日 % | 止损次数 | 止盈次数 | 换手 | 手续费 | 滑点 | 资金费净付出 | 总成本 |',
                  '| --- | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: |']
        for variant, run in interval['variants'].items():
            m = run['metrics']
            values = [variant] + [f'{m[k]:.6f}' if m[k] is not None else 'NA' for k in
                                  ('return_pct', 'cagr_pct', 'sharpe', 'maxdd_pct', 'worst_3d_return_pct',
                                   'stop_trigger_count', 'tp_trigger_count', 'turnover', 'commission_paid',
                                   'slippage_paid', 'funding_paid', 'total_cost_paid')]
            lines.append('| ' + ' | '.join(values) + ' |')
        lines += ['', '触发次数按成交订单计（两档同日止盈计两次）；止损次数包含移动和保本。', '',
                  '| 变体 | 固定止损 | 移动止损 | 保本止损 | 止盈 | funding 覆盖/持仓币日 |',
                  '| --- | ---: | ---: | ---: | ---: | ---: |']
        for variant, run in interval['variants'].items():
            m, c = run['metrics'], run['metrics']['exit_trigger_counts']
            lines.append(f'| {variant} | {c.get("stop_loss", 0)} | {c.get("trailing_stop", 0)} |'
                         f' {c.get("breakeven_stop", 0)} | {c.get("take_profit", 0)} |'
                         f' {m["funding_covered"]}/{m["funding_observations"]} |')
        lines += ['', '### 块自助法：' + name, '',
                  '按日期对齐简单日收益差，参考收益与差值成对抽取非循环重叠 5 日块，拼接截为原样本长度；'
                  '2000 次，seed=20261101。每次重算 Sharpe(变体)−Sharpe(参考)，不是差值序列本身的 Sharpe。'
                  'P 是自助样本符号比例；无差异的自比较两侧 P 都为 0。K=11，未作预注册以外的多重试验校正。', '',
                  '| 变体 | 参考 | ΔSharpe | P(差>0) | P(差<0) | 95% 区间 |',
                  '| --- | --- | ---: | ---: | ---: | --- |']
        for variant in VARIANTS:
            for reference in ('B0', 'S20'):
                b = interval['bootstrap'][reference][variant]
                lines.append(f'| {variant} | {reference} | {b["sharpe_diff"]:.6f} |'
                             f' {b["p_diff_gt_zero"]:.4f} | {b["p_diff_lt_zero"]:.4f} |'
                             f' [{b["ci95"][0]:.6f}, {b["ci95"][1]:.6f}] |')
        lines.append('')
    lines += ['## 机械判定（全部规则）', '']
    lines += [d['line'] + '  ' for d in result['decisions']]
    lines += ['', '## 执行约定及预注册细节/偏离', ''] + ['- ' + note for note in EXECUTION_NOTES]
    lines += ['', '- 两区间各自空仓起步；首日收盘信号次日开盘执行，之后每三天目标阶梯，仅目标实际变化时调仓。'
              '区间末按收盘估值，不额外强平。缺失 OHLC 不交易，只前填估值；缺失 funding 计零。',
              '- Sharpe 使用简单日收益、样本标准差和 √365，含首个零收益日；年化使用 365/观测日数。'
              '最差 3 日收益为三个相邻日简单收益的复合值，含初始资金到首日的收益。最大回撤包含初始资金。',
              '- 累计换手为逐笔绝对名义/成交前权益的双向口径。成本为初始资金 1000 下的金额；'
              '资金费为净付出，可为负。滑点已计入净值，费用列仅作归因；总成本为三者之和。',
              '- 日 funding 沿用 CarryBroker：隔夜持仓×当日开盘×日均 rate×3。'
              '日线无盘中路径，未模拟强平、冲击、容量或最小订单。', '',
              '## 局限与冻结输入', '',
              '- forward 约 180 天、约 60 次调仓，功效有限；100 币面板有幸存者偏差。'
              '通过门槛仅为本预注册证据，不代表严格样本外验证。', '']
    for name, frozen in result['inputs'].items():
        lines.append(f'- {name}: SHA256 `{frozen["sha256"]}`')
    lines += ['', '完整冻结成分/表达式/方向、目标信号、每日净值/收益、退出成交明细和判定布尔门槛见 results.json。']
    return '\n'.join(lines) + '\n'


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--out', type=Path, default=Path('.fq_stop_out'))
    parser.add_argument('--root', type=Path, default=ROOT)
    args = parser.parse_args(argv)
    out = args.out.resolve()
    if out.is_relative_to(args.root.resolve()) or out.is_relative_to(ROOT.parent.resolve()):
        parser.error('--out must be outside the read-only data tree')
    prereg_before = PREREG.read_bytes()
    result = study(args.root)
    if PREREG.read_bytes() != prereg_before:
        raise RuntimeError('Preregistration changed during study; refusing output')
    out.mkdir(parents=True, exist_ok=True)
    store.write_json(out / 'results.json', result)
    (out / 'report.md').write_text(report(result), encoding='utf-8')
    for decision in result['decisions']:
        print(decision['line'])
    print(f'Reports: {out / "report.md"}, {out / "results.json"}')


if __name__ == '__main__':
    main()
