> **2026-11-20 裁决：放弃。** 理由：能力演进非故障；等客户明确要求接新图片/embedding 上游再启动，本轮裁决不动。
> 归档目的仅为清出在途清单，内容原样保留于 `changes/archive/`；若将来重启，从归档目录取回即可。

## 状态：**有效，但无需本周启动（2026-11-20 治理复核）**

复核结论（逐条核过代码与生产）：

- **缺口属实**：`isOpenAIImageGenerationModel` 硬编码 gpt-image/grok-imagine 两族，`isGeminiCompatibleImageModel` 单独硬编码 gemini 族，`embeddingModelIDs` 是四条 Go 静态切片（`service/openai_images.go:556`、`embedding_model_catalog.go:31`）
- **但没有痛点驱动**：近 24 小时生产日志 `requires an image model` 拒绝 **0 次**；embedding 目录四条覆盖了当前全部在用模型
- **价值定位**：让「接一家新图片/embedding 上游」从「改 Go + 出片」变成「后台登记」。是运营效率，不是故障修复

结论：**保留在途，不归档、不废弃**。它们是能力演进，不是 bug。启动时机由「某客户明确要求接入非现有三族的图片模型或新 embedding 供应商」决定，
不必现在动。工作量为 27 项 / 8 组，含 3 个判定点逐个接 + 等价性测试。

## Why

今天要给词元接一家新的图片/embedding 上游，多半会被网关在**触达上游之前**拒掉，原因不是渠道
不可用，而是三张写死在代码里的模型名白名单：

- 图片：`service/openai_images.go` 的图片准入由**两个谓词**把关（2026-11-20 复核订正：
  原文说一个谓词认三族，实际是两层）——`isOpenAIImageGenerationModel` 认
  `gpt-image-*` + `grok-imagine*` 两族；`isGeminiCompatibleImageModel` 另认
  `gemini-*-image*`，两者由 `validateCompatibleImagesModel` 串起来。任何其它 OpenAI 兼容
  渠道的图片模型名
  （flux / seedream / 即梦 / 可灵，或聚合渠道的自有命名）会在解析阶段返回 400
  `images endpoint requires an image model`。
- embedding：`service/embedding_model_catalog.go` 的 `embeddingModelIDs` 是四条 Go 静态切片
  （两个 Qwen3 尺寸 + 三个 OpenAI text-embedding），新聚合渠道的向量模型默认在后台「可用模型」
  与分组白名单里看不见、选不上。
- 视频：除 Ark seedance 与 Grok 媒体外，没有任何其它厂商的路由。

结果：用户说「要能支持除 openainext 之外的其他渠道方资源」，实际卡点是**模型名白名单**，
而它本该是配置。今天要加一个模型族 = 改 Go + 出片。

## What Changes

- **BREAKING（行为面，非接口面）**：图片模型族的判定从「三个硬编码前缀谓词」改为「可配置模型族表」，
  默认值 = 今天这两层谓词认的全部族，未配置时逐字节等价。后台可登记新的模型名前缀族，登记后该族模型能穿过准入
  触达上游（**能否真的出图仍由上游决定**，网关不再替上游做拒绝）。
- embedding 模型目录从 Go 静态切片改为配置项，默认值 = 今天的四条，同样零值等价。
- 图片族的配置项 MUST 同时作用于三处判定（请求解析、渠道映射后的复核、Responses 意图识别），
  不允许出现「端点放行了但意图识别没放行」的半开状态。
- 视频：**只做 OpenAI 兼容形状的扩展**——把 Grok 媒体转发器的平台分支从「grok / 复合组」扩到
  「任何登记了 OpenAI 兼容视频端点的平台」。即梦/可灵/Runway 的原生异步任务协议**不在本 change**。
- 一致性门：新增族名 MUST NOT 让既有三族的行为发生变化（逐字节等价测试），且三个判定点的族集合
  MUST 来自同一份配置（不允许三处各读各的）。

## Capabilities

### New Capabilities

- `ciyuan-model-family-registry`: 模型族配置契约——图片与 embedding 的「哪些模型名被本部署承认」
  由可配置族表决定而非代码常量；默认族与改造前逐字节等价；三个图片判定点取自同一份配置；
  新增族不改变既有族的准入结果。

### Modified Capabilities

（无——图片/视频/embedding 端点形状与计量口径由 `paas` 仓 `ciyuan-media-lane` 等跨仓契约规定，
本 change 不改它们的语义，只改「准入名单从哪来」）

## Impact

- **代码**：`service/openai_images.go`（三个谓词改为查配置）、`service/openai_images_responses.go`
  （意图识别同源）、`service/embedding_model_catalog.go`（切片 → 配置）、`service/image_generation_intent.go`
  （若依赖同一谓词）、admin 配置的读写面（分组/系统设置二者择一）
- **配置面**：新增可配置族表（默认值即今天的三族 + 四条），后台可增删
- **数据**：零 schema 变更（若走系统设置表则零迁移；若走 groups 列则需迁移——design 决定）
- **风险**：把准入放宽后，配置写错会让一个不可用的模型名进入候选，症状是「上游 4xx 透传给终端用户」
  而不是「网关 400」——错误归属改变，需要在错误归因上留痕
