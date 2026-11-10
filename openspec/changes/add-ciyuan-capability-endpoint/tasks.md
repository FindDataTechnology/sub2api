# Tasks: add-ciyuan-capability-endpoint

> 谱系纪律（ADR-0024 / 台账 §二）：实现主体落在 `backend/internal/lanes/`（fork 自有新增包）。
> 上游触碰 **两处、均不含逻辑**：① 新增文件 `handler/lanes_capability_models.go`；
> ② `routes/gateway.go` 同一行构造调用多传一个方法值。两处已登记进 paas 台账 §二
> 「能力快照模型源（新增）」行。
> 零迁移（号段冻结于 249）、零认证快照字段变更（不动 `apiKeyAuthSnapshotVersion`）、零计费改动。

## 1. 端点骨架

- [x] 1.1 `Lanes` 增持 `models func(ctx, *service.APIKey) ([]string, []string)`（`NewFromOpenAIHandler`
  新增该形参，不加新访问器）——验证：`go build ./...` 通过；`Lanes` 既有字段与对外方法未变
- [x] 1.2 路由挂 `group.GET("/ciyuan/capabilities", ...)` 于 `lanes/router.go` 的 `RegisterGatewayLanes`
  内（apiKeyAuth 链上、组门之前）——验证：`routes/gateway.go` **无新增挂载点**（diff 仅构造参数一行，+1/-1）；
  `internal/server/routes` 12.165s 全绿（含 400 字符断言窗口测试）
- [x] 1.3 网关服务未装配时 503 + 车道共用错误体形状，且不返回半截 JSON——验证：
  `TestServeCapabilities_NotWiredIs503`（断言无 `models` 段、`type=lane_upstream_unavailable`、`lane=capabilities`）

## 2. 模型两谓词

- [x] 2.1 `serving` 来自 `GatewayHandler.LanesCapabilityModels` → `GetAvailableModels`（复用既有 30s 短缓存，
  不自建缓存）——验证：`TestServeCapabilities_ServingVsPermittedOnly`；逻辑在 handler 侧复用而非重写
- [x] 2.2 `permitted_only` = 可见集合 − `serving`，每项带 `basis ∈ {allowlist, platform_default}`——验证：
  三条用例（白名单孤儿 → `allowlist`、无映射回落 → `platform_default`、白名单关闭 → 同 default 口径）
- [x] 2.3 `kind` 只标可确定的（`service.IsEmbeddingModelID` → `embedding`，其余 `unknown`）——验证：
  `TestModelKind`；**不出现**猜出来的 chat/image/video
- [x] 2.4 key 未绑分组时走全量 `ListSchedulable` 口径、`allowlist_enabled=false`——验证：
  `TestLanesCapabilityModels_UnboundKeyFallsBackToAllSchedulable`（断言 `groupID=nil` 时看到全量账号映射而非空集）、
  `TestLanesCapabilityModels_NilServiceDegrades`（网关服务缺失 → 双 nil 不 panic）。
  **这个测试当场逮到真缺口**：既有 `gatewayModelsAccountRepoStub` 只实现了 `ListSchedulableByGroupID`，
  嵌入的 `AccountRepository` 是 nil 接口 → 未绑分组分支一跑就 SIGSEGV。补 `ListSchedulable` 后转绿，
  证明该分支此前**从未被执行过**（不是「测过了」）

## 3. 车道 / 媒体 / 额度

- [x] 3.1 车道取 `Pool.Candidates` / `CandidatesForPlatforms` 的候选数；**空池 → `false`+0，取数失败 → `null`**
  （首版把两者混成 `null`，被 `TestCapabilityLanes_CountsSchedulableAccounts` 当场逮住后修正）——验证：
  `TestCapabilityLanes_PoolNotWiredIsNull`（断言字面量 `null` 且 `NotContains "available":false`）、
  `TestCapabilityLanes_CountsSchedulableAccounts`（停用账号不计）、`TestCapabilityLanes_ListErrorIsNullNotEmpty`
- [x] 3.2 `media.image` / `batch_image` 取分组开关；`video.models` 取 `VideoModelPrices` 键排序，无配置 → `null`
  ——验证：`TestBuildCapabilitiesMedia`（含 `"models":null` 字面量断言）
- [x] 3.3 `limits` 取认证快照字段；`key_quota_usd == 0` → `quota_unlimited: true`；`window_usage` 恒 `null`
  ——验证：`TestBuildCapabilitiesLimits_QuotaUnlimitedIsExplicit`（不限 vs 真上限可区分）
- [x] 3.4 响应体不含单价/倍率——验证：`TestServeCapabilities_NoPriceOrMultiplierKeys`（对序列化结果
  断 `rate_multiplier` / `price` / `image_price` / `usd_per` 四词均不出现）

## 4. 只读性与错误面

- [x] 4.1 端点不写状态、不计费、不探测——验证：`capabilities.go` 对 `RecordUsage` / `Meter` /
  `reserveInflight` / `WriteUsage` **零引用**（grep 实证），对 `http.Get` / `http.Post` 零引用
  （唯一 `probe` 命中是 JSON 字段名 `probed`）
- [x] 4.2 缺 key 时 401 且带 `lane` 可归因（正常路径由 `apiKeyAuth` 先拦）——验证：
  `TestServeCapabilities_RequiresKey`
- [x] 4.3 顶层 `schema` / `generated_at` / `basis` / `probed` 齐备且取值固定——验证：
  `TestServeCapabilities_ServingVsPermittedOnly`（`schema=ciyuan.capabilities/v1`、`basis=schedulable_account_config`、`probed=false`）

## 5. 验收

- [x] 5.1 本地快包：`go build ./...` + `go test ./internal/lanes/ ./internal/server/routes/` + `go vet`——
  验证：2026-11-10 实跑 `lanes` 9.899s / `routes` 12.165s 双 ok、`vet` exit=0；
  `gofmt -l` 对**新增两文件**为空（仓内 `audio_*.go`/`pool.go`/`websearch.go` 基线即不合规，非本次引入，未擅自改）
- [ ] 5.2 快照与出片 → 滚 GitOps → ArgoCD 手动 sync——**未做**：需先定出片线配额（章程 Q1 未裁），
  且本 change 未合并 main
- [ ] 5.3 生产实证（真实 key 请求一次，记录 serving/permitted_only 计数与车道可用性）——**未做**：待 5.2

## 6. 跨仓后续（不在本 change）

- [ ] 6.1 `paas` 仓立契约口径 spec（章程 tasks 3.2）——验证：`paas/openspec/specs/ciyuan-capability-endpoint/`
  存在且 Scenario 与本实现一致
- [ ] 6.2 `paas` 仓接入：壹座 sync 改「读快照为主、探测为校验」；各线 `llm.py` 读推荐默认模型
  （章程 tasks 3.5-3.6）——验证：各仓独立 change 的 tasks 勾选 + 证据路径
