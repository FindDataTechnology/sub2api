# key-self-service Specification

## Purpose
TBD - created by archiving change add-key-capacity-contract. Update Purpose after archive.

## Requirements

### Requirement: 用户可在自己的池内自助分配席位

用户 SHALL 能在自己的池容量内，为不同 key 分配保底与硬顶（如池 5 → A 保底 3 / B 保底 2），
分配 MUST 满足 `capacity-ledger` 的三条不变量。超过剩余可分配额度时 MUST 拒绝，
并回传 `requested` / `remaining` / `capacity`。

#### Scenario: 池内正常分配
- **WHEN** 用户池容量 5，为 A 申请保底 3、为 B 申请保底 2
- **THEN** 两次均成功，剩余可分配额度为 0

#### Scenario: 超额分配被拒
- **WHEN** 在上一状态再为 C 申请保底 1
- **THEN** 拒绝，回传 `remaining: 0`
- **THEN** 错误信息指明"池容量已被占满"，而不是笼统的"参数错误"

### Requirement: 用户可自助创建 key，且只能向下声明容量

系统 SHALL 提供用户面 `POST /api/v1/keys`（与 admin 面 `/api/v1/admin/api-keys` 分离），
让已登录用户在自己的分组与额度范围内创建 key，并可在创建时声明 `concurrency_limit` 与
`rpm_limit`。

当请求声明的上限超过该用户的 user 上限或所属分组的 group 上限时，系统 MUST 拒绝并返回
`LIMIT_EXCEEDS_UPSTREAM`，同时回传被比较的两个具体数值。系统 MUST NOT 静默截断为较小值。

#### Scenario: 声明值超过上位上限被拒
- **WHEN** 用户请求 `concurrency_limit=20`，而其分组上限为 5
- **THEN** 返回 400，错误码 `LIMIT_EXCEEDS_UPSTREAM`，响应含 `requested: 20` 与 `upstream_limit: 5`
- **THEN** 不创建 key

#### Scenario: 不声明即继承
- **WHEN** 用户创建 key 时未提供 `concurrency_limit` / `rpm_limit`
- **THEN** 落库为 0（不限），实际受 user 与 group 上限约束，与今天行为一致

#### Scenario: key 数量上限
- **WHEN** 用户已有 key 数达到每用户上限
- **THEN** 创建被拒并说明上限，不静默失败

### Requirement: 用户只能查看与操作自己的 key

`GET /api/v1/keys` 与 `GET /api/v1/keys/{id}/state` SHALL 只返回当前身份自己的 key。
访问他人 key MUST 返回 404（不泄露存在性），MUST NOT 返回 403 暴露资源存在。

#### Scenario: 越权查询
- **WHEN** 用户 A 查询用户 B 的 key id
- **THEN** 返回 404
- **THEN** 响应中不含任何 B 的 key 名称或状态

#### Scenario: key 明文只出现一次
- **WHEN** 创建成功
- **THEN** 明文 key 仅在创建响应中出现一次
- **THEN** 后续任何查询端点 MUST NOT 返回明文（沿用既有 `hasKey` 式投影）
