# 74a5daae 修复后独立验证

源码基线 `74a5daae0c5a8c74029e46eeeb3237713a492c16`，Go 1.25.3 / darwin arm64。

`*_test.go.txt` 是隔离验证源码快照，使用 Go overlay，不参与常规包测试。trader/market 快照依赖本提交正式测试中的 fix06Mock、fix06Transport 等 helper；kernel 复用上一轮两个用例，证明 C2 和 BTC 窗口问题仍在。

回调顺序和 kernel 用例断言残留缺陷存在，不能直接作为期望修复的 CI 断言。其余独立用例断言修复后的正确行为。worker panic 均限制在测试子进程中，使用内存 transport，不触达实际交易所。

在仓库根目录生成 overlay：

```sh
python3 - <<'PY'
import json
from pathlib import Path
root = Path.cwd().resolve()
new = root / 'docs/architecture/review-2026-10-06/fix-verification'
old = root / 'docs/architecture/review-2026-10-06/evidence'
pairs = [('trader', new / 'trader_verify_test.go.txt'),
         ('market/breakout', new / 'market_verify_test.go.txt'),
         ('kernel', old / 'kernel_recheck_test.go.txt')]
Path('/private/tmp/nofx-fix-verify-overlay.json').write_text(json.dumps({
    'Replace': {str(root / pkg / 'verify_overlay_20261006_test.go'): str(src)
                for pkg, src in pairs}}))
PY
go test -overlay /private/tmp/nofx-fix-verify-overlay.json \
  ./trader ./market/breakout ./kernel \
  -run '^(TestVerify06|TestRecheck06)' -count=1 -v
```

本轮第一批独立测试后另补 peer 三消费点用例，记录合并在 independent-output.txt。正式验证命令及输出保存在 validation-output.txt；所有记录退出码为 0。
