"""信号过滤器可行性检验(需要 numpy + scikit-learn)。

三部分:(a) 单变量 Spearman 半段稳定性(描述性,不计入检验数);(b) 走步(expanding train)四个模型的
OOS AUC / rho / 保留 top70%、top50% 的 R 及置换检验 p 值;(c) 嵌套流程——每折只用过去数据按半段稳定性选一个
特征、用训练集中位数当阈值,与保留同样数量的时间块置换比较。最后对所有检验做 Bonferroni 并给出裁决。
"""
import math
import warnings

import numpy as np
from sklearn.ensemble import HistGradientBoostingClassifier
from sklearn.linear_model import LogisticRegression, Ridge
from sklearn.metrics import roc_auc_score
from sklearn.preprocessing import StandardScaler

warnings.filterwarnings("ignore")

LABELS = ("r_base", "r_plain")
CUTOFFS = (0.7, 0.5)
NESTED_DRAWS = 5000
HOUR_MS = 60 * 60 * 1000
MAX_HOLDING_HOURS = 72
EMBARGO_MS = MAX_HOLDING_HOURS * HOUR_MS
PERMUTATION_BLOCK_MS = 24 * HOUR_MS


def model_factories():
    return {
        "logit_L2": lambda: LogisticRegression(C=0.05, max_iter=2000),
        "logit_L1": lambda: LogisticRegression(C=0.05, penalty="l1", solver="liblinear"),
        "ridge": lambda: Ridge(alpha=50.0),
        "hgb_depth2": lambda: HistGradientBoostingClassifier(max_depth=2, learning_rate=0.05, max_iter=100,
                                                             min_samples_leaf=20),
    }


def _rank(a):
    a = np.asarray(a)
    order = np.argsort(a, kind="stable")
    ranks = np.empty(len(a), float)
    start = 0
    while start < len(a):
        end = start + 1
        while end < len(a) and a[order[end]] == a[order[start]]:
            end += 1
        ranks[order[start:end]] = (start + end - 1) / 2
        start = end
    return ranks


def spear(a, b):
    m = ~np.isnan(a) & ~np.isnan(b)
    if m.sum() < 20:
        return np.nan
    return np.corrcoef(_rank(a[m]), _rank(b[m]))[0, 1]


class Data:
    def __init__(self, rows, label, min_coverage=0.8):
        rows = sorted(rows, key=lambda r: r["t0_ms"])
        self.rows, self.n, self.label = rows, len(rows), label
        self.t0_ms = np.array([r["t0_ms"] for r in rows], dtype=np.int64)
        # ml-dataset records the label horizon in label_params.horizon_h.
        # Legacy datasets without metadata use the maximum holding period.
        self.horizon_ms = np.array([float(r.get("label_params", {}).get(
            "horizon_h", MAX_HOLDING_HOURS)) * HOUR_MS for r in rows])
        if not np.isfinite(self.horizon_ms).all() or (self.horizon_ms < 0).any():
            raise ValueError("label horizon must be finite and nonnegative")
        keys = sorted({k for r in rows for k in r["feat"]})
        cov = {k: sum(r["feat"].get(k) is not None for r in rows) / self.n for k in keys}
        self.keys = [k for k in keys if cov[k] >= min_coverage]
        self.X = np.array([[np.nan if r["feat"].get(k) is None else r["feat"][k] for k in self.keys] for r in rows], float)
        self.y = np.array([r[label] for r in rows], float)
        self.yb = (self.y > 0).astype(int)


def _prep(Xtr, Xte):
    med = np.nanmedian(Xtr, axis=0)
    med = np.where(np.isnan(med), 0, med)
    a, b = np.where(np.isnan(Xtr), med, Xtr), np.where(np.isnan(Xte), med, Xte)
    sc = StandardScaler().fit(a)
    return sc.transform(a), sc.transform(b)


def _edges(n, folds, min_train):
    start = int(n * min_train)
    return start, np.linspace(start, n, folds + 1).astype(int)


def purged_train_indices(d, test_start, embargo_ms=EMBARGO_MS):
    """Past labels must end before test start, with an additional embargo gap.

    Expanding training has no post-test observations; embargo is placed between
    each training label's end and test start. Equal t0 timestamps never split
    into train/test, even with a zero horizon and embargo.
    """
    if embargo_ms < 0:
        raise ValueError("embargo_ms must be nonnegative")
    before = np.arange(test_start)
    boundary = d.t0_ms[test_start]
    return before[(d.t0_ms[before] < boundary) &
                  (d.t0_ms[before] + d.horizon_ms[before] <= boundary - embargo_ms)]


def block_permutation(times_ms, rng, block_ms=PERMUTATION_BLOCK_MS):
    """Shuffle UTC time blocks, keeping chronological order inside each block."""
    if block_ms <= 0:
        raise ValueError("block_ms must be positive")
    times = np.asarray(times_ms, dtype=np.int64)
    order = np.argsort(times, kind="stable")
    if not len(order):
        return order
    buckets = times[order] // block_ms
    blocks = np.split(order, np.flatnonzero(np.diff(buckets)) + 1)
    return np.concatenate([blocks[i] for i in rng.permutation(len(blocks))])


def walk_forward(d, y, yb, make, folds, min_train, embargo_ms=EMBARGO_MS):
    start, edges = _edges(d.n, folds, min_train)
    pred = np.full(d.n, np.nan)
    for a, b in zip(edges[:-1], edges[1:]):
        if a == b:
            continue
        train = purged_train_indices(d, a, embargo_ms)
        if len(train) < 2:
            continue
        Xtr, Xte = _prep(d.X[train], d.X[a:b])
        m = make()
        if isinstance(m, Ridge):
            m.fit(Xtr, y[train])
            pred[a:b] = m.predict(Xte)
        else:
            if len(np.unique(yb[train])) < 2:
                continue
            m.fit(Xtr, yb[train])
            pred[a:b] = m.predict_proba(Xte)[:, 1]
    return pred, start


def score(pred, start, y, yb):
    p, yy, bb = pred[start:], y[start:], yb[start:]
    valid = np.isfinite(p) & np.isfinite(yy)
    p, yy, bb = p[valid], yy[valid], bb[valid]
    out = {"auc": roc_auc_score(bb, p) if len(set(bb)) > 1 else np.nan, "rho": spear(p, yy), "all": yy.sum()}
    for q in CUTOFFS:
        keep = p >= np.quantile(p, 1 - q) if len(p) else np.zeros(0, bool)
        t = int(q * 100)
        out[f"top{t}"], out[f"top{t}_n"] = yy[keep].sum(), int(keep.sum())
        out[f"top{t}_avg"] = yy[keep].mean() if keep.any() else np.nan
        out[f"lift{t}"] = out[f"top{t}"] - out["all"] * q if len(p) else np.nan
    return out


def univariate(d, out=print):
    h = d.n // 2
    out(f"\n[a] 单变量稳定性(描述性,不计入检验数):两个半段 Spearman 同号且 |rho|>=0.10")
    hits, expected = 0, 0.0
    for j, k in enumerate(d.keys):
        r1, r2, ra = spear(d.X[:h, j], d.y[:h]), spear(d.X[h:, j], d.y[h:]), spear(d.X[:, j], d.y)
        # 纯噪声下两半段同号且都 >=0.10 的概率(正态近似,rho 的标准差≈1/sqrt(n-1))
        t1, t2 = 0.10 * math.sqrt(max(h - 1, 1)), 0.10 * math.sqrt(max(d.n - h - 1, 1))
        expected += 2 * (0.5 * math.erfc(t1 / math.sqrt(2))) * (0.5 * math.erfc(t2 / math.sqrt(2)))
        if not (np.isnan(r1) or np.isnan(r2)) and np.sign(r1) == np.sign(r2) and min(abs(r1), abs(r2)) >= 0.10:
            hits += 1
            out(f"  {k:22s} H1={r1:+.2f} H2={r2:+.2f} all={ra:+.2f}")
    out(f"  命中 {hits} 个;纯噪声下期望约 {expected:.1f} / {len(d.keys)} 个")
    return hits, expected


def run_models(d, folds, min_train, perms, rng, names, out=print,
               embargo_ms=EMBARGO_MS, block_ms=PERMUTATION_BLOCK_MS):
    out(f"\n[b] 走步检验:expanding train,{folds} 折,OOS = 最后 {d.n - int(d.n * min_train)} 个信号,置换 {perms} 次")
    res = {}
    for name in names:
        make = model_factories()[name]
        pred, start = walk_forward(d, d.y, d.yb, make, folds, min_train, embargo_ms)
        s = score(pred, start, d.y, d.yb)
        null = {q: [] for q in CUTOFFS}
        for _ in range(perms):
            perm = block_permutation(d.t0_ms, rng, block_ms)
            pp, _ = walk_forward(d, d.y[perm], d.yb[perm], make, folds, min_train, embargo_ms)
            # Use the same OOS support as the observed fit (a permuted training
            # fold can become single-class and consequently unscorable).
            pp[~np.isfinite(pred)] = np.nan
            sp = score(pp, start, d.y, d.yb)
            missing_oos = np.any(np.isfinite(pred[start:]) & ~np.isfinite(pp[start:]))
            for q in CUTOFFS:
                null[q].append(np.nan if missing_oos else sp[f"lift{int(q * 100)}"])
        r = {"auc": s["auc"], "rho": s["rho"], "oos_all": s["all"]}
        for q in CUTOFFS:
            t = int(q * 100)
            r[f"lift{t}"] = s[f"lift{t}"]
            r[f"top{t}"] = s[f"top{t}"]
            draws = np.array(null[q])
            r[f"p{t}"] = ((np.sum((draws >= s[f"lift{t}"]) | ~np.isfinite(draws)) + 1) /
                          (perms + 1)) if np.isfinite(s[f"lift{t}"]) else 1.0
        res[name] = r
        out(f"  {name:11s} AUC={s['auc']:.3f} rho={s['rho']:+.3f} | OOS all={s['all']:+.1f}R | "
            f"top70%: {s['top70']:+.1f}R lift={s['lift70']:+.1f}R p={r['p70']:.3f} | "
            f"top50%: {s['top50']:+.1f}R lift={s['lift50']:+.1f}R p={r['p50']:.3f}")
    return res


def run_nested(d, folds, min_train, rng_seed, out=print,
               embargo_ms=EMBARGO_MS, block_ms=PERMUTATION_BLOCK_MS):
    start, edges = _edges(d.n, folds, min_train)
    keep = np.ones(d.n, bool)
    chosen = []
    for a, b in zip(edges[:-1], edges[1:]):
        if a == b:
            continue
        train = purged_train_indices(d, a, embargo_ms)
        h, best = len(train) // 2, None
        for j, k in enumerate(d.keys):
            r1, r2 = spear(d.X[train[:h], j], d.y[train[:h]]), spear(d.X[train[h:], j], d.y[train[h:]])
            if np.isnan(r1) or np.isnan(r2) or np.sign(r1) != np.sign(r2):
                continue
            sc = min(abs(r1), abs(r2))
            if best is None or sc > best[0]:
                best = (sc, j, np.sign(r1))
        if best is None:
            chosen.append("(none)")
            continue
        sc, j, sg = best
        thr = np.nanmedian(d.X[train, j])
        v = d.X[a:b, j]
        keep[a:b] = np.where(np.isnan(v), True, (v >= thr) if sg > 0 else (v <= thr))
        chosen.append(f"{d.keys[j]}({'+' if sg > 0 else '-'}{sc:.2f})")
    yy, kk = d.y[start:], keep[start:]
    rng = np.random.default_rng(rng_seed)
    null = np.array([yy[block_permutation(d.t0_ms[start:], rng, block_ms)][kk].sum()
                     for _ in range(NESTED_DRAWS)])
    p = (np.sum(null >= yy[kk].sum()) + 1) / (NESTED_DRAWS + 1)
    out(f"\n[c] 嵌套检验(每折只用过去数据选特征+阈值):各折选中 {chosen}")
    kept_avg = yy[kk].mean() if kk.any() else np.nan
    dropped_avg = yy[~kk].mean() if (~kk).any() else np.nan
    out(f"   保留 {int(kk.sum())}/{len(kk)}  保留 {yy[kk].sum():+.1f}R (avg {kept_avg:+.3f}) | "
        f"剔除 {yy[~kk].sum():+.1f}R (avg {dropped_avg:+.3f}) | 对时间块置换 p={p:.3f}")
    return {"p": p, "chosen": chosen, "kept_sum": float(yy[kk].sum()), "n_kept": int(kk.sum())}


def evaluate(rows, label, min_coverage=0.8, folds=5, min_train=0.4, perms=200, seed=7, models=None, out=print,
             embargo_ms=EMBARGO_MS, block_ms=PERMUTATION_BLOCK_MS):
    d = Data(rows, label, min_coverage)
    out(f"label={label} n={d.n} features={len(d.keys)} (覆盖率>={min_coverage:.0%})  meanR={d.y.mean():+.3f} win={d.yb.mean():.0%}")
    uni = univariate(d, out)
    names = list(models or model_factories())
    out(f"purge=标签持有窗口; embargo={embargo_ms / HOUR_MS:g}h; 时间置换块={block_ms / HOUR_MS:g}h")
    mres = run_models(d, folds, min_train, perms, np.random.default_rng(seed), names, out, embargo_ms, block_ms)
    nres = run_nested(d, folds, min_train, seed + 1, out, embargo_ms, block_ms)
    n_tests = len(names) * len(CUTOFFS) + 1
    return {"label": label, "n": d.n, "features": len(d.keys), "models": mres, "nested": nres,
            "tests": n_tests, "univariate": uni, "perms": perms}


def verdict(results, alpha=0.05, out=print):
    """results: {label: evaluate() 的返回}。Bonferroni 的检验数=所有 label 的检验之和。
    PASS 条件:嵌套检验和同一个模型在【每个 label 上】都 <= alpha/检验数;否则 NO DEPLOYABLE FILTER。"""
    tests = sum(r["tests"] for r in results.values())
    a_c = alpha / tests
    min_p = min(1 / (r["perms"] + 1) for r in results.values())
    out(f"\n[裁决] 检验数={tests}  Bonferroni 阈值 alpha={alpha}/{tests}={a_c:.4f}")
    if min_p > a_c:
        out(f"  注意:置换 p 值下限 1/(perms+1)={min_p:.4f} 高于阈值,模型检验不可能通过;需要 --perms >= {math.ceil(1 / a_c)}")
    nested_ok = {lab: r["nested"]["p"] <= a_c for lab, r in results.items()}
    model_ok = {}
    for lab, r in results.items():
        model_ok[lab] = {m for m, v in r["models"].items() if min(v["p70"], v["p50"]) <= a_c and max(v["lift70"], v["lift50"]) > 0}
        out(f"  {lab}: nested p={r['nested']['p']:.4f} {'通过' if nested_ok[lab] else '未过'} | "
            f"通过的模型: {sorted(model_ok[lab]) or '无'}")
    common = set.intersection(*model_ok.values()) if model_ok else set()
    if all(nested_ok.values()) and common and len(results) == len(LABELS):
        first = next(iter(results.values()))
        line = f"PASS: candidate filter {sorted(common)[0]} (nested: {first['nested']['chosen']})"
    elif len(results) < len(LABELS):
        line = "NO DEPLOYABLE FILTER (只检验了一个 label;PASS 需要两个 label 都通过,请用 --label both)"
    else:
        line = "NO DEPLOYABLE FILTER"
    out(line)
    return line


def run(rows, labels=LABELS, **kw):
    results = {}
    for lab in labels:
        results[lab] = evaluate(rows, lab, **{k: v for k, v in kw.items() if k != "alpha"})
        print()
    return results, verdict(results, kw.get("alpha", 0.05))
