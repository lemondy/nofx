# 三推研究 v2 小型汇总

主报告：[threepush-study.md](../threepush-study.md)；协议：[threepush-preregistration.md](../threepush-preregistration.md)。报告先列校准结果。

|文件|内容|
|---|---|
|summary.json|真实检验数、训练 BH 通过、保留确认、默认有效清单及校准裁决|
|null.json|正式十种子大面板校准；逐族可估计数、训练拒绝数及保留确认数|
|planted.json|同量级植入面板的检出数及同一训练/保留候选键|
|preregistration-lock.json|真实目标重跑前的代码、协议、校准及尺度哈希|
|default-tests.csv|默认参数的全部预设方向、周期和期限，仅含匹配 Y1 推断族|
|class-estimates.csv|各类形态原始数、匹配数、日期集中度及训练/保留点估计|
|geometry-descriptive.csv|Y2、rr_space、Y3、先到占比等点估计与非缺失样本数，无推断统计|
|v1-v2.csv|两个版本的校准与真实结论对照|
|verification.json|测试、因果性检查、冻结及归档哈希核验|

`difference` 是形态 Y1 减五个匹配对照的均值，原始单位为 log return，乘 10,000 为 bps；不是可交易净收益。`n` 按该期限有完整标签的形态计，`matched_n` 是匹配差非缺失数。无匹配或不足检验门槛不能解释为没有效应。保留 t 空白表示该项未成为训练冻结候选。

几何目标由形态几何机械决定，不构成预测证据；不进入 BH，不判“有效/不支持”。H3 的空间差只作几何事实描述。H5 是 medium 内四个预设要素在匹配 Y1 上的条件增量。

v1 真实结果及保留窗已被看过；v2 依修正定义评估一次，不是全新独立保留样本。v1 原文、代码、测试、报告、锁、汇总及大产物完整保存在工作树 `artifacts/threepush/out_v1/`，`archive-manifest.json` 为逐文件 SHA256。v2 大面板逐种子全表、尺度来源清单、真实逐形态数据、候选锁及运行日志在 `artifacts/threepush/out_v2/`。

大产物（逐检验 CSV、事件表、缓存）保存在本地 `data/regime_study/threepush/out_v1|out_v2/`（不入库）；本目录仅含小型汇总。
