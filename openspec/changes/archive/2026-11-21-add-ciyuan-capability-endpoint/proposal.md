## Why

词元的 `/v1/models` 只回答「这把 key **被允许** 看哪些模型」（分组 allowlist ∩ 平台默认），不回答
「此刻**能不能服务**」。两者的差集正是生产上最容易撞的一堵墙：id 在 allowlist 里、可调度账号
却没有它的映射（或账号全处冷却），调用方直到真正发请求才拿到 404/503。

后果已经量化在消费侧——壹座为此自建了一套探针式 sync（`paas/openspec/changes/archive/2026-09-30-add-llm-model-discovery`）：
拉全量 id → 逐个打 1-token 补全 → 分 `serving`/`unauthorized`/`upstream_down`/`rate_limited`/`not_chat`/`error`。
`paas/dsh-profile.js:42-60` 记了代价：「ID 形状双向漂移——信探测，别信历史」，deepseek 在带前缀与裸名
之间一天翻两次。**每个消费方都要自己付这笔钱**，而词元在鉴权那一刻其实已经算出来了。

同时暴露的三件事各线都读不到：三车道（rerank/audio/websearch）此刻有没有可用上游账号、
图像/视频在该分组是否开放、这把 key 的额度与速率窗口还剩多少。结果是各线靠硬编码与试错。

本 change 只做一件事：**把词元已经算出来的能力事实，开一个 key-scoped 只读出口**。
不改调度、不改计费、不改 `/v1/models` 的 OpenAI 兼容语义。

## What Changes

- 新增 `GET /v1/ciyuan/capabilities`（apiKeyAuth 链上、组门之前，与 `/v1/sub2api/billing` 同位），
  用调用者自己的 key 鉴权，返回这把 key 当前的能力快照。
- 快照给出两个**分离且都带判据来源**的集合，而不是一个含糊的模型列表：
  - `serving`：可调度账号的 `model_mapping` 并集（= `GetAvailableModels` 的既有口径，**非**实时探测）
  - `permitted_only`：通过分组 allowlist 但**没有任何可调度账号**能服务（今天静默失败的那一撮）
- 同时给出：三车道可用性（候选账号数）、图像/视频分组开关（未知用 `null`，不用 `false` 冒充）、
  额度与速率窗口的**配置值**（用量字段留 `null`——沿用 `KeyBillingInfo` 的既有边界）。
- 明示口径：`basis: "schedulable_account_config"` + `probed: false` + `generated_at`，
  调用方自己决定多信这份快照（探测仍可作为校验）。

**不做**：不新增迁移、不改认证快照结构（因此**不需要**动 `apiKeyAuthSnapshotVersion`）、
不改 `/v1/models`、不引入新平台、不做实时探测、不给单价数字。

## Capabilities

### New Capabilities
- `ciyuan-capability-endpoint`：词元对调用者暴露的 key-scoped 能力快照（可用模型两谓词、车道可用性、
  媒体开关、额度与窗口配置），及其诚实性口径（不冒充实时、未知不用假值）。

### Modified Capabilities
（无——`ciyuan-gateway-lanes` 的车道协议、`ciyuan-gateway-identity` 的登录闭环、
`/v1/models` 的 OpenAI 兼容语义均不变；本 change 只增一个只读端点）

## Impact

- **代码**：新增 `backend/internal/lanes/capabilities.go` + 测试；`lanes/router.go` 加一行路由；
  `lanes/app.go` 的 `Lanes` 结构多存一个 `*service.OpenAIGatewayService`（构造处已在手）。
  **不触碰任何上游文件**——`RegisterGatewayLanes` 已挂在 `routes/gateway.go:195`，属已登记缝点。
- **数据库**：零迁移（号段冻结于 249，本 change 不占用）。
- **认证快照**：零字段变更 → 零版本号风险。
- **计费**：零变更；端点只读，不计费、不扣窗口。
- **对外契约**：新增一条只读端点；`/v1/models`、三车道端点、admin 面均不变。
- **消费方**：后续由 `paas` 侧 change 接入（壹座 sync 降级为校验、各线读推荐默认模型），不在本 change。
