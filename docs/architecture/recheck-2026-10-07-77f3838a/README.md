# 77f3838a 最新修复独立复核证据

源码基线 `77f3838af41a19c2495da0a897256be87bf4f375`；Go 1.25.3 / darwin arm64。报告见上级目录 `CODE_REVIEW_2026-10-07_RECHECK.md`。

`*_test.go.txt` 是隔离测试快照，不进入常规 Go 包构建。`make_overlay.py` 在 `/private/tmp/nofx-audit77` 生成两份带边界 hook 的源码副本和 overlay 映射；不改仓库业务源码。hook 分别用于 BTC regime closes、策略取数响应和后续 generic 执行报价响应；保留真实的 pending 生命周期、策略参数展开、adapter 报价检查、信号计算及执行端授权函数。调参器通过内存 HTTP transport 返回历史价格/资金费，journal 和历史池写入各测试临时目录。没有向实盘交易所提交订单。

新测试同时验证修复行为和残留缺陷。`TestAudit77Residual*` 刻意断言缺陷的当前行为，PASS 是复现成功；修复后应改写为正确行为断言。其余新用例验证恰好 60 根、15m/5m 配置的真实取数接线、micro-trend 启用、暖缓存 singleflight、同 symbol hedge 查询去重和真正发生在 HTTP 阻塞期间的追加/合并。

复用上一轮 `review-2026-10-06/fix-verification` 的负数量空头、父 panic 和 worker 子进程测试。旧回调残留用例已过时，此轮 **不执行**，回调顺序由最新正式 Fix06 测试验证。

在仓库根目录运行：

```sh
python3 docs/architecture/recheck-2026-10-07-77f3838a/make_overlay.py
go test -overlay /private/tmp/nofx-audit77/overlay.json \
  ./kernel ./market/breakout ./trader \
  -run '^(TestAudit77|TestVerify06(Healthy|Short|Parent|WorkerRecovery))' -count=1 -v
go test -race -overlay /private/tmp/nofx-audit77/overlay.json \
  ./kernel ./market/breakout ./trader -run '^TestAudit77' -count=1 -v
```

现存记录中，`independent-output.txt` 为上述普通验证，但执行账户去重用例是随后补充的，记录在 `final-targeted-output.txt`。后者也对恰好 60 根 fixture 再做 race 验证；`independent-race-output.txt` 的第一版接线 fixture 为 62 根，最终已截为请求的恰好 60 根。以上全部退出码为 0。

正式验证命令与输出在 `validation-output.txt`；全项目构建和五包完整测试通过，修复相关正式 race 测试通过。重跑时不得把复现类 PASS 用作问题清零证明，也不应将模拟授权通过等同于已观察到实盘订单绕过。
