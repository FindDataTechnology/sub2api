> **2026-11-20 裁决：放弃。** 理由：能力演进非故障；等需要混布第二家车道供应商再启动，本轮裁决不动。
> 归档目的仅为清出在途清单，内容原样保留于 `changes/archive/`；若将来重启，从归档目录取回即可。

## 状态：**有效，但无需本周启动（2026-11-20 治理复核）**

复核结论（逐条核过代码与生产）：

- **缺口属实**：`lanes/pool.go:49 LanePlatforms` 用 switch 硬编码 audio 三平台；`audio.go:51 audioProtocolOf` 只认豆包/腾讯字符串
- **但没有痛点驱动**：5 个车道平台已经全在 `domain/platformList` 里（fork 已加 `PlatformGatewayLane`），24h 车道流量仅 6 次；现状能跑，只是加新平台要改 Go
- **价值定位**：把车道平台注册到已经存在的 platformList，让新增平台不改代码。前提已备（注册表在，只是车道绕开它）

结论：**保留在途，不归档、不废弃**。它们是能力演进，不是 bug。启动时机由「某车道需要混布第二家供应商，或接新 OpenAI 兼容语音供应商」决定，
不必现在动。工作量为 19 项 / 6 组。

## Why

词元车道（rerank / search / audio）今天要接一家新上游供应商，必须改 Go 代码并出片，因为
「哪个平台能承接哪个车道」被写死在 `backend/internal/lanes/pool.go` 的 `LanePlatforms()`，
而「该平台说哪种上游协议」被写死在 `lanes/audio.go` 的 `audioProtocolOf()`（只认豆包/腾讯两个
字符串，其余一律落回 OpenAI 兼容）。

与此同时，仓库里**已经存在**一张平台注册表：`backend/internal/domain/platforms.go` 的
`platformList` 被注释为「平台的单一数据来源」，前端清单还由后端测试
（`TestFrontendBuiltinPlatformCatalogInSync`）自动生成并校验。车道没接它，才是「加一家渠道
要发版」的根因——注册表本来就在，只是车道绕开了它。

后果具体到运营：同一车道想混布第二家 rerank 供应商、或接一家 OpenAI 兼容形状的新语音供应商，
都得走「改代码 → 单测 → 出片 → 滚产」整链，而这件事本质是配置。

## What Changes

- `PlatformSpec` **只增**三个可选字段：`Lanes []string`（该平台承接哪些车道）、`Protocol string`
  （车道上游协议族）、`LaneBaseURLs map[string]string`（按车道的可选默认端点）。零值语义 =
  今天的行为，老平台登记项一字不改即可继续工作。
- `LanePlatforms(lane)` 改为从 `platformList` 派生（过滤 `Lanes` 含该车道者），`LanePlatform(lane)`
  保留为返回首选平台的兼容 shim。
- `audioProtocolOf(account)` 改为「注册表优先、账号凭据可覆盖」：历史账号（未标注
  `audio_protocol`）行为逐字节等价，新平台登记一条即自动落到正确协议族。
- 前端平台清单（`frontend/src/constants/platformCatalog.builtin.json`）随注册表重生成，
  管理台车道建号下拉自动出现新平台——不改前端代码。
- 一致性与回归门：新增平台后 `LanePlatforms` 的输出必须与「旧硬编码表」在既有平台上逐项相等
  （等价性测试），防止派生逻辑悄悄改变现有候选集与优先序。

## Capabilities

### New Capabilities

- `ciyuan-lane-upstream-registry`: 车道上游注册表的对外可观察契约——「承接哪个车道的平台集合」
  由平台注册表派生而非代码硬编码；新增一个车道上游平台不需要改动任何适配器代码；协议族选择以
  注册表为准、账号级标注可覆盖；既有平台的候选集合与优先序保持不变。

### Modified Capabilities

（无——车道端点形状、计量口径、错误面均由 `paas` 仓的跨仓契约规定，本 change 不改它们的语义）

## Impact

- **代码**：`backend/internal/domain/platforms.go`（+3 字段、+若干登记行）、
  `backend/internal/lanes/pool.go`（派生实现）、`backend/internal/lanes/audio.go`（协议族取值）、
  `backend/internal/domain/platforms_test.go`（期望清单按位补入）、前端清单重生成
- **数据**：零 schema 变更；零迁移
- **运维**：新增车道供应商 = 注册表加一行 + 重生成前端清单 + 后台建号，**不出片**
- **风险**：`LanePlatforms` 派生后候选集顺序若与原表不一致，会改变同优先级下的跨平台轮转次序 →
  等价性测试钉死；`audioProtocolOf` 改动触及历史账号行为 → 「未标注协议 = OpenAI 兼容」必须有
  逐字节等价断言
