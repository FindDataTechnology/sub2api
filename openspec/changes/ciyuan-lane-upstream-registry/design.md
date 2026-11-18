## Context

见 proposal.md — Why。此处只记设计所需现状（2026-11-18 实证）。

| 事实 | 证据 |
|---|---|
| 车道平台映射硬编码 | `backend/internal/lanes/pool.go:41-63`，`LaneAudio` 返回 `[openainext, doubao, tencent]` 三平台字面量；其余车道走 `LanePlatform()` 单平台映射 |
| 车道单平台映射 | `pool.go:33-44`：rerank→siliconflow、websearch→serpapi、audio→openainext（注释自陈「单平台映射已不足以表达候选集合，保留以兼容既有调用」） |
| 协议族判定 | `lanes/audio.go:51-62` `audioProtocolOf`：读 `credentials["audio_protocol"]`，只认 `doubao`/`tencent`，其余（含缺失）一律 `openai` |
| 平台注册表已存在 | `backend/internal/domain/platforms.go` `platformList` 注释明写「平台的单一数据来源」；`PlatformSpec{ID, DisplayName, Gateway, CNProvider, LiteLLMProvider}` |
| 车道平台已在注册表 | 同文件已登记 `siliconflow`/`serpapi`/`openainext`/`doubao`/`tencent`，均为 `Gateway=lane` |
| 前端清单生成链 | `service/platform_catalog.go` `BuildPlatformCatalog()` → 测试 `TestFrontendBuiltinPlatformCatalogInSync` 校验 `frontend/src/constants/platformCatalog.builtin.json`；前端 `platformCatalog.ts` 已含 `'lane'` gateway 枚举与 `isLanePlatform` 谓词 |
| 车道建号 UI 已成 | `CreateAccountModal.vue` 有 `lanePlatforms` computed（过滤 `gateway==='lane'`）与 `selectLanePlatform`，新登记平台**自动出现** |
| 池取数面 | `Pool.Candidates(platform)` / `CandidatesForPlatforms(platforms)`，后者按**入参平台序**稳定归组（`pool.go:130-170`）——优先序完全由 `LanePlatforms` 的输出顺序决定 |
| 计量口径 | `Meter.WriteUsage` → `OpenAIRecordUsageInput{RerankRequests, WebSearchCalls, Audio}`；价目取分组字段（`rerank_price_per_request`、`WebSearchPricePerCall`、音频三价） |

**约束**：ADR-0024 只增不改纪律；`domain/platforms.go` 已是登记缝点（台账 §二「平台清单」），
本次是**在同一缝点上扩大登记内容**，不新增缝点。零 schema、零迁移。

## Goals / Non-Goals

**Goals**

- 加一家车道上游 = 注册表一行 + 前端清单重生成 + 后台建号，不出片。
- 派生结果与旧硬编码表逐项相等（含顺序），由测试钉死。
- 历史账号（未标注协议）行为逐字节等价。

**Non-Goals**

- 不做渠道级/平台级价目（design D4：YAGNI，实例未出现）。
- 不新建插件式渠道 SDK、不抽象出「车道适配器接口注册表」——协议族字符串足够。
- 不改车道端点形状、计量单位、错误面（这些由 `paas` 跨仓契约规定）。
- 不把 lanes 并进 chat 网关族。

## Decisions

### D1 扩展现有平台注册表，而不是新建车道专用注册表

在 `PlatformSpec` 上增字段，而非在 `lanes` 包里建第二张表。

**理由**：仓库已有「平台单一数据源」的明文约定，前端清单、分组平台下拉、用户平台配额、定价匹配
都从它派生。新建第二张表就意味着「加一个渠道要改两处」，把今天的问题换个地方重演。
`Gateway=lane` 这个枚举值已经把这批平台的消费方限定清楚了。

**备选**：`lanes` 包内 `map[string]laneSpec` 注册表 → 否决（前端清单拿不到，建号下拉要另接一条链）。

### D2 字段形状：三个可选字段，零值等于今天

```go
type PlatformSpec struct {
    // ... 既有字段不动
    Lanes        []string          `json:"lanes,omitempty"`
    Protocol     string            `json:"protocol,omitempty"`         // 车道上游协议族
    LaneBaseURLs map[string]string `json:"lane_base_urls,omitempty"`   // 车道 → 默认端点
}
```

- `Lanes` 为空 → 该平台不承接任何车道（既有非车道平台零改动）。
- `Protocol` 为空 → 默认 `openai`（与 `audioProtocolOf` 今天的兜底一致）。
- `LaneBaseURLs` 为空 → 沿用适配器内置默认端点（今天的行为）。

**理由**：三个字段各自独立可选，让「只想让某平台接 audio」与「只想改默认端点」不必互相牵连。
用切片而非位图/枚举，是为了让 JSON 化的前端清单直接可读。

### D3 `LanePlatforms` 从注册表派生，优先序 = 注册表声明顺序

派生规则：过滤 `platformList` 中 `Lanes` 含目标车道的平台，**按 `platformList` 的展示顺序**输出。

关键取舍：今天的 audio 顺序是 `[openainext, doubao, tencent]`，而 `platformList` 里这三个平台的
登记顺序正是 `openainext → doubao → tencent`（`platforms.go` 词元车道段）。**两者天然一致**，
所以派生有解无需调整注册表顺序。等价性测试钉死这一点；若将来有人插入新平台，测试会要求他
显式决定插入位置的影响。

**备选**：在 `Lanes` 里带权重/序号 → 否决（YAGNI；今天没有「同一个车道需要非展示顺序」的实例，
真出现时再加，且那时的字段语义更明确）。

### D4 协议族：注册表优先，凭据覆盖，缺失落 openai

```go
func audioProtocolOf(account *Account) string {
    if v, ok := credentials["audio_protocol"].(string); ok { /* 既有：doubao/tencent 命中 */ }
    if spec, ok := domain.LookupPlatform(account.Platform); ok {
        if p := spec.Protocol; p != "" { return p }
    }
    return audioProtocolOpenAI
}
```

顺序是刻意的：**凭据优先**，保住所有已标注账号的行为（包括未来「同平台内一个账号走另一协议」
的运维手段）；注册表作为未标注时的默认。缺失一律 `openai`，与今天逐字节等价。

**备选**：注册表优先 → 否决。那会把现有已标注 doubao/tencent 的账号行为翻掉（同一平台若注册表
写 openai，账号标注就失效了）。

### D5 等价性用「旧列表常量 + 断言」，不靠人工复核

改造时把旧硬编码列表以测试常量形式保留在 `_test.go` 中，断言 `LanePlatforms(lane)` 与之逐项相等
（含顺序）；`audioProtocolOf` 对「未标注协议账号」的返回值断言为 `openai`。删除旧常量即测试红。

**理由**：这类「派生逻辑看起来一样」的改动最容易在顺序上静默漂移，而顺序直接决定跨平台轮转
次序与故障转移行为。

### D6 新增平台的验收判据是「不写适配器代码」

验收动作：在注册表加一行假设平台（协议族 = 既有的 `openai`），重生成前端清单，后台建号，
发一次车道请求。整个过程中 `git diff` 里 MUST NOT 出现 `backend/internal/lanes/` 下的文件。

**理由**：这是本 change 唯一真正的产出承诺。写成可检验的动作，比写成「注册表化了」有意义。

## Risks / Trade-offs

- [派生顺序与旧表不一致] 轮转次序变化导致故障转移行为改变 → D5 等价性测试（含顺序）
- [凭据覆盖顺序写反] 已标注 doubao/tencent 的账号被翻成 openai，实时语音全挂 → D4 顺序 + 「未标注=openai」与「已标注=凭据」双向断言
- [前端清单未重生成] 新平台在后台建号下拉里看不见 → 既有 `TestFrontendBuiltinPlatformCatalogInSync` 会红；任务里显式列为一步
- [`Gateway=lane` 平台误入 chat 分组] 既有已知可恢复误配（平台清单注释已记），本 change 不扩大该风险面
- [为每个协议族写适配器才是真成本] 注册表只解决「平台 → 协议族」的映射；真要接一个全新原生协议，仍需适配器 → 明确写入 Non-Goals，避免「注册表化 = 免费接一切」的误读

## Migration Plan

1. 注册表加三个字段 + 把五个既存车道平台的 `Lanes`/`Protocol` 按今天行为逐条填好
2. `LanePlatforms` 改派生、`LanePlatform` 保留为 shim
3. `audioProtocolOf` 加注册表兜底（凭据优先）
4. 等价性测试 + 历史账号行为测试
5. 重生成前端清单（`TestFrontendBuiltinPlatformCatalogInSync` 的输出）
6. 出片滚产；验收按 D6 做一次「零适配器代码新增平台」演练后移除假设登记

**回滚**：纯代码改动，无迁移无 schema。退镜像 tag 即回旧版。

## Open Questions

无。
