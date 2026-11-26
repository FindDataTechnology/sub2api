## MODIFIED Requirements

### Requirement: 额度与窗口只给配置值，用量与价目不给

快照 `limits` 段 SHALL 含 key 额度（`key_quota_usd`、`quota_unlimited`、`key_quota_used_usd`）、
账户余额（`balance_usd`）、三档速率限额（`rate_limit_5h_usd` / `1d` / `7d`）与分组三档限额
（`group_daily_limit_usd` / `weekly` / `monthly`）。

**容量契约（add-key-capacity-contract）追加三组字段**：并发上限
（`concurrency_limit`、`rpm_limit`）、实际生效值（`effective_concurrency_limit`、
`effective_rpm_limit`）与来源（`limit_source`，取值 `key|user|unlimited`）。

`effective_*` MUST 是「key 硬顶与该用户池容量取更严者」的结果，而非 key 自身配置值：
key 未设上限（0）而用户池为 5 时，`effective_concurrency_limit` MUST 为 5 且
`limit_source` MUST 为 `user`。**MUST NOT 在池有上限时把 `effective_*` 报成 0 或省略**——
那会让调用方以为"不限"，与实际行为相反。

`limit_source` MUST 与 `effective_*` 自洽：`unlimited` 仅当两层都未设限。
并发与 RPM 的来源各自独立判定（一个可能来自 key、另一个来自 user）。

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

#### Scenario: key 未设上限但池有上限时报池值

- **WHEN** key 的 `concurrency_limit` 为 0，而其所属用户的 `concurrency` 为 5
- **THEN** `limits.effective_concurrency_limit` SHALL 为 5
- **THEN** `limits.limit_source` SHALL 为 `user`
- **THEN** 消费方 MUST NOT 据此认为该 key 不受限

#### Scenario: 两层都未设限才报不限

- **WHEN** key 的 `concurrency_limit` 为 0 且用户 `concurrency` 也为 0
- **THEN** `limits.effective_concurrency_limit` SHALL 为 0
- **THEN** `limits.limit_source` SHALL 为 `unlimited`

#### Scenario: key 硬顶比池更严时取 key

- **WHEN** key 的 `concurrency_limit` 为 3，用户池为 5
- **THEN** `limits.effective_concurrency_limit` SHALL 为 3
- **THEN** `limits.limit_source` SHALL 为 `key`
