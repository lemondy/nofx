# 2026-10-06 独立复核证据

业务源码为 abba5978，原报告为 113bbb39。`*_test.go.txt` 是复核用例快照，不参与常规包测试；通过 Go overlay 临时加入对应包，无需修改业务源码。

这些用例断言“缺陷存在”以验证报告证据，修复业务代码后应改写为“缺陷消失”的正式回归用例，不能将本目录的预期用于长期 CI。

在仓库根目录生成 overlay 并重跑：

```sh
python3 - <<'PY'
import json
from pathlib import Path
root = Path.cwd().resolve()
evidence = root / 'docs/architecture/review-2026-10-06/evidence'
pairs = [('trader', 'trader'), ('market/breakout', 'market'),
         ('kernel', 'kernel'), ('store', 'store'), ('api', 'api')]
overlay = {'Replace': {
    str(root / package / 'recheck_overlay_20261006_test.go'):
    str(evidence / (name + '_recheck_test.go.txt'))
    for package, name in pairs
}}
Path('/private/tmp/nofx-recheck-overlay.json').write_text(json.dumps(overlay))
PY
go test -overlay /private/tmp/nofx-recheck-overlay.json \
  ./trader ./market/breakout ./kernel ./store ./api \
  -run '^TestRecheck06' -count=1 -v
```

基线构建与回归：

```sh
go build ./...
go test ./trader ./market/breakout ./kernel ./store ./api \
  ./trader/kucoin ./trader/gate ./trader/hyperliquid \
  ./trader/bybit ./trader/aster ./trader/okx -count=1
```

实际测试记录为 `reproduction-output.txt`、`suite-output.txt`。Go 1.25.3 / darwin arm64。完整回归包含部分既有公开行情请求；独立复现全部使用内存 transport 或内存 HTTP recorder，worker panic 在子进程中发生。

`production-output.txt` 只保留指定 PID、二进制元数据/指纹与明确窗口的日志统计，不包含凭据、持仓数据或数据库内容。仅记录复核时观测，不能替代可复现的干净构建清单。
