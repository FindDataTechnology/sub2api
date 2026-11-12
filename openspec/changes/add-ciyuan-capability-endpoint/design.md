# Design: add-ciyuan-capability-endpoint

## Context

调用方今天能从词元读到的、与「能力」有关的只有三样，且都不够：

| 现有面 | 给的是 | 缺的是 |
|---|---|---|
| `GET /v1/models`（`gateway_handler.go:1145`） | 分组 allowlist ∩ 平台目录（OpenAI 兼容形状） | 这些 id **此刻**有没有可调度账号能服务 |
| `GET /v1/sub2api/billing`（`gateway_handler.go` `KeyBillingInfo`） | 余额/额度/窗口的配置值 | 与模型、车道无关 |
| `GET /admin/accounts/upstream-queue-metrics`（AIMD 观测） | 上游排队与容量状态 | 需要 admin 身份，调用者拿不到 |

`GatewayService.GetAvailableModels(ctx, groupID, platform)`（`gateway_service.go:1403`）的实现已经命中了
「能服务」的正确口径：`ListSchedulableByGroupID` → 按平台过滤（含 mixed scheduling 豁免）→ 取
`model_mapping` 并集，passthrough 账号跳过 mapping 改由默认集补充。它还有 30s 级短缓存
（`modelsListCache`），所以复用它是**零新增成本**。

`lanes.Lanes` 已经持有 `Pool`（`Candidates` / `CandidatesForPlatforms`，走同一 `accountRepo`），
车道上游账号数可直接数出来。分组侧我要的开关都在认证快照里（`APIKeyAuthGroupSnapshot`：
`AllowImageGeneration` / `AllowBatchImageGeneration` / `VideoModelPrices` / `DailyLimitUSD` /
`WeeklyLimitUSD` / `MonthlyLimitUSD` / `RateMultiplier`）。

**结论：本 change 不需要任何新数据源、新表或新缓存。**

## Goals / Non-Goals

**Goals**

- G1 一把 key 能一次读到「能服务什么 / 只是被允许 / 车道通不通 / 媒体开不开 / 还剩多少」。
- G2 口径诚实：区分「账号配置可用」与「实时探测」，未知一律 `null`，不填假值。
- G3 零缝点越界、零迁移、零快照版本风险——实现全部落在 `internal/lanes/`。

**Non-Goals**

- 不做实时探测（谁来探测、探多少、多久一次都是消费方的策略，不是网关的职责）。
- 不改 `/v1/models` 的 OpenAI 兼容形状（加非标字段会污染兼容面）。
- 不给单价数字（章程 `ciyuan-integration-charter` 决策：不给价目；价目按分组配置属管理端）。
- 不暴露 admin 面（队列指标、账号明细留在 admin）。
- 不做跨消费者接入（壹座 sync 降级、各线读推荐默认模型，属 `paas` 侧后续 change）。

## Decisions

**D1 路径用 `/v1/ciyuan/capabilities`，不是 `/v1/capabilities`。**
与既有 `/v1/sub2api/billing` 同构（`routes/gateway.go:193`），命名空间化避免与上游未来新增的
`/v1/*` 端点撞名；`ciyuan` 是产品名、`sub2api` 是上游名，两者并存不冲突。备选 `/v1/models?serving=1`
被否：把两个语义塞进一个 OpenAI 兼容端点，两个都做不干净。

**D2 路由挂在 `RegisterGatewayLanes` 内部，零上游文件触碰。**
`lanes.RegisterGatewayLanes(gateway, lanesApp)` 已在 `routes/gateway.go:195` 挂到 apiKeyAuth 链上、
组门之前——这是**已登记缝点**。在 `lanes/router.go` 加一行 `group.GET("/ciyuan/capabilities", ...)`
即可，既不用改 `routes/gateway.go`（避开 400 字符源码断言窗口），也不用新增缝点。

**D3 「能服务」用 `GetAvailableModels` 的既有口径，并显式标注它不是探测。**
响应带 `"basis": "schedulable_account_config"` + `"probed": false`。备选「端点内部真探一次」
被否：探测会让一个只读端点变成烧钱端点（每个 id 一次补全），与「前台读面不做事」相互矛盾。

**D4 两个集合分开给，不合并成一个列表。**
- `serving[]`：可调度账号映射并集 → 真正能被路由到的
- `permitted_only[]`：在 `/v1/models` 里看得到、但没有可调度账号能服务 → **今天静默失败的来源**；
  每项带 `basis: "allowlist" | "platform_default"`，说明它是因为分组白名单还是平台默认目录进来的

消费方拿这两个集合就能回答「我该选哪个」而不必自己探。保持 `/v1/models` 原样，两个面互不干扰。

**D5 未知用 `null`，不用 `false` / `[]` 冒充。**
- 车道：`Pool` 未装配（测试骨架）→ `available: null`；装配后 `available = upstream_accounts > 0`
- 视频：分组无视频价目配置 → `models: null`（**不是**「不支持」；也可能是未配置）
- 窗口用量：认证快照只带限额、用量在 Redis 检查时读（`api_key_auth_cache.go` 注释明说）
  → `window_usage: null`

这条直接对应本仓反复栽过的「读不到当成不存在」（同一族缺陷已在 credentials/记忆链上出过事故），
故在契约层面就禁止假值。

**D6 额度语义照抄 `KeyBillingInfo`，不新造。**
`key_quota_usd = 0` 在既有语义里是「不限」，不是「零额度」——响应必须带 `quota_unlimited: true/false`
把这个歧义消掉。`/v1/sub2api/billing` 今天怎么解释，这里就怎么解释，避免同一事实两个口径。

**D7 模型 `kind` 只标能确定的。**
能用既有 `service.IsEmbeddingModelID` 判出的标 `embedding`，其余标 `unknown`。
**不猜** chat / image / video（id 形状不足以判定，猜错比不标更坏）。备选「维护一张 kind 表」被否：
那是消费方已有的 family table（`paas/dsh-profile.js`）该做的事，词元重复维护必然漂移。

## API 形状（v1 契约草案）

```json
{
  "schema": "ciyuan.capabilities/v1",
  "generated_at": "2026-11-10T13:05:00Z",
  "basis": "schedulable_account_config",
  "probed": false,
  "key": {
    "name": "yizuo-ab12cd34-20261110",
    "group_id": 3,
    "group": "core",
    "platform": "openai",
    "status": "active",
    "allowlist_enabled": true
  },
  "models": {
    "serving":         [{"id": "deepseek-v4.1-flash", "kind": "unknown"}],
    "permitted_only":  [{"id": "glm-5.3-flash", "kind": "unknown", "basis": "allowlist"}],
    "counts": {"serving": 1, "permitted_only": 1}
  },
  "lanes": {
    "rerank":    {"available": true,  "upstream_accounts": 2},
    "audio":     {"available": false, "upstream_accounts": 0},
    "websearch": {"available": null,  "upstream_accounts": null}
  },
  "media": {
    "image":       {"allowed": true},
    "batch_image": {"allowed": false},
    "video":       {"models": null}
  },
  "limits": {
    "key_quota_usd": 0,
    "quota_unlimited": true,
    "key_quota_used_usd": 1.23,
    "balance_usd": 42.0,
    "rate_limit_5h_usd": 0,
    "rate_limit_1d_usd": 0,
    "rate_limit_7d_usd": 0,
    "group_daily_limit_usd": null,
    "group_weekly_limit_usd": null,
    "group_monthly_limit_usd": null,
    "window_usage": null
  }
}
```

约定：
- 端点只读、不计费、不扣窗口、不写用量行。
- 401 与 chat 入口对无效 key 的应答同形（复用 `apiKeyAuth` 与既有错误面）。
- 无分组（key 未绑组）时 `models` 用 `ListSchedulable` 全量口径，`allowlist_enabled: false`。
- 网关服务缺失（测试骨架）时返回 `503` + 与车道同形的错误体，不返回半截 JSON。

## Risks / Trade-offs

- **[口径被误读成实时]** 缓解：`probed: false` + `basis` 字段 + 契约文档明写；消费方的探测保留为校验。
- **[`GetAvailableModels` 的 30s 缓存让快照滞后]** 可接受：它正是 `/v1/models` 同一份数据，
  两边若不一致才是 bug。不额外加缓存层（加了就会出现「capabilities 与 models 不同步」的新问题）。
- **[`permitted_only` 可能为空但 `/v1/models` 仍返回模型]** 空集含义 = 「allowlist 里没有孤儿 id」，
  不是「没有模型」；因为默认目录回落的项会带 `basis: "platform_default"` 出现在同一列表里。
- **[车道可用性依赖 `Pool`]** `Pool == nil` 时全部 `null`；这是测试骨架的诚实表达，不是故障。
- **[分组阈值字段是 USD 限额不是剩余量]** `limits` 段只给配置值 + `window_usage: null`，
  消费方不得把它当余额。文档与字段名都带 `limit` 字样，不写 `remaining`。

## Migration Plan

零迁移、零快照字段变更、零计费变更。上线路径照谱系 runbook §6-§7：快照 → 出片 → GitOps → 同步 →
四件套探针。**回滚 = 回退镜像 tag**，无数据面残留（端点不写任何状态）。

## Open Questions

- **车道是否要在能力快照里附带「最近一次成功时间」**（区分「配了账号」与「真的通」）？
  v1 不做（要读 usage 行，会把只读端点变成查询端点）；若 Stage 2 的按线聚合能顺手产出该指标，再补。
- **是否需要 admin 版本的同一端点**（带账号明细）？v1 不做：admin 已有 `upstream-queue-metrics`。
