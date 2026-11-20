## ADDED Requirements

### Requirement: 任务提交与查询

系统 SHALL 提供 `POST /v1/ciyuan/tasks` 提交任务、`GET /v1/ciyuan/tasks/{id}` 查询任务，
以既有 API key 鉴权（与 chat 入口同链、同无效 key 拒绝口径）。提交 MUST 接受
`external_id`（幂等键）、`model`、`messages`、`mode`（`now` | `defer`）、
`deadline`（`defer` 时必填）、可选 `max_tokens` 与 `price_policy`。
同一 `external_id` 重复提交 MUST 返回首次结果（`duplicate: true`），MUST NOT 产生第二个任务。

#### Scenario: 幂等重放
- **WHEN** 调用方以相同 `external_id` 再次提交
- **THEN** 返回首次任务的状态，`duplicate` 为 `true`
- **THEN** 不产生新的任务行，也不产生新的用量行

#### Scenario: defer 缺 deadline 被拒
- **WHEN** `mode=defer` 且未提供 `deadline`
- **THEN** 返回 400，错误码 `TASK_DEADLINE_REQUIRED`，不落库

### Requirement: 两种紧急度，默认与今日行为一致

`mode=now`（默认）SHALL 行为等价于一次同步调用：容量不足时按既有口径返回 429 + Retry-After，
MUST NOT 入队。`mode=defer` SHALL 在容量不足时保持 `pending` 并退避重试，直到拿到槽位
或到达 `deadline`；到达 `deadline` 仍未执行 MUST 置 `failed(reason=deadline_exceeded)`
并记录原因。任何任务 MUST NOT 静默滞留：`pending` 超过 `deadline` 必须落终态并留下原因。

#### Scenario: now 模式容量不足
- **WHEN** 以 `mode=now` 提交且当前无可用槽位
- **THEN** 返回 429 + Retry-After，与同步 chat 路径口径一致
- **THEN** MUST NOT 创建任务行

#### Scenario: defer 到期未完成
- **WHEN** `mode=defer` 的任务到达 `deadline` 仍无可用槽位
- **THEN** 状态置 `failed`，`reason` 为 `deadline_exceeded`
- **THEN** 查询端点 MUST 能返回该原因，且 `task_attempts` 有对应记录

### Requirement: 价格感知推迟受截止时间硬约束

对 `mode=defer` 且带 `price_policy` 的任务，系统 SHALL 比较"此刻执行的估价"与"最近低谷窗口的估价"，
当且仅当**节省比例达到 `min_saving`** 且 **`next_offpeak_start` + 预计耗时 <= `deadline`** 时，
才 SHALL 推迟。推迟 MUST NOT 发生在截止时间不允许的情况下。

#### Scenario: 允许推迟
- **WHEN** 当前处于高峰（因子 2.0）、`deadline` 距低谷窗口开始还有 3 小时、预计耗时 10 分钟、
  节省比例 50% ≥ `min_saving`
- **THEN** 任务置 `deferred_until = next_offpeak_start`，`price_window` 记录为高峰
- **THEN** 查询端点暴露 `estimated_cost_now` 与 `estimated_cost_offpeak`

#### Scenario: 不允许推迟
- **WHEN** `next_offpeak_start + 预计耗时 > deadline`
- **THEN** 任务 MUST 立即执行，MUST NOT 推迟
- **THEN** 响应中说明推迟被截止时间否决（可观测原因，不是静默）

### Requirement: 估价不冒充账单

提交/查询响应中的费用字段 MUST 以 `_estimate` 结尾，MUST 附带当前价格窗口标识；
任务完成后系统 SHALL 写入 `actual_cost`（来自既有用量记录），并使二者可同时读取。
系统 MUST NOT 将估价作为账单展示或对账依据。

#### Scenario: 完成后的估价与实扣
- **WHEN** 任务完成
- **THEN** 响应同时含 `estimated_cost`（`max_tokens` 上界口径）与 `actual_cost`（用量行口径）
- **THEN** 二者允许不相等，且不构成错误

### Requirement: 任务车道复用既有容量闸与写账路径

任务发起 MUST 经过既有 per-account 容量闸（`tryAcquireAccountSlot`）与既有转发/计量路径；
MUST NOT 自建并发计数、MUST NOT 自写用量行。任务产生的用量 MUST 与同步请求同表同字段，
并可从 `request_id` 回溯到任务记录。

#### Scenario: 任务不绕过 AIMD
- **WHEN** 某账号处于 AIMD 熔开状态
- **THEN** 任务车道对该账号的尝试 MUST 与同步请求受到同一约束
- **THEN** 不存在"任务车道能挤进同步请求进不去的账号"的情形
