# 2026-10-05 审查复现证据

基线提交：`4cf16c408c31b9e58127f358b884872a9db4869a`，分支 `dev`。

本目录保存临时审查测试的源码文本、原始失败输出和原有测试通过输出。以上基线审查阶段没有修改生产逻辑。`.go.txt` 不参与常规 Go 测试编译。

修复后的说明见 [修复记录](/Users/zhangyun/workspace/nofx/docs/architecture/QUANT_FIXES_2026-10-05.md)。新增 `fixes-suite-output.txt` 和 `fixes-race-output.txt` 保存当前实现的通过输出，原始基线及失败输出保留不变。

源码文本已执行 gofmt，原始输出中的临时测试行号对应执行时的格式；用例名和断言行为一致。

- `trader_audit_test.go.txt`：6 个执行/风控边界用例；借用仓库现有 `riskTestTrader` 和 `uptrend1dData` 测试辅助函数。
- `kernel_audit_test.go.txt`：未知规则字段用例。
- `breakout_audit_test.go.txt`：时间桶、参数保存失败与弱相关用例。
- `reproduction-output.txt`：10 个用例在基线实现上均触发预期失败；用于证明行为缺口，不表示已修复。
- `baseline-output.txt`：移除临时用例后的现有回归测试结果。

以下复现命令只适用于上述基线提交的 checkout，且要求三个临时路径不存在。当前修复工作树已有正式回归用例，同名测试不应再复制进去：

```sh
cp docs/architecture/review-2026-10-05/evidence/trader_audit_test.go.txt trader/review_20261005_temp_test.go
cp docs/architecture/review-2026-10-05/evidence/kernel_audit_test.go.txt kernel/review_20261005_temp_test.go
cp docs/architecture/review-2026-10-05/evidence/breakout_audit_test.go.txt market/breakout/review_20261005_temp_test.go
go test ./trader ./kernel ./market/breakout -run '^TestAudit05' -count=1 -v
```

检查结果后删除刚复制的三个临时文件即可恢复普通测试范围。测试使用模拟交易接口和临时文件，不提交真实交易单；短相关性用例只验证统计准入条件，不评估策略收益。

现有回归命令：

```sh
go test ./kernel ./market/... ./trader/... ./store/... ./api ./manager
```

部分测试使用本地 HTTP 模拟服务，需要允许运行本地监听；它们不构成真实交易所集成验证。
