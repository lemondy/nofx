# 与 qlib 的数值对拍

用 qlib 自己的算子（`qlib/data/ops.py`）和我们的表达式引擎在同一份随机行情上计算，逐项比较。

```sh
git clone --depth 1 https://github.com/microsoft/qlib.git /tmp/qlib
python -m venv /tmp/qv && /tmp/qv/bin/pip install numpy pandas scipy pyyaml requests pydantic-settings \
    redis dill fire python-redis-lock tqdm pymongo loguru joblib filelock pyarrow
cd /tmp/qlib && PYTHONPATH=.:<repo>/scripts/factor_mining/audit_qlib /tmp/qv/bin/python \
    <repo>/scripts/factor_mining/audit_qlib/audit_alpha158.py <repo> out.json   # 种子库 vs Alpha158DL
    ... audit_ops.py <repo>                                                      # 共有算子逐个对拍
```

`shim.py` 用 numpy 代替 qlib 的两个 Cython 模块（滚动 OLS：Slope/Rsquare/Resi），按 `.pyx` 的数学实现；其余算子是 qlib 原代码。
不属于 CI（需要 qlib 源码）；改动 `expr.py` / `zoo.py` 后手动跑一遍。
