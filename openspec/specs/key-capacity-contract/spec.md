# key-capacity-contract Specification

## Purpose
TBD - created by archiving change add-key-capacity-contract. Update Purpose after archive.

## Requirements

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

### Requirement: 有效上限必须如实暴露（含继承）

能力快照与 `state` 端点 SHALL 回报 key 的**有效**上限，而不只是 key 列上的原始值：
`effective_concurrency_limit`、`effective_rpm_limit` 与各自的 `limit_source`
（`key` | `user` | `group` | `unlimited`）。

当 key 列值为 0（不限）但 user/group 层有限制时，`effective_*` MUST 反映该限制，
且 `limit_source` MUST 指向实际生效的那一层。系统 MUST NOT 在 key 列为 0 时报"不限"
而实际受 user 层约束。

#### Scenario: key 不限但 user 限 5
- **WHEN** 某 key 的 `concurrency_limit=0`，其 user 的 `Concurrency=5`
- **THEN** `effective_concurrency_limit` 为 5，`limit_source` 为 `user`
- **THEN** 响应中 MUST NOT 出现"不限"的表述

#### Scenario: 超限时说明是哪一层
- **WHEN** 请求因并发超限被拒
- **THEN** 错误体带 `limit_kind=concurrency` 与 `limit_scope`（`key` | `user` | `group`）
- **THEN** 调用方能据此判断是削减自己的并发还是申请更高额度

### Requirement: 两级准入（全局硬顶 + 该 key 硬顶），总额不可绕过

准入 SHALL 只判两条：

```
(池容量 == 0 ∨ userInflight < 池容量)     // ① 全局硬顶
∧ (硬顶 == 0 ∨ keyInflight < 硬顶)          // ② 该 key 硬顶
```

系统 MUST 保证：无论用户创建多少把 key，该用户合计并发 MUST NOT 超过池容量。

#### Scenario: 多 key 不能绕过池容量
- **WHEN** 池容量 5，用户创建 10 把 key（各自 request=0、limit=0）
- **THEN** 该用户合计并发 MUST NOT 超过 5
- **THEN** 不存在「10 把 key 各跑 5」

#### Scenario: 硬顶独立生效
- **WHEN** 池容量 5，key A 设 `concurrency_limit: 2`，其余 key 空闲
- **THEN** A 最多用 2，MUST NOT 用满 5

### Requirement: 保底的语义是额度保留，不是吞吐保证

`concurrency_request` SHALL 只保证该 key 的额度不被他人申请占用（`Σrequest ≤ 池容量`），
系统 MUST NOT 承诺「池饱和时保底内的请求仍被放行」——池是硬顶，饱和即拒。
若调用方需要吞吐保证，SHALL 通过设置 `limit = request` 获得严格独占。

#### Scenario: 保底不等于可通行
- **WHEN** 池容量 5，key A request=3 limit=0，其他 key 正占满池
- **THEN** A 的请求会被拒（池满），其 request 不构成通行权
- **THEN** 该行为 MUST 在错误体与文档中如实说明，MUST NOT 宣称「保底可用」

#### Scenario: limit=request 得到严格独占
- **WHEN** 池容量 5，key A request=3 limit=3，其他 key 占满其余 2
- **THEN** A 始终能用到 3（别人的占用无法挤占），且不超过 3

### Requirement: key 级准入与超限语义

系统 SHALL 在 `apiKeyAuth` 之后、分组白名单门之前，按 key 的 `concurrency_limit` 与
`rpm_limit` 做准入。超限 MUST 返回与既有 429 同形的错误体，并 MUST 带 `retry_after`、`limit_kind`
（`concurrency` | `rpm`）与 `limit_scope`（`key` | `user`），使调用方能区分
"我超了这把 key" / "我超了账号总额度" / "上游满了"。

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
