## ADDED Requirements

### Requirement: 新 key 的基础情况来自群体基线，且必须标注置信度

对新建（或历史样本不足的）key，系统 SHALL 给出 `baseline` 段：`sustainable_rps`、
`p50_latency_ms`、`p95_latency_ms`、`error_ratio`，并 MUST 附 `source`（`population` | `personal`）
与 `confidence`（`low` | `medium` | `high`）。

基线 SHALL 取自该 key 所属 `(platform, model)` 类的群体统计，且 MUST 用**中位数**而非均值。
当群体样本不足以支撑一个置信档时，`confidence` MUST 为 `low`，且数值字段 MUST 为 `null`
或区间——系统 MUST NOT 在小样本下给出看似精确的数字。

#### Scenario: 全新 key 的首次查询
- **WHEN** 新建 key 尚无任何调用记录时查询其基线
- **THEN** 返回 `source: "population"`，并给出其类的群体中位数
- **THEN** 若该类样本不足，`confidence: "low"` 且数值为 `null`

#### Scenario: 个人样本足够后切换
- **WHEN** 该 key 自身累计调用达到阈值
- **THEN** `source` 变为 `personal`，数值改用该 key 自身统计
- **THEN** `confidence` 随样本量提升

### Requirement: 稳定性口径统一且分母含失败

系统 SHALL 按 key 提供 `stability` 段，含 `success_ratio`、`p95_latency_ms`、
`throttle_ratio`、`active_hours`，窗口支持 1h / 24h / 7d。

`success_ratio` 的分母 MUST 包含失败尝试。系统 MUST NOT 仅用成功计费行计算成功率
（那会恒得 100%）。任一来源缺失时该指标 MUST 为 `null`。

#### Scenario: 有失败时不得显示 100%
- **WHEN** 某 key 在 24h 内有成功调用也有 429/5xx
- **THEN** `success_ratio` 严格小于 1
- **THEN** `throttle_ratio` 大于 0

#### Scenario: 无数据
- **WHEN** 某 key 在窗口内无任何调用与错误记录
- **THEN** 各指标为 `null`，MUST NOT 为 `0` 或 `1`
- **THEN** 响应中说明"无数据"而非"稳定"
