# ciyuan-capability-endpoint Specification

## Purpose
规定词元对调用者暴露的 key-scoped 能力快照：一把调用 key 能一次读到「哪些模型此刻有可调度账号
能服务、哪些只是被允许、三车道通不通、图像/视频是否开放、额度与速率窗口的配置值」，且口径必须
诚实——区分「账号配置可用」与「实时探测」，未知一律 `null` 而非假值。

本能力只增一个只读端点，不改 `/v1/models` 的 OpenAI 兼容语义、不改车道协议、不改计费。

## Requirements

### Requirement: key-scoped 能力快照端点

系统 SHALL 在 apiKeyAuth 链上、分组 allowlist 门之前提供 `GET /v1/ciyuan/capabilities`，
以调用者自己的 API key 鉴权并返回该 key 的能力快照。端点 MUST 只读：MUST NOT 计费、MUST NOT 扣减
额度或速率窗口、MUST NOT 写入任何用量行、MUST NOT 向上游发起模型探测请求。

快照 MUST 含顶层字段 `schema`（值为 `ciyuan.capabilities/v1`）、`generated_at`（RFC3339 时间戳）、
`basis`、`probed`，以及 `key` / `models` / `lanes` / `media` / `limits` 五个段。

#### Scenario: 有效 key 读取快照

- **WHEN** 调用者以有效 key 请求 `GET /v1/ciyuan/capabilities`
- **THEN** 系统 SHALL 返回 200 与上述五段结构
- **THEN** `basis` SHALL 为 `schedulable_account_config` 且 `probed` SHALL 为 `false`
- **THEN** 该请求 MUST NOT 产生用量行、MUST NOT 改变余额

#### Scenario: 无效 key 的拒绝与 chat 入口同形

- **WHEN** 调用者以无效或已吊销 key 请求该端点
- **THEN** 拒绝状态码与错误体形状 MUST 与 chat 入口对同一 key 的应答一致

#### Scenario: 网关服务未装配时不返回半截结构

- **WHEN** 部署或测试骨架未装配网关服务（无法计算模型集合）
- **THEN** 系统 MUST 返回 503 与车道共用的错误体形状
- **THEN** MUST NOT 返回缺少 `models` 段的不完整 JSON

### Requirement: 可用模型分为 serving 与 permitted_only 两个集合

快照 `models` 段 SHALL 给出两个分离的集合，语义 MUST 可区分：

- `serving`：当前分组内**可调度账号**的模型映射并集（与 `GetAvailableModels` 同口径，按平台过滤、
  含 mixed scheduling 豁免、passthrough 账号按默认集补充）
- `permitted_only`：在 `/v1/models` 中可见但**不属于** `serving` 的 id；每项 MUST 带 `basis`，
  取值为 `allowlist`（因分组白名单进入）或 `platform_default`（因平台默认目录回落进入）

每项 MUST 含 `id` 与 `kind`；`kind` 仅在可确定时给出具体值（embedding 用既有判定），
其余 MUST 为 `unknown`，系统 MUST NOT 猜测 chat / image / video。
`models` 段 MUST 含 `counts`（两个集合的元素数）。

#### Scenario: 白名单里的孤儿 id 被单独列出

- **WHEN** 分组 allowlist 含某 id，但分组内没有任何可调度账号的模型映射包含它
- **THEN** 该 id MUST NOT 出现在 `serving` 中
- **THEN** 该 id MUST 出现在 `permitted_only` 中且 `basis` 为 `allowlist`

#### Scenario: 无映射时平台默认目录不冒充 serving

- **WHEN** 分组内所有账号都没有模型映射且平台存在默认目录
- **THEN** 默认目录的 id MUST 出现在 `permitted_only` 中且 `basis` 为 `platform_default`
- **THEN** 它们 MUST NOT 出现在 `serving` 中（无账号依据）

#### Scenario: 分组白名单关闭

- **WHEN** 分组未启用模型白名单
- **THEN** `key.allowlist_enabled` SHALL 为 `false`，`permitted_only` MUST 只含平台默认目录回落的项

#### Scenario: key 未绑定分组

- **WHEN** 调用者的 key 没有分组
- **THEN** 系统 SHALL 以全部可调度账号为口径计算 `serving`，且 `allowlist_enabled` SHALL 为 `false`

### Requirement: 车道与媒体可用性，未知用 null

快照 `lanes` 段 SHALL 含 `rerank` / `audio` / `websearch` 三项，每项含 `available` 与
`upstream_accounts`。`available` SHALL 为布尔值仅当车道上游池已装配且取数成功；池未装配或取数失败时
`available` MUST 为 `null` 且 `upstream_accounts` MUST 为 `null`，系统 MUST NOT 以 `false` 表示未知。

快照 `media` 段 SHALL 含 `image`（`allowed` 布尔）与 `batch_image`（`allowed` 布尔），取自认证分组开关；
`video` MUST 含 `models`，值为分组视频价目配置中的模型 id 列表，分组无该配置时 MUST 为 `null`。

#### Scenario: 车道有可用上游

- **WHEN** 某车道的上游池取到至少一个可调度账号
- **THEN** 该车道 `available` SHALL 为 `true` 且 `upstream_accounts` SHALL 为正整数

#### Scenario: 车道池未装配

- **WHEN** 车道应用未装配（测试骨架）
- **THEN** 全部车道 `available` 与 `upstream_accounts` MUST 为 `null`

### Requirement: 额度与窗口只给配置值，用量与价目不给

快照 `limits` 段 SHALL 含 key 额度（`key_quota_usd`、`quota_unlimited`、`key_quota_used_usd`）、
账户余额（`balance_usd`）、三档速率限额（`rate_limit_5h_usd` / `1d` / `7d`）与分组三档限额
（`group_daily_limit_usd` / `weekly` / `monthly`）。

`key_quota_usd` 为 0 MUST 由 `quota_unlimited: true` 明示为「不限」，MUST NOT 被解释为零额度。
窗口**已用量** MUST 为 `null`（认证快照只带限额，用量在检查时另行读取）。
快照 MUST NOT 含任何模型单价或分组倍率数字。

#### Scenario: 额度不限被显式标注

- **WHEN** 调用者的 key 额度为 0（不限）
- **THEN** `quota_unlimited` SHALL 为 `true`
- **THEN** 消费方 MUST 能据此判定「不限」而非「已用尽」

#### Scenario: 窗口用量不给假值

- **WHEN** 检查 `limits.window_usage`
- **THEN** 其值 MUST 为 `null`
