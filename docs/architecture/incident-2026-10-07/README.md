# 挂单秒撤隔离复现

运行版本：74a5daae0c5a8c74029e46eeeb3237713a492c16。所有用例均使用虚构行情/订单；复核读取真实日志和数据库的部分不涉及修改。

market 用实际 executionTimeframeData 生成 100×3m→20×15m fixture；kernel 用实际 ComputeSymbolSignals 生成数据不足禁开码；trader 仅在 getMarketData 读取边界注入同一 fixture，真实挂单管理函数观察撤单、终态回读及 pending 清理。BTC 过滤和顶部确认在该模拟中关闭，以隔离 15m 数据量问题。

从仓库根目录重跑，先恢复生产版本的源码到 overlay，避免当前未提交修复影响结果：

```sh
python3 - <<'PY'
import json
import subprocess
from pathlib import Path
root = Path.cwd().resolve()
out = Path('/private/tmp/nofx-incident-74a5')
out.mkdir(exist_ok=True)
evidence = root / 'docs/architecture/incident-2026-10-07'
replace = {}
changed = subprocess.check_output(
    ['git', 'diff', '74a5daae', '--name-only', '--', '*.go'], text=True).splitlines()
for rel in changed:
    p = out / rel
    p.parent.mkdir(parents=True, exist_ok=True)
    try:
        data = subprocess.check_output(['git', 'show', '74a5daae:' + rel],
                                       stderr=subprocess.DEVNULL)
    except subprocess.CalledProcessError:
        replace[str(root / rel)] = ''
        continue
    p.write_bytes(data)
    replace[str(root / rel)] = str(p)
for rel in subprocess.check_output(
        ['git', 'ls-files', '--others', '--exclude-standard', '--', '*.go'],
        text=True).splitlines():
    replace[str(root / rel)] = ''
rel = 'trader/auto_trader_marketdata.go'
raw = subprocess.check_output(['git', 'show', '74a5daae:' + rel], text=True)
sig = 'func (at *AutoTrader) getMarketData(symbol string) (*market.Data, error) {'
raw = raw.replace(sig,
    'var incident07MarketData func(string) (*market.Data, error)\n\n' + sig +
    '\n\tif incident07MarketData != nil { return incident07MarketData(symbol) }', 1)
p = out / rel
p.parent.mkdir(parents=True, exist_ok=True)
p.write_text(raw)
replace[str(root / rel)] = str(p)
for pkg in ['market', 'kernel', 'trader']:
    replace[str(root / pkg / 'incident_overlay_20261007_test.go')] = str(
        evidence / (pkg + '_test.go.txt'))
(out / 'overlay.json').write_text(json.dumps({'Replace': replace}))
PY
go test -overlay /private/tmp/nofx-incident-74a5/overlay.json \
  ./market -run '^TestIncident07ExecutionFixture$' -count=1 -v
go test -overlay /private/tmp/nofx-incident-74a5/overlay.json \
  ./kernel -run '^TestIncident07PendingDataInsufficient$' -count=1 -v
go test -overlay /private/tmp/nofx-incident-74a5/overlay.json \
  ./trader -run '^TestIncident07PendingCancellation$' -count=1 -v
```

必须依上述顺序运行，后两包读取第一包生成的 `/private/tmp/nofx-incident-execution-fixture.json`。测试通过表示缺陷在该历史版本复现，不表示已修复。

cancellation-pairs.tsv 是当日日志在 00:00–06:15（Asia/Shanghai）内按订单 ID 匹配的 29 条成功挂单和程序撤单记录；输出省略订单 ID，保留符号、时间、间隔和日志行号。成功撤单日志未带原因，不能用该表直接推导每笔订单首个撤单码。
