## ADDED Requirements

### Requirement: 能力快照暴露价格窗口与渠道余量（只读、advisory）

能力快照 SHALL 新增 `pricing` 与 `headroom` 两段，供调用方在提交前决定 `now` 还是 `defer`：

- `pricing`: 当前是否处于高峰（`peak: bool`）、当前生效倍率（`multiplier: number`）、
  低谷窗口的下一个起止（`next_offpeak_start` / `next_offpeak_end`，RFC3339）、时区。
- `headroom`: 按渠道（平台）给出可用槽位估算 `available_slots` 与正在使用的槽位
  `in_use_slots`；无法确定时 MUST 为 `null`（不是 0）。

两段 MUST 标注为 advisory：系统 MUST NOT 承诺 `headroom` 在请求发出时仍然成立
（真实闸门是发起时的容量闸）。

#### Scenario: 高峰时刻的定价段
- **WHEN** 调用方在 DeepSeek 高峰时段（UTC 01:00–04:00 工作日）读取快照
- **THEN** `pricing.peak` 为 `true`，`pricing.multiplier` 为高峰因子（2.0）
- **THEN** `pricing.next_offpeak_start` 给出下一个低谷开始的 RFC3339 时间

#### Scenario: 余量未知
- **WHEN** 某渠道的槽位统计不可得（例如统计后端未装配）
- **THEN** `headroom.<platform>.available_slots` MUST 为 `null`
- **THEN** MUST NOT 返回 `0` 冒充"已满"
