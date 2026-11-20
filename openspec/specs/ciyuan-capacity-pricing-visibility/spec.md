# ciyuan-capacity-pricing-visibility Specification

## Purpose
规定能力快照里价格窗口与吞吐观测的只读、advisory 语义：调用方可以看到当前窗口的配置价格与实测吞吐，但这些读数 SHALL NOT 反过来影响调度或计费判定。

## Requirements

### Requirement: 能力快照暴露价格窗口与吞吐观测（只读、advisory）

能力快照 SHALL 新增 `pricing` 与 `capacity` 两段，供调用方在提交前决定「现在发」还是「推迟到低谷」：

- `pricing`：当前是否处于高峰（`peak: bool`）、当前生效的**复合**倍率（`multiplier: number`，
  = 分组峰率 × DeepSeek 官方峰谷因子）、低谷窗口的下一个起止（`next_offpeak_start` /
  `next_offpeak_end`，RFC3339）、时区（`timezone`）。数据源 MUST 复用既有倍率函数，
  MUST NOT 新增配置项，MUST NOT 缓存整窗（现读现算，避免与账单价漂移）。
- `capacity`：对该 key **近 7 天用得最多的那一个模型**给出的观测值——可持续速率
  `sustainable_rps`（分钟级 p95）、失败占比 `error_ratio`（分母含成功）、样本量
  `sample_active_minutes`（有请求的分钟数）、置信度 `confidence`（`low` / `medium` / `high`，
  按样本量分档）。样本不足时数值 MUST 为 `null`（**不是 0**），且 `confidence` MUST 退回 `low`。

两段 MUST 标注 `advisory: true`：系统 MUST NOT 承诺 `capacity` 在请求发出时仍然成立
（真实闸门是发起时的容量闸）。

`capacity` 段 MUST NOT 暴露账号数、槽位数或 `account.Concurrency`——这些属于账号层且随时会变，
写进对外契约等于把易变拓扑固化成承诺。观测值只回答「最近跑得动多快」，不回答「现在还空着几个位置」。

> 与 S1 设计的差异（**实现时改判，以此处为准**）：`design.md` 的迁移计划原写 `headroom`
> （按渠道给 `available_slots` / `in_use_slots`）。那个形状要求暴露实时槽位，与上面这条
> 「不暴露槽位」的纪律直接冲突，且「无法确定时给 null」掩盖不了「确定时就是把拓扑发出去」。
> 故实现改为观测口径并改名 `capacity`，`headroom` 不再出现在响应里。

#### Scenario: 高峰时刻的定价段

- **WHEN** 调用方在 DeepSeek 高峰时段（UTC 01:00–04:00 工作日）读取快照
- **THEN** `pricing.peak` 为 `true`，`pricing.multiplier` **大于 1.0**，且等于
  「分组峰率 × 2.0」（分组峰率为 1.0 时是 2.0；分组自身处于高峰时更高）
- **THEN** `pricing.next_offpeak_start` 给出下一个低谷开始的 RFC3339 时间；已在低谷时给出
  **当前这段**低谷的起点与终点（调用方的问题是「什么时候能便宜地跑」，答案就是「现在，直到 X 点」）

#### Scenario: 余量未知

- **WHEN** 某模型的观测样本不足（有请求的分钟数 < 60），或统计后端未装配 / 取数失败
- **THEN** `capacity.sustainable_rps` 与 `capacity.error_ratio` MUST 为 `null`，`confidence` 为 `low`
- **THEN** MUST NOT 返回 `0` 冒充「没有容量」或「已满」
