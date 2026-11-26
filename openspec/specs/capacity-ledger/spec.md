# capacity-ledger Specification

## Purpose
TBD - created by archiving change add-key-capacity-contract. Update Purpose after archive.

## Requirements

### Requirement: 容量以「池 + 席位」两级建模

系统 SHALL 把用户容量建模为池、把 key 建模为池内席位：

- 池：`users.concurrency`（既有字段），语义为该用户**全部活跃 key 合计**的最大并发；
  `0` 表示不限池。
- 席位·保底：`api_keys.concurrency_request`，语义为该 key 保证拿到的并发；`0` 表示不占席位。
- 席位·硬顶：`api_keys.concurrency_limit`，语义为该 key 的上限；`0` 表示不额外限制
  （可借支到池上限）。

三个字段 SHALL 随认证快照下发，且快照版本号 MUST 同批递增。

#### Scenario: 存量 key 行为不变
- **WHEN** 既有 key 的 `concurrency_request` 与 `concurrency_limit` 均为 0
- **THEN** 其准入行为与改动前逐字一致（仅受池容量约束）
- **THEN** 不因新增字段被拒

#### Scenario: 快照版本递增
- **WHEN** 本次改动上线后首次鉴权
- **THEN** 旧版本快照 MUST 被判定过期并重建
- **THEN** 存在单测断言「旧版本快照不被采信」

### Requirement: 席位的分配不变量

系统 MUST 在写路径（创建/更新 key、变更池容量）事务内持锁校验以下不变量，任一违反即拒绝并
回传 `requested` / `remaining` / `capacity`：

1. 该用户全部 active key 的 `concurrency_request` 之和 ≤ 池容量（池 > 0 时）；
2. `key.concurrency_limit` ≤ 池容量（池 > 0 且硬顶 > 0 时）；
3. `key.concurrency_request` ≤ `key.concurrency_limit`（硬顶 > 0 时）。

系统 MUST NOT 静默截断或静默取较小值。

#### Scenario: 保底之和超池
- **WHEN** 池容量 5，已有两把 key 保底 3 与 2，再为第三把 key 申请保底 1
- **THEN** 拒绝，回传 `requested: 1`、`remaining: 0`、`capacity: 5`
- **THEN** 已存在的两把 key 不受影响

#### Scenario: 硬顶超过池
- **WHEN** 池容量 5，某 key 请求 `concurrency_limit: 10`
- **THEN** 拒绝，理由说明硬顶不得超过池容量

### Requirement: 剩余额度以查询聚合得出，不维护物化计数器

剩余可分配额度 SHALL 由「池容量 − 该用户活跃 key 的 request 之和」查询得出。
系统 MUST NOT 维护物化的"剩余额度"计数器作为权威来源。

#### Scenario: 删除 key 后额度自动归还
- **WHEN** 删除一把 `concurrency_request: 3` 的 key
- **THEN** 该用户的剩余可分配额度立即增加 3
- **THEN** 无需任何补偿任务或对账脚本
