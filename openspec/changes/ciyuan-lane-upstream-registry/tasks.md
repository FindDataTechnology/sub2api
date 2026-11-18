# Tasks: ciyuan-lane-upstream-registry

## 0. 前置

- [ ] 0.1 确认 `merge-upstream-v0-2-21` 已完成并滚产（本 change 的基线必须是合并后的 main）。验证：`git merge-base --is-ancestor <合流提交> HEAD`，且生产镜像 tag 指向该合并提交之后的快照

## 1. 注册表扩展

- [ ] 1.1 `backend/internal/domain/platforms.go` 的 `PlatformSpec` 只增 `Lanes []string` / `Protocol string` / `LaneBaseURLs map[string]string` 三个字段（带 json tag 与注释写明零值语义）。验证：`go build ./...` 通过，既有登记项一字未改仍编译
- [ ] 1.2 为既存五个车道平台逐条填登记：`siliconflow`（rerank）、`serpapi`（websearch）、`openainext`（audio, protocol=openai）、`doubao`（audio, protocol=doubao）、`tencent`（audio, protocol=tencent）。验证：`platformList` 中每行与今日 `LanePlatforms`/`audioProtocolOf` 的实际行为一一对应
- [ ] 1.3 `backend/internal/domain/platforms_test.go` 按上游既定「按位补入」模式同步期望清单

## 2. 派生实现

- [ ] 2.1 `backend/internal/lanes/pool.go`：`LanePlatforms(lane)` 改为从 `domain.Platforms()` 过滤 `Lanes` 含该车道者、按注册表顺序输出；`LanePlatform(lane)` 保留为返回首个平台的兼容 shim。验证：`go build ./...` + 既有车道测试全绿
- [ ] 2.2 `backend/internal/lanes/audio.go`：`audioProtocolOf` 改为「凭据优先 → 注册表 → 回落 openai」。验证：凭据标注 doubao/tencent 的账号仍走原协议；未标注账号仍落 openai
- [ ] 2.3 若 `LaneBaseURLs` 有登记项，接线到对应适配器的默认端点取值（无登记则完全不改端点逻辑）。验证：未登记平台的行为与改动前逐字节等价

## 3. 等价性回归（本 change 的核心判据）

- [ ] 3.1 在 `backend/internal/lanes/pool_test.go` 以测试常量保留旧硬编码列表，逐车道断言 `LanePlatforms` 输出与之**逐项相等（含顺序）**
- [ ] 3.2 断言 `CandidatesForPlatforms` 的跨平台归组结果（平台序 + 同平台内 priority 升序）与改动前一致
- [ ] 3.3 断言 `audioProtocolOf`：未标注 → `openai`；标注 `doubao`/`tencent` → 原值；标注未知值 → `openai`；注册表协议族仅在未标注时生效
- [ ] 3.4 跑 `go test -tags=unit ./internal/lanes/... ./internal/domain/...` 全绿

## 4. 前端清单与建号面

- [ ] 4.1 重生成 `frontend/src/constants/platformCatalog.builtin.json`，跑 `TestFrontendBuiltinPlatformCatalogInSync` 验证一致
- [ ] 4.2 验证管理台车道建号下拉自动包含注册表中所有 `Lanes` 非空的平台（`lanePlatforms` computed 取 `gateway==='lane'`）。验证：前端单测或本地打开页面确认
- [ ] 4.3 frontend `check:i18n` + `vue-tsc` + `vitest` 全绿

## 5. 出片与验收

- [ ] 5.1 bump `ciyuan-tag`、快照仓、GHA、hkccr→relay→ccr、`fd-infra-deploy` tag、ArgoCD 手动 sync。验证：pod Running、健康探针 200
- [ ] 5.2 **零适配器代码演练**：在注册表临时加一行假设平台（`Lanes=["audio"]`、`Protocol="openai"`），重生成前端清单，后台建号，发一次 TTS 请求。验证：请求 200 且用量行入账；`git diff --stat` 中 `backend/internal/lanes/` 为空
- [ ] 5.3 演练后移除假设登记行，重生成清单，重新出片或确认不影响生产（假设平台无账号时车道行为不变）。验证：`LanePlatforms` 输出回到 5.2 之前的状态
- [ ] 5.4 生产三车道真回执：rerank / search / audio（ASR+TTS）各一次成功请求，用量行对账
- [ ] 5.5 台账 `paas/docs/sub2api-fork-patches/README.md` §二 更新「平台清单」缝点行（登记内容扩大：三个新字段 + 消费方）与 §一 发布记录
