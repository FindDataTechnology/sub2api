# Tasks: ciyuan-model-family-registry

## 0. 前置

- [ ] 0.1 确认 `merge-upstream-v0-2-21` 已合流滚产（基线必须是合并后的 main）。验证：`git merge-base --is-ancestor <合流提交> HEAD`
- [ ] 0.2 记录改造前的等价基线：把今天三个谓词的判定结果固化成测试表格（`gpt-image-1/2`、`grok-imagine*`×3、`gemini-*-image*`、若干非图片名如 `flux-1`、`text-embedding-3-large`）。验证：该表格在改造前后各跑一次结果一致

## 1. 注册表与默认族

- [ ] 1.1 新增 `backend/internal/service/model_family_registry.go`：族类型（`prefix`/`exact`/`contains` 三种匹配）、`IsImageModel`、`IsEmbeddingModelID`、原子快照与失效重载
- [ ] 1.2 默认族逐字复现今天的谓词：image = `gpt-image-` / `grok-imagine`（含 `grok-imagine-edit`、`grok-imagine-image*` 语义）/ `gemini-`+`-image`；embedding = 今天四条。验证：0.2 的表格全部命中
- [ ] 1.3 配置读写：在系统设置中新增 JSON 键（形如 `openai_model_families`），admin 面可读写；未配置时用默认族。验证：后台保存后可立即读到新值，清空后回落默认
- [ ] 1.4 配置校验：族数 ≥1、`prefix`/`exact` 非空、未知匹配类型拒绝。验证：非法配置返回 400 且不入库

## 2. 判定点接线（逐个接、每接一处跑等价测试）

- [ ] 2.1 `service/openai_images.go` 的 `validateOpenAIImagesModel` / `validateCompatibleImagesModel` 改为查注册表；`isGeminiCompatibleImageModel` 的**能力归类**语义保留（`RequiredCapabilityForModel` 不动）。验证：图片既有用例全绿 + 0.2 表格等价
- [ ] 2.2 `service/image_generation_intent.go` 的 `IsImageGenerationIntent` 改查注册表。验证：意图识别既有用例全绿
- [ ] 2.3 `service/openai_images_responses.go:1799/1803` 两处改查注册表。验证：Responses 侧图片工具用例全绿
- [ ] 2.4 `service/embedding_model_catalog.go` 的 `embeddingModelIDs` 切片替换为注册表查询（保守：目录仍只影响后台可见/可配，不改路由）。验证：`embedding_model_catalog_test.go` 全绿 + 后台可见性用例等价

## 3. 等价性与同源断言

- [ ] 3.1 断言默认族下，三个判定点对同一组模型名给出一致结论（同源测试，不是三处各自测试）
- [ ] 3.2 断言拒绝文案在默认配置下与改造前逐字一致
- [ ] 3.3 断言 `RequirementForModel`/`RequiredCapabilityForModel` 的能力归类在默认族下不变（gemini 仍走 APIKey passthrough、gpt-image 仍走 native、无模型名仍走 basic→native 回退）
- [ ] 3.4 新增族登记后，图片/embedding 各一条真实路径断言：准入放行（图片走 `httptest` 上游桩）

## 4. 错误归属

- [ ] 4.1 准入拒绝路径保持 400 与今天文案（等价性）。验证：3.2 覆盖
- [ ] 4.2 新族放行后的上游失败原样透出（状态码与错误类型不改写）。验证：上游桩返回 4xx/5xx，客户端收到同状态
- [ ] 4.3 失败观测打 `model_family_source=config` 类 tag，使错配族表可归因。验证：日志/观测面能区分「准入拒绝」与「新族上游失败」

## 5. 视频平台判定

- [ ] 5.1 `handler/grok_media.go` 的平台判定从 `IsSeedance() ? openai : grok` 改为查平台注册表「是否登记 OpenAI 兼容视频端点」；默认登记结果与今天完全一致（仅 seedance→openai、grok→grok）
- [ ] 5.2 断言默认登记下视频/图片路由与改造前逐案一致（含复合组走 grok 的分支）

## 6. 缝点登记（与 ciyuan-seam-check 联动）

- [ ] 6.1 把本 change 触碰的上游文件补进台账 `paas/docs/sub2api-fork-patches/README.md` §二：`service/openai_images.go`、`service/openai_images_responses.go`、`service/image_generation_intent.go`、`service/embedding_model_catalog.go`（后者原已在册，更新说明）、`handler/grok_media.go`、`service/domain_constants.go`（新增设置键常量）
- [ ] 6.2 台账 §二 同步记录「新增文件」（`model_family_registry.go`）与「新增设置键」

## 7. 出片与验收

- [ ] 7.1 全量单测绿 + frontend 三件（`check:i18n`/`vue-tsc`/`vitest`）绿
- [ ] 7.2 出片滚产（tag/快照/GHA/ccr/清单/ArgoCD）
- [ ] 7.3 **真机验收**：后台登记一个上游真实支持、但今天会被拒的图片模型族 → 发真实请求 → 穿过准入并触达上游。验证：响应不再是「requires an image model」；成功或上游真实错误均可
- [ ] 7.4 真机验收：登记一个新 embedding 模型，确认后台账号可用模型与分组白名单里可见、可配
- [ ] 7.5 回归：既有三族与四条 embedding 在生产行为不变（各发一次真实请求对账用量行）
- [ ] 7.6 台账 §一 追加发布记录；§二 缝点行与 6.x 一致
