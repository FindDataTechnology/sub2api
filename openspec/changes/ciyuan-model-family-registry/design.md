## Context

见 proposal.md — Why。设计所需现状（2026-11-18 实证）：

| 事实 | 证据 |
|---|---|
| 图片准入谓词 | `service/openai_images.go:483` `isOpenAIImageGenerationModel` = `IsGPTImageGenerationModel(model) \|\| isGrokImageGenerationModel(model)`；`gpt-image-*` 前缀 / `grok-imagine`、`grok-imagine-edit`、`grok-imagine-image*` |
| gemini 图片例外 | 同文件 `:513` `isGeminiCompatibleImageModel`：`gemini-` 前缀且含 `-image`。由 `validateCompatibleImagesModel`（:519）在 `validateOpenAIImagesModel` 之前放行 |
| 判定点一（解析） | `parseOpenAIImagesJSONRequest` → `:239 validateCompatibleImagesModel(req.Model)` |
| 判定点二（转发复核） | `:638` 与 `:642`：渠道映射后的 `requestModel` 与账号 `GetMappedModel` 后的 `upstreamModel` 各判一次 |
| 判定点三（意图识别） | `service/image_generation_intent.go:40 IsImageGenerationIntent` → `:44 isOpenAIImageGenerationModel`（另见 `openai_images_responses.go:1799/1803` 用 `validateOpenAIImagesModel`） |
| Responses 工具侧 | `isOpenAIImageGenerationModel` 同时驱动 Responses 工具转换、定价与限流策略（源码注释明写：与 `isGeminiCompatibleImageModel` 刻意分开正是为此） |
| embedding 目录 | `service/embedding_model_catalog.go:36` `embeddingModelIDs` 四条静态；消费点 `IsEmbeddingModelID`（:41）、`admin_group.go` 的 `defaultModelsListCandidateIDs` OpenAI 分支、`admin/account_handler.go` 的 `GetAvailableModels` 默认分支、`account_test_service.go` 的 `FetchOpenAIAccountModels` 出口 |
| 系统设置机制已存在 | `SettingService`（`setting_service.go`）读写 `system_settings` 键值；`SystemSettings` 结构 + `parseSettings`/`buildSystemSettingsUpdates`；`OpenAIGatewayService` 已持有 `settingService`（`openai_gateway_service.go:489`） |
| 热路径顾虑 | 图片解析/意图识别是每请求路径；`GetAllSettings` 是 DB 读 → 必须走缓存，不能每请求查库 |
| 视频平台分支 | `handler/grok_media.go:56-60`：`endpoint.IsSeedance()` → `platform=openai`，否则 `platform=grok`；`service/seedance.go:79` 要求 `base_url` + `openai_capabilities` 含 seedance |

**约束**：ADR-0024 只增不改。`openai_images.go` / `image_generation_intent.go` 目前**不在**缝点登记表 §二
（是上游文件），本 change 会触碰它们 → 必须同时补登记（这是 `ciyuan-seam-check` 的前置素材）。

## Goals / Non-Goals

**Goals**

- 图片与 embedding 的准入族表可配置，默认零值等价，不出片即可加一家渠道的模型名。
- 三个图片判定点同源（同一份配置、同一解析函数）。
- 错误归属可辨识：准入拒绝 vs 上游失败。

**Non-Goals**

- 不做渠道级价目；不做非 OpenAI 形状的原生视频协议；不改图片/embedding 端点形状与计量。
- 不把族表做成按账号/按分组的细粒度策略（YAGNI：先全局一份，真需要再分层）。

## Decisions

### D1 族表落在系统设置（JSON），不落在 groups 列

用 `system_settings` 的一个 JSON 键（形如 `openai_model_families`），复用既有 `SettingService`
的读写与 admin 面，零迁移。

**理由**：族表是**部署级能力面**，不是租户/分组级策略——「本部署承认哪些图片模型」对所有分组都成立。
放 groups 列会逼着每个分组各配一份，且要迁移 10+ 组存量行。

**备选**：`groups` 加 JSON 列 → 否决（语义错位 + 迁移成本）；代码常量改为环境变量 → 否决
（运营要后台可改，env 要重启且不可审计）。

### D2 族表形状：`{mode: [{prefix|exact, match, note}]}`

```json
{
  "image": [
    {"prefix": "gpt-image-", "note": "OpenAI native"},
    {"prefix": "grok-imagine", "note": "xAI"},
    {"prefix": "gemini-", "contains": "-image", "note": "Gemini compatible"}
  ],
  "embedding": [
    {"exact": "Qwen/Qwen3-Embedding-8B"},
    {"exact": "text-embedding-3-large"},
    ...
  ]
}
```

匹配规则刻意最小：`prefix` / `exact` / `contains` 三种，大小写不敏感。不做正则——正则是给自己挖
ReDoS 与不可读配置的坑。

**理由**：今天三族的形状恰好能被 `prefix` 与 `prefix+contains` 表达（gemini 那条），所以默认配置
能逐字复现今天的谓词，等价性可断言。

### D3 三个判定点收敛到一个解析函数，配置快照缓存

新增一个 `ModelFamilyRegistry`（`service/` 内，新增文件），提供：
`IsImageModel(name) / IsEmbeddingModelID(name) / Snapshot() `。内部持有原子快照，
`SettingService` 更新时失效重载（沿用既有 setting 缓存失效模式）。

三个判定点全部改调它：`parseOpenAIImagesJSONRequest` 的 `validateCompatibleImagesModel`、
转发复核处、`IsImageGenerationIntent`、`Responses` 侧、`IsEmbeddingModelID`。

**关键**：`isGeminiCompatibleImageModel` 的语义（走 APIKey passthrough 能力）必须保留——
族表只决定「是不是图片模型」，**能力归类**仍由 `RequiredCapabilityForModel` 承担。两者不能合并。

**理由**：今天「是图片模型」与「需要哪种能力」是两个问题，前者可配置、后者是路由语义。
混在一起会让放宽准入顺带改写路由，风险大得多。

### D4 错误归属：准入拒绝带专用错误码，上游失败原样透出

- 准入拒绝：保持今天的 400 + 文案（等价性要求）。
- 新族放行后上游失败：沿用既有 failover 与错误透传路径，**不新增强制转换**。
- 运维观测：在新族模型的失败上打一个可辨识的 tag（如 `model_family_source=config`），
  使「错配族表」可从观测面区分。

**理由**：放宽准入最坏的形态是「本该 400 的变成 502」，用户与运维都难判。tag 让它可归因。

### D5 视频只走「登记 OpenAI 兼容视频端点」

`handleGrokMedia` 的平台判定改为读平台注册表（与 `ciyuan-lane-upstream-registry` 同源思路），
而不是 `if IsSeedance() { openai } else { grok }` 的二选一。登记项 = 「该平台有 OpenAI 兼容视频端点」。

本 change 只做「让登记过的新平台可承接」，**不登记任何新平台**——真实登记随具体渠道接入时做。

**理由**：把二选一改成查表，是为后续接入留缝；本 change 不替未出现的渠道预建适配器。

## Risks / Trade-offs

- [热路径每请求查配置] 族表判定在图片解析/意图识别上 → D3 原子快照缓存；配置更新时失效重载；增加一条「更新后立即生效」的测试
- [放宽后错误归属模糊] 见 D4；观测 tag 兜底
- [等价性被破坏] 默认配置必须逐字复现今天三个谓词 → 任务里列等价性断言（族集合、拒绝文案、gemini 例外）
- [意图识别被放宽导致 Responses 工具误判] `isOpenAIImageGenerationModel` 同时驱动 Responses 工具转换/定价/限流 → 只在**图片族表**内放宽，且用「新族 + Responses 请求」的测试确认不误转
- [配置写错导致流量打到不存在的模型] D4 观测 tag + 后台配置校验（族至少 1 条、prefix 非空）
- [触碰上游文件扩大缝点面] 必须同批补登记 §二（与 `ciyuan-seam-check` 联动）

## Migration Plan

1. 新增 `ModelFamilyRegistry` + 默认族（复现今天行为），先只接线一个判定点并有等价测试
2. 逐个接线其余判定点，每接一处跑一次等价测试
3. admin 配置面（系统设置里可读写该 JSON 键）
4. 视频平台判定改查表（默认登记仍只有 seedance/grok）
5. 出片滚产；验收：后台登记一个新图片族 → 真实请求穿过准入

**回滚**：退镜像 tag。配置键留在库里无害（旧版不读它）。

## Open Questions

无。
