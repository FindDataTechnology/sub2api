## ADDED Requirements

### Requirement: key 可声明容量契约

`api_keys` SHALL 支持三个可配字段：`concurrency_limit`（并发上限，非负整数，**0 表示不限**）、
`rpm_limit`（每分钟请求上限，非负整数，**0 表示不限**）、`effective_at`
（生效时刻，`null` 表示创建即生效）。三个字段 SHALL 随认证快照下发，且快照版本号 MUST 同批递增。

#### Scenario: 存量 key 行为不变
- **WHEN** 既有 key 的三个新字段为默认值（0 / 0 / null）
- **THEN** 其准入行为与本次改动前逐字一致，不因新字段被拒

#### Scenario: 快照版本递增
- **WHEN** 本次改动上线后首次鉴权
- **THEN** 旧的同号快照 MUST 被判定为过期并重建
- **THEN** 存在单测断言「旧版本快照不被采信」

### Requirement: key 级准入与超限语义

系统 SHALL 在 `apiKeyAuth` 之后、分组白名单门之前，按 key 的 `concurrency_limit` 与
`rpm_limit` 做准入。超限 MUST 返回与既有 429 同形的错误体，并 MUST 带
`retry_after` 与 `limit_kind`（`concurrency` | `rpm`），使调用方能区分"我方限流"与"上游满"。

#### Scenario: 并发超限
- **WHEN** 某 key 的进行中请求数达到 `concurrency_limit`
- **THEN** 新请求返回 429，`limit_kind=concurrency`，带 `retry_after`
- **THEN** 上游 MUST NOT 被触达（不产生用量行、不占账号槽位）

#### Scenario: rpm 超限
- **WHEN** 某 key 在滑动一分钟内的请求数达到 `rpm_limit`
- **THEN** 新请求返回 429，`limit_kind=rpm`
- **THEN** 该 429 MUST 计入 `ops_error_logs`，使 key 稳定性口径可见

### Requirement: 限流后端不可用时放行并可观测

当并发/rpm 计数后端不可用时，系统 MUST 放行请求（fail-open），并 MUST 递增可观测计数器。
系统 MUST NOT 因计数后端故障返回 429。

#### Scenario: 计数后端故障
- **WHEN** 计数后端超时或不可用
- **THEN** 请求正常进入后续链路（不返 429）
- **THEN** 失败计数器递增，可在指标中看到该窗口内的 fail-open 次数
- **THEN** `GET /api/v1/keys/{id}/state` 的计数后端状态显示为不可用

### Requirement: 生效时刻语义

`effective_at` 之前，key 的鉴权 SHALL 通过但请求 MUST 被拒绝，错误码 `KEY_NOT_YET_EFFECTIVE`，
并回传 `effective_at`。系统 MUST NOT 用 401 表示"时间未到"（身份本身有效）。

#### Scenario: 未到生效时刻
- **WHEN** 调用方使用 `effective_at` 在未来的 key
- **THEN** 返回拒绝，错误码 `KEY_NOT_YET_EFFECTIVE`，响应含 `effective_at`
- **THEN** 不产生用量行、不触达上游

### Requirement: 配置变更的传播状态可查

系统 SHALL 提供 `GET /api/v1/keys/{id}/state`，回报：`config_applied`（当前生效配置是否与库中一致）、
`observed_snapshot_version`、计数后端类型与可用性。目的是让运维能回答"我改的生效了吗"，
而不是靠等与猜。

#### Scenario: 改完配置立即查询
- **WHEN** 管理员把 `concurrency_limit` 从 5 改为 20 并立即查询 state
- **THEN** 若快照尚未重建，`config_applied` 为 `false` 且回报 `observed_snapshot_version`
- **THEN** 快照重建后同一查询返回 `config_applied: true`
