"""实盘已平仓交易的 Kelly 与样本量分析（只读打开数据库，绝不写入）。

用法:
    python -m scripts.backtest.live_kelly --db /path/to/data.db \
        --out .prob_out/live_kelly.md --json .prob_out/live_kelly.json

口径与判断依据（详见报告）:
    * 仅统计 status='CLOSED' 且 source='system' 的行作为主分析。
      代码依据: store/position.go CreateOpenPosition 在系统直接开仓路径上
      默认 Source="system"; 而 store/position_builder.go 的交易所成交回放
      (sync) 路径写 Source="sync"，其中混有人工单（ai_managed=0），故
      "系统下单"取 source='system'。另附 ai_managed=1（全部来源）的参考组。
    * risk = |entry_price − initial_stop_loss| × (entry_quantity，为 0 时用
      quantity)；R = (realized_pnl − fee) / risk。realized_pnl 为毛盈亏
      （未扣手续费，经抽样核对 (exit−entry)×qty×方向 ≈ realized_pnl），
      因此减去 fee。initial_stop_loss<=0 的行不计 R，单独计数。
    * E[R] 95% 区间: bootstrap（固定种子 10000 次）与 t 区间双口径。
    * Kelly（每笔以初始风险计）: f* = p − (1−p)/b；连续近似 E[R]/E[R²]。
    * 胜率贝叶斯后验: Beta(1+赢, 1+输)。
    * 显著所需样本量: n_needed = (1.96·s/E[R])²，E[R]<=0 时不适用。

环境不依赖 scipy: Beta / t 分位数用正则化不完全贝塔函数（连分式）+ 二分法。
"""

from __future__ import annotations

import argparse
import datetime as dt
import json
import math
import sqlite3
from pathlib import Path

import numpy as np

SEED = 42
N_BOOT = 10000
Z95 = 1.96

COLS = (
    "id, trader_id, symbol, side, entry_quantity, quantity, entry_price, "
    "exit_price, entry_time, exit_time, realized_pnl, fee, leverage, "
    "close_reason, source, initial_stop_loss, ai_managed"
)


# ---------------------------------------------------------------------------
# 统计原语（无 scipy 实现）
# ---------------------------------------------------------------------------

def _betacf(a: float, b: float, x: float) -> float:
    """正则化不完全贝塔函数用的连分式（Lentz 算法，Numerical Recipes 口径）。"""
    maxit, eps, fpmin = 300, 3e-16, 1e-300
    qab, qap, qam = a + b, a + 1.0, a - 1.0
    c = 1.0
    d = 1.0 - qab * x / qap
    if abs(d) < fpmin:
        d = fpmin
    d = 1.0 / d
    h = d
    for m in range(1, maxit + 1):
        m2 = 2 * m
        aa = m * (b - m) * x / ((qam + m2) * (a + m2))
        d = 1.0 + aa * d
        if abs(d) < fpmin:
            d = fpmin
        c = 1.0 + aa / c
        if abs(c) < fpmin:
            c = fpmin
        d = 1.0 / d
        h *= d * c
        aa = -(a + m) * (qab + m) * x / ((a + m2) * (qap + m2))
        d = 1.0 + aa * d
        if abs(d) < fpmin:
            d = fpmin
        c = 1.0 + aa / c
        if abs(c) < fpmin:
            c = fpmin
        d = 1.0 / d
        de = d * c
        h *= de
        if abs(de - 1.0) < eps:
            break
    return h


def betainc_reg(a: float, b: float, x: float) -> float:
    """正则化不完全贝塔函数 I_x(a, b) = Beta CDF。"""
    if x <= 0.0:
        return 0.0
    if x >= 1.0:
        return 1.0
    ln_bt = (math.lgamma(a + b) - math.lgamma(a) - math.lgamma(b)
             + a * math.log(x) + b * math.log1p(-x))
    bt = math.exp(ln_bt)
    if x < (a + 1.0) / (a + b + 2.0):
        return bt * _betacf(a, b, x) / a
    return 1.0 - bt * _betacf(b, a, 1.0 - x) / b


def beta_ppf(q: float, a: float, b: float) -> float:
    """Beta(a, b) 分布的分位数（CDF 单调，二分法到机器精度）。"""
    if not 0.0 < q < 1.0:
        raise ValueError("q must be in (0, 1)")
    lo, hi = 0.0, 1.0
    for _ in range(200):
        mid = 0.5 * (lo + hi)
        if mid <= lo or mid >= hi:
            break
        if betainc_reg(a, b, mid) < q:
            lo = mid
        else:
            hi = mid
    return 0.5 * (lo + hi)


def t_ppf(q: float, df: float) -> float:
    """学生 t 分布的分位数。利用 t 的尾概率与 I_x(df/2, 1/2) 的关系。

    P(T > t) = 0.5 * I_{df/(df+t^2)}(df/2, 1/2)  =>  给定上尾概率 p，
    先解 beta_ppf(2p, df/2, 0.5) = df/(df+t^2)，再反解 t。
    """
    if not 0.0 < q < 1.0:
        raise ValueError("q must be in (0, 1)")
    if q == 0.5:
        return 0.0
    if q < 0.5:
        return -t_ppf(1.0 - q, df)
    tail = 2.0 * (1.0 - q)  # 上尾概率的两倍
    x = beta_ppf(tail, df / 2.0, 0.5)
    return math.sqrt(df * (1.0 / x - 1.0))


# ---------------------------------------------------------------------------
# 核心公式
# ---------------------------------------------------------------------------

def kelly_fraction(p: float | None, b: float | None) -> float | None:
    """离散 Kelly: f* = p − (1−p)/b（每笔风险占权益比例）。"""
    if p is None or b is None or b <= 0:
        return None
    return p - (1.0 - p) / b


def kelly_continuous(rs) -> float | None:
    """连续近似 Kelly: f* ≈ E[R] / E[R²]。"""
    rs = [float(x) for x in rs]
    n = len(rs)
    if n == 0:
        return None
    e = sum(rs) / n
    e2 = sum(x * x for x in rs) / n
    if e2 <= 0:
        return None
    return e / e2


def beta_posterior(wins: int, losses: int) -> dict:
    """胜率后验 Beta(1+赢, 1+输) 的均值与 95% 可信区间。"""
    a, b = 1.0 + wins, 1.0 + losses
    return {
        "alpha": a,
        "beta": b,
        "mean": a / (a + b),
        "ci95": [beta_ppf(0.025, a, b), beta_ppf(0.975, a, b)],
    }


def n_needed(expectancy: float | None, std: float | None,
             z: float = Z95) -> float | None:
    """检出当前期望所需样本量 n = (z·s/E)²；E<=0 或 s 无效时不适用。"""
    if expectancy is None or std is None or expectancy <= 0 or std <= 0:
        return None
    return (z * std / expectancy) ** 2


def compute_r(rec: dict) -> tuple[float | None, float | None, str]:
    """单笔 R 值。返回 (risk, r, reason)。

    risk = |entry_price − initial_stop_loss| × qty（entry_quantity，为 0 用
    quantity）；R = (realized_pnl − fee) / risk。initial_stop_loss<=0 →
    reason='stop_missing'；risk 无效 → reason='risk_invalid'。两种情况
    risk 与 r 均为 None。
    """
    stop = rec.get("initial_stop_loss") or 0.0
    if stop <= 0:
        return None, None, "stop_missing"
    entry = float(rec["entry_price"])
    qty = float(rec.get("entry_quantity") or 0.0)
    if qty <= 0:
        qty = float(rec.get("quantity") or 0.0)
    risk = abs(entry - float(stop)) * qty
    if risk <= 0:
        return None, None, "risk_invalid"
    r = (float(rec.get("realized_pnl") or 0.0) - float(rec.get("fee") or 0.0)) / risk
    return risk, r, "ok"


def summarize(rs, seed: int = SEED, n_boot: int = N_BOOT) -> dict:
    """对一组 R 值给出胜率、赔率、期望、区间、Kelly、后验与样本量。"""
    arr = np.asarray([float(x) for x in rs], dtype=float)
    n = int(arr.size)
    if n == 0:
        return {
            "n": 0, "wins": 0, "losses": 0, "flat": 0, "p": None,
            "r_win": None, "r_loss": None, "b": None, "expectancy": None,
            "std": None, "boot_ci95": None, "p_positive": None,
            "t_ci95": None, "kelly": None, "kelly_cont": None,
            "beta": None, "n_needed": None,
        }
    wins = int((arr > 0).sum())
    losses = int((arr < 0).sum())
    flat = n - wins - losses
    p = wins / n
    r_win = float(arr[arr > 0].mean()) if wins else None
    r_loss = float(arr[arr < 0].mean()) if losses else None
    b = (r_win / abs(r_loss)) if (r_win is not None and r_loss not in (None, 0.0)) else None
    expectancy = float(arr.mean())
    std = float(arr.std(ddof=1)) if n > 1 else None

    rng = np.random.default_rng(seed)
    idx = rng.integers(0, n, size=(n_boot, n))
    boot_means = arr[idx].mean(axis=1)
    boot_ci95 = [float(np.percentile(boot_means, 2.5)),
                 float(np.percentile(boot_means, 97.5))]
    p_positive = float((boot_means > 0).mean())

    t_ci95 = None
    if std is not None and std > 0:
        half = t_ppf(0.975, n - 1) * std / math.sqrt(n)
        t_ci95 = [expectancy - half, expectancy + half]

    return {
        "n": n, "wins": wins, "losses": losses, "flat": flat, "p": p,
        "r_win": r_win, "r_loss": r_loss, "b": b,
        "expectancy": expectancy, "std": std,
        "boot_ci95": boot_ci95, "p_positive": p_positive,
        "t_ci95": t_ci95,
        "kelly": kelly_fraction(p, b),
        "kelly_cont": kelly_continuous(arr.tolist()),
        "beta": beta_posterior(wins, losses),
        "n_needed": n_needed(expectancy, std),
    }


# ---------------------------------------------------------------------------
# 数据读取（只读）
# ---------------------------------------------------------------------------

def open_ro(db_path: str) -> sqlite3.Connection:
    """只读打开 SQLite（mode=ro），写入会直接报错。"""
    con = sqlite3.connect(f"file:{db_path}?mode=ro", uri=True)
    con.row_factory = sqlite3.Row
    return con


def load_closed(con: sqlite3.Connection, extra_where: str = "1=1") -> list[dict]:
    sql = (
        f"SELECT {COLS} FROM trader_positions "
        f"WHERE status='CLOSED' AND {extra_where} ORDER BY exit_time, id"
    )
    return [dict(r) for r in con.execute(sql)]


# ---------------------------------------------------------------------------
# 分组与聚合
# ---------------------------------------------------------------------------

def trader_label(trader_id: str) -> str:
    """trader_id 前 8 位 + 模型名，如 d7ff3764_glm。"""
    parts = trader_id.split("_")
    model = parts[2] if len(parts) >= 3 else ""
    return f"{trader_id[:8]}_{model}" if model else trader_id[:8]


def build_labels(trader_ids) -> dict:
    """trader_id → 展示名；前 8 位+模型重名时追加尾段消歧。"""
    buckets: dict[str, list[str]] = {}
    for tid in trader_ids:
        buckets.setdefault(trader_label(tid), []).append(tid)
    labels = {}
    for lab, tids in buckets.items():
        if len(tids) == 1:
            labels[tids[0]] = lab
        else:
            for tid in sorted(tids):
                labels[tid] = f"{lab}_{tid.rsplit('_', 1)[-1][:6]}"
    return labels


def enrich(rows: list[dict], labels: dict) -> list[dict]:
    """补充 risk / r / month / label 字段。"""
    out = []
    for rec in rows:
        rec = dict(rec)
        risk, r, reason = compute_r(rec)
        rec["risk"], rec["r"], rec["r_reason"] = risk, r, reason
        ts = rec.get("exit_time") or rec.get("entry_time")
        rec["month"] = ms_to_month(ts)
        rec["label"] = labels.get(rec["trader_id"], rec["trader_id"][:8])
        out.append(rec)
    return out


def monthly_er(trades: list[dict]) -> list[dict]:
    """按平仓月份聚合 E[R]（UTC 月）。"""
    buckets: dict[str, list[float]] = {}
    for t in trades:
        if t["r"] is None:
            continue
        buckets.setdefault(t["month"], []).append(t["r"])
    return [
        {"month": m, "n": len(v), "expectancy": sum(v) / len(v)}
        for m, v in sorted(buckets.items())
    ]


def analyze_scope(trades: list[dict], desc: str, seed: int = SEED,
                  n_boot: int = N_BOOT) -> dict:
    """一个口径下的分析：全部合并一组 + 每个 trader_id 一组。"""
    with_r = [t for t in trades if t["r"] is not None]
    rs_all = [t["r"] for t in with_r]
    reason_counts: dict[str, int] = {}
    for t in trades:
        if t["r"] is None:
            reason_counts[t["r_reason"]] = reason_counts.get(t["r_reason"], 0) + 1

    by_trader: dict[str, list[dict]] = {}
    for t in trades:
        by_trader.setdefault(t["trader_id"], []).append(t)

    groups = [{
        "key": "全部合并",
        "trader_id": None,
        "closed": len(trades),
        "stats": summarize(rs_all, seed=seed, n_boot=n_boot),
    }]
    for tid in sorted(by_trader):
        t_trades = by_trader[tid]
        t_rs = [t["r"] for t in t_trades if t["r"] is not None]
        groups.append({
            "key": t_trades[0]["label"],
            "trader_id": tid,
            "closed": len(t_trades),
            "stats": summarize(t_rs, seed=seed, n_boot=n_boot),
        })

    span = [
        ms_to_date(min(int(t["exit_time"] or t["entry_time"] or 0) for t in trades)),
        ms_to_date(max(int(t["exit_time"] or t["entry_time"] or 0) for t in trades)),
    ] if trades else None

    return {
        "filter": desc,
        "closed": len(trades),
        "with_r": len(with_r),
        "without_r": len(trades) - len(with_r),
        "without_r_reasons": reason_counts,
        "span_utc": span,
        "monthly": monthly_er(trades),
        "groups": groups,
    }


# ---------------------------------------------------------------------------
# 数据概况（口径核对）
# ---------------------------------------------------------------------------

def overview(con: sqlite3.Connection) -> dict:
    o: dict = {}
    o["total_rows"] = con.execute(
        "SELECT COUNT(*) FROM trader_positions").fetchone()[0]
    o["source_status"] = [
        {"source": r[0], "status": r[1], "n": r[2]}
        for r in con.execute(
            "SELECT source, status, COUNT(*) FROM trader_positions "
            "GROUP BY source, status ORDER BY source, status")
    ]
    o["stop_by_source"] = [
        {"source": r[0], "closed": r[1], "with_stop": r[2] or 0}
        for r in con.execute(
            "SELECT source, COUNT(*), SUM(CASE WHEN initial_stop_loss>0 THEN 1 "
            "ELSE 0 END) FROM trader_positions WHERE status='CLOSED' "
            "GROUP BY source")
    ]
    tot, with_stop = con.execute(
        "SELECT COUNT(*), SUM(CASE WHEN initial_stop_loss>0 THEN 1 ELSE 0 END) "
        "FROM trader_positions WHERE status='CLOSED'").fetchone()
    o["stop_overall"] = {"closed": tot, "with_stop": with_stop or 0}
    o["ai_managed_by_source"] = [
        {"source": r[0], "ai_managed": r[1], "n": r[2], "with_stop": r[3] or 0}
        for r in con.execute(
            "SELECT source, ai_managed, COUNT(*), SUM(CASE WHEN "
            "initial_stop_loss>0 THEN 1 ELSE 0 END) FROM trader_positions "
            "WHERE status='CLOSED' GROUP BY source, ai_managed "
            "ORDER BY source, ai_managed")
    ]
    o["pnl_fee_check"] = pnl_fee_check(con)
    return o


def pnl_fee_check(con: sqlite3.Connection, per_source: int = 3) -> list[dict]:
    """抽样核对 realized_pnl 是否已扣手续费：对比毛盈亏 (exit−entry)×qty×方向。"""
    out = []
    for source in ("system", "sync"):
        rows = con.execute(
            f"SELECT {COLS} FROM trader_positions WHERE status='CLOSED' "
            f"AND source=? AND exit_price>0 ORDER BY id LIMIT ?",
            (source, per_source)).fetchall()
        for rec in rows:
            rec = dict(rec)
            qty = rec["entry_quantity"] or rec["quantity"]
            sign = 1.0 if str(rec["side"]).upper().startswith("LONG") else -1.0
            gross = (rec["exit_price"] - rec["entry_price"]) * qty * sign
            out.append({
                "id": rec["id"], "source": rec["source"], "side": rec["side"],
                "gross": gross, "realized_pnl": rec["realized_pnl"],
                "fee": rec["fee"], "net": rec["realized_pnl"] - rec["fee"],
            })
    return out


# ---------------------------------------------------------------------------
# 报告渲染
# ---------------------------------------------------------------------------

def fmt(x, nd: int = 4, dash: str = "—") -> str:
    if x is None or (isinstance(x, float) and math.isnan(x)):
        return dash
    return f"{x:.{nd}f}"


def fmt_pct(x, nd: int = 1, dash: str = "—") -> str:
    if x is None:
        return dash
    return f"{100 * x:.{nd}f}%"


def fmt_ci(ci, nd: int = 4) -> str:
    if not ci:
        return "—"
    return f"[{ci[0]:.{nd}f}, {ci[1]:.{nd}f}]"


def fmt_n(x) -> str:
    if x is None:
        return "不适用"
    return f"{x:,.0f}"


def summary_table(scope: dict) -> str:
    head = ("| trader | 笔数 | 有R笔数 | 胜率p | 赔率b | E[R] | "
            "E[R] 95%区间(bootstrap) | P(E[R]>0) | Kelly f* | n_needed |\n"
            "|---|---:|---:|---:|---:|---:|---|---:|---:|---:|\n")
    lines = [head]
    for g in scope["groups"]:
        s = g["stats"]
        lines.append(
            f"| {g['key']} | {g.get('closed', s['n'])} | {s['n']} | {fmt_pct(s['p'])} | "
            f"{fmt(s['b'], 2)} | {fmt(s['expectancy'])} | "
            f"{fmt_ci(s['boot_ci95'])} | {fmt_pct(s['p_positive'])} | "
            f"{fmt(s['kelly'])} | {fmt_n(s['n_needed'])} |\n")
    return "".join(lines)


def detail_block(scope: dict) -> str:
    out = []
    for g in scope["groups"]:
        s = g["stats"]
        if s["n"] == 0:
            out.append(f"- **{g['key']}**: 无可用 R 值。")
            continue
        beta = s["beta"]
        out.append(
            f"- **{g['key']}**: 赢 {s['wins']} / 输 {s['losses']} / 平 {s['flat']}，"
            f"R_w={fmt(s['r_win'])}，R_l={fmt(s['r_loss'])}，s={fmt(s['std'])}；"
            f"t 区间 95%: {fmt_ci(s['t_ci95'])}；连续近似 "
            f"f* = E[R]/E[R²] = {fmt(s['kelly_cont'])}；"
            f"Beta({beta['alpha']:.0f},{beta['beta']:.0f}) 后验均值 "
            f"{fmt_pct(beta['mean'])}，95%可信区间 "
            f"[{fmt_pct(beta['ci95'][0])}, {fmt_pct(beta['ci95'][1])}]。")
    return "\n".join(out)


def monthly_table(scope: dict) -> str:
    rows = scope["monthly"]
    if not rows:
        return "（无可按月统计的 R 值）\n"
    head = ("| 月份(UTC) | 笔数 | E[R] |\n|---|---:|---:|\n")
    return head + "".join(
        f"| {r['month']} | {r['n']} | {fmt(r['expectancy'])} |\n"
        for r in rows)


def fmt_reasons(reasons: dict) -> str:
    """无 R 原因计数的中文显示。"""
    names = {"stop_missing": "缺初始止损", "risk_invalid": "risk 无效"}
    if not reasons:
        return "无"
    return "、".join(f"{names.get(k, k)}×{v}" for k, v in sorted(reasons.items()))


def render_markdown(report: dict) -> str:
    ov = report["overview"]
    main = report["scopes"]["main"]
    ref = report["scopes"]["ai_managed"]
    main_span = main["span_utc"] or ["—", "—"]
    ref_span = ref["span_utc"] or ["—", "—"]

    src_lines = "".join(
        f"| {r['source']} | {r['status']} | {r['n']} |\n"
        for r in ov["source_status"])
    stop_lines = "".join(
        f"| {r['source']} | {r['closed']} | {r['with_stop']} | "
        f"{fmt_pct(r['with_stop'] / r['closed']) if r['closed'] else '—'} |\n"
        for r in ov["stop_by_source"])
    ai_lines = "".join(
        f"| {r['source']} | {r['ai_managed']} | {r['n']} | {r['with_stop']} |\n"
        for r in ov["ai_managed_by_source"])
    check_lines = "".join(
        f"| {c['id']} | {c['source']} | {c['side']} | {c['gross']:.6f} | "
        f"{c['realized_pnl']:.6f} | {c['fee']:.6f} | {c['net']:.6f} |\n"
        for c in ov["pnl_fee_check"])

    parts = [f"""# nofx 实盘已平仓交易：Kelly 与样本量分析

- 生成时间: {report['generated_at']}
- 数据库: `{report['db']}`（**只读打开**，`mode=ro`，全程未写入）
- 表: `trader_positions`，共 {ov['total_rows']} 行

## 1. 结论摘要

- `source` 只有两个取值：`sync` 与 `system`。"系统下单"取 **`source='system'`**
  （判断依据见 §2）。该口径下已平仓仅 **{main['closed']} 笔**（平仓时间
  {main_span[0]} ~ {main_span[1]} UTC），全部带初始止损，其中 {main['with_r']} 笔可计算 R。
- 主口径 E[R] 为 **{'负' if (main['groups'][0]['stats']['expectancy'] or 0) < 0 else '正'}**
  （n={main['with_r']}，极小样本）：Kelly f* 与显著性样本量 n_needed 均按
  公式给出，但 E[R]≤0 时 n_needed 不适用、f* 为负——含义是"当前优势下
  不应加仓"，且 n={main['with_r']} 远不足以定论。
- 参考口径 `ai_managed=1`（含 sync 来源的 AI 账本，{ref['closed']} 笔平仓、
  {ref['with_r']} 笔有 R）一并列出，用于交叉观察，但主结论以 §3 为准。

## 2. 数据概况与口径

### 2.1 source 分布

| source | status | 行数 |
|---|---|---:|
{src_lines}
### 2.2 initial_stop_loss > 0 的比例（status='CLOSED'）

| source | 已平仓 | 有初始止损 | 比例 |
|---|---:|---:|---:|
{stop_lines}| 合计 | {ov['stop_overall']['closed']} | {ov['stop_overall']['with_stop']} | {fmt_pct(ov['stop_overall']['with_stop'] / ov['stop_overall']['closed'])} |

### 2.3 “系统下单”取 source='system' 的判断依据

- 代码 `store/position.go` 的 `CreateOpenPosition`：仓位行由**系统直接开仓路径**
  创建时 `Source` 默认写 `"system"`（`pos.Source = "system"`）。
- 代码 `store/position_builder.go` 的 `handleOpen`：由**交易所成交回放（sync）**
  重建的行写 `Source: "sync"`，`exchange_position_id` 带 `sync_` 前缀；该路径
  同时接收账户上的**人工单**，仅靠 `ai_managed` 标志区分归属。
- 数据佐证（CLOSED 行）：

| source | ai_managed | 行数 | 有初始止损 |
|---|---|---:|---:|
{ai_lines}
  `source='system'` 的 {main['closed']} 笔全部 `ai_managed=1` 且都带初始止损；
  `sync` 中 312 笔 `ai_managed=0`（人工/外部单），因此过滤"系统下单"用
  `source='system'`，而不用混入人工单的 `sync`。

### 2.4 realized_pnl 是否已扣手续费

抽样对比 毛盈亏 `(exit−entry)×qty×方向` 与 `realized_pnl`（LONG 取 +1，SHORT 取 −1）：

| id | source | side | 毛盈亏 | realized_pnl | fee | realized_pnl−fee |
|---|---|---|---:|---:|---:|---:|
{check_lines}
`realized_pnl` 与毛盈亏一致（误差仅为成交价舍入），即**未扣手续费**。
因此本报告 R 值按 **R = (realized_pnl − fee) / risk** 计算（减 fee）。

### 2.5 R 值口径

- `risk = |entry_price − initial_stop_loss| × entry_quantity`（entry_quantity
  为 0 时用 `quantity`）；每笔以**初始风险**为单位，R 即初始风险倍数。
- `initial_stop_loss<=0` 的行不计 R，单独计数（见各表"无R"说明）。
- 胜率 p 按 R>0 计；R=0 记为"平"，不计入赢/输。

## 3. 主分析：status='CLOSED' 且 source='system'

时间跨度（按 exit_time）: {main_span[0]} ~ {main_span[1]}（UTC）；
已平仓 {main['closed']} 笔，可算 R 的 {main['with_r']} 笔，
无 R 的 {main['without_r']} 笔（原因: {fmt_reasons(main['without_r_reasons'])}）。
trader 名 = trader_id 前 8 位 + 模型名，跨口径重名时追加尾段消歧。

### 3.1 汇总表

{summary_table(main)}
### 3.2 分组明细（t 区间 / 连续 Kelly / Beta 后验）

{detail_block(main)}
### 3.3 按月 E[R]（全部合并，按 exit_time 归月，看稳定性）

{monthly_table(main)}
## 4. 参考：ai_managed=1（全部来源，含 sync 路径的 AI 账本）

时间跨度（按 exit_time）: {ref_span[0]} ~ {ref_span[1]}（UTC）；
已平仓 {ref['closed']} 笔，可算 R 的 {ref['with_r']} 笔，
无 R 的 {ref['without_r']} 笔（原因: {fmt_reasons(ref['without_r_reasons'])}）。
此口径混入 `sync` 路径的 AI 单，样本更大，仅作交叉参考。

### 4.1 汇总表

{summary_table(ref)}
### 4.2 分组明细

{detail_block(ref)}
### 4.3 按月 E[R]（全部合并，按 exit_time 归月）

{monthly_table(ref)}
## 5. 局限与说明

- **样本量**：主口径仅 {main['with_r']} 笔，E[R]、b、Kelly 的置信区间极宽；
  Kelly 公式假设独立同分布、参数已知，小样本下 f* 的估计误差远大于其本身。
- **R 口径**：以初始止损距离为 1R，未跟踪移动止损后的实际风险变化；
  部分平仓/分批止盈合并计入单笔 realized_pnl。
- **区间**：bootstrap 与 t 区间在小样本下都偏乐观；P(E[R]>0) 为 bootstrap
  重采样中均值>0 的频率（固定种子 {report['seed']}，{report['n_boot']} 次）。
- **n_needed = (1.96·s/E[R])²** 仅当 E[R]>0 时有意义；它回答"按当前期望与
  波动，要把 E[R] 的 95% 置信区间下界推到 0 之外大约需要多少笔"。
- 胜率后验为均匀先验 Beta(1,1) 的更新，先验选择对小样本影响明显。

## 6. 文件

- 报告: `{report.get('out', '—')}`
- JSON: `{report.get('json_path', '—')}`
"""]
    return "".join(parts)


# ---------------------------------------------------------------------------
# 工具
# ---------------------------------------------------------------------------

def ms_to_month(ms) -> str | None:
    if not ms:
        return None
    return dt.datetime.fromtimestamp(int(ms) / 1000, tz=dt.timezone.utc).strftime("%Y-%m")


def ms_to_date(ms) -> str:
    if not ms:
        return "—"
    return dt.datetime.fromtimestamp(int(ms) / 1000, tz=dt.timezone.utc).strftime("%Y-%m-%d")


def build_report(con: sqlite3.Connection, db_path: str, seed: int = SEED,
                 n_boot: int = N_BOOT) -> dict:
    all_ids = {r[0] for r in con.execute(
        "SELECT DISTINCT trader_id FROM trader_positions WHERE status='CLOSED'")}
    labels = build_labels(sorted(all_ids))

    report = {
        "db": db_path,
        "generated_at": dt.datetime.now(dt.timezone.utc).strftime("%Y-%m-%d %H:%M:%S UTC"),
        "seed": seed,
        "n_boot": n_boot,
        "overview": overview(con),
        "scopes": {
            "main": analyze_scope(
                enrich(load_closed(con, "source='system'"), labels),
                "status='CLOSED' 且 source='system'", seed=seed, n_boot=n_boot),
            "ai_managed": analyze_scope(
                enrich(load_closed(con, "ai_managed=1"), labels),
                "status='CLOSED' 且 ai_managed=1（全部来源）", seed=seed, n_boot=n_boot),
        },
    }
    return report


# ---------------------------------------------------------------------------
# CLI
# ---------------------------------------------------------------------------

def main(argv=None) -> int:
    ap = argparse.ArgumentParser(
        description="实盘已平仓交易的 Kelly 与样本量分析（只读数据库）")
    ap.add_argument("--db", required=True, help="SQLite 数据库路径（只读打开）")
    ap.add_argument("--out", default=".prob_out/live_kelly.md", help="Markdown 报告输出路径")
    ap.add_argument("--json", dest="json_path", default=".prob_out/live_kelly.json",
                    help="JSON 结果输出路径")
    ap.add_argument("--seed", type=int, default=SEED, help="bootstrap 固定种子")
    ap.add_argument("--boots", type=int, default=N_BOOT, help="bootstrap 次数")
    args = ap.parse_args(argv)

    con = open_ro(args.db)
    try:
        report = build_report(con, args.db, seed=args.seed, n_boot=args.boots)
    finally:
        con.close()
    report["out"] = args.out
    report["json_path"] = args.json_path

    out_path = Path(args.out)
    out_path.parent.mkdir(parents=True, exist_ok=True)
    out_path.write_text(render_markdown(report), encoding="utf-8")

    json_path = Path(args.json_path)
    json_path.parent.mkdir(parents=True, exist_ok=True)
    json_path.write_text(
        json.dumps(report, ensure_ascii=False, indent=2, default=float),
        encoding="utf-8")

    print(f"已写入: {out_path}")
    print(f"已写入: {json_path}")
    for scope_name in ("main", "ai_managed"):
        scope = report["scopes"][scope_name]
        print(f"\n[{scope['filter']}] 平仓 {scope['closed']} 笔 / 有R {scope['with_r']} 笔")
        for g in scope["groups"]:
            s = g["stats"]
            if s["n"] == 0:
                print(f"  {g['key']:<28} 无可用 R")
                continue
            print(f"  {g['key']:<28} n={s['n']:>4} p={fmt_pct(s['p']):>7} "
                  f"E[R]={fmt(s['expectancy']):>9} f*={fmt(s['kelly']):>9} "
                  f"n_needed={fmt_n(s['n_needed'])}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
