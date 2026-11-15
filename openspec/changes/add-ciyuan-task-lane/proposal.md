## Why

词元今天只有一条**同步请求路径**，价格与容量两个维度都只被"事后处理"：

| 维度 | 现状 | 缺口 |
|---|---|---|
| **价格** | 已完整实现：`ChannelModelPricing.TimePricing`（分时倍率，可带时区/仅工作日/多时段）+ DeepSeek 官方峰谷（高峰 = 低谷 2×，01:00–04:00 与 06:00–10:00 UTC 工作日） | **调度侧一次都没读过价格**（`PeakMultiplierAt` 仅出现在计费与展示面）。选路只看账号 priority，所以一个**可延后**的任务在高峰提交，就必然按 2× 付费 |
| **容量** | 已实现：每账号 `Concurrency` 有界信号量 + AIMD + 队列（`bound = account.Concurrency`，队列深 `bound×4`） | 队列满即 `ErrQueueFull` → **429 + Retry-After 直接打给调用方**。调用方只能盲目重试，没有"等一会儿一定能完成"的语义 |
| **计费时刻** | 已正确：结算用**请求级 `PricingAt`**（请求开始捕获，零值才回退当前时刻），注释明写"避免跨峰谷请求的成本与售价错位" | 无缺陷。**不要**把它当成 bug 改 |

**结论：不是计价错了，也不是容量没闸，而是缺一层"任务调度"。** 今天的每一条请求都是一次
"要么现在成功、要么立刻 429"的赌博；而实际上大量工作（批处理、索引、评测、回填）是有
**截止时间**、可以等 20 分钟、也可以等 4 小时到低谷再跑的。

本 change 增加一条**任务车道**：调用方声明"任务"（含截止时间与紧急度），网关负责在容量与
价格约束下把它跑完——而不是把一个 429 摔回给调用方。

**用户已明确的边界**：只做软件侧。上游模型不稳定（如免费档 503 风暴）不在本 change 处理。

## What Changes

- 新增**任务车道**：`POST /v1/ciyuan/tasks` 提交、`GET /v1/ciyuan/tasks/{id}` 查询，
  形状对齐仓内既有的异步范式（`batch_image_jobs` 的 submit/poll），不改动
  `/v1/chat/completions` 的任何语义。
- 新增**紧急度语义**：`mode=now`（≤今天的行为：容量不足立即 429）与 `mode=defer`
  （带 `deadline`，容量不足则入队等待，不再 429）。默认 `now`，旧调用方零改动。
- 新增**价格感知排程**：对 `mode=defer` 的任务，比较"此刻执行的估价"与"最近低谷窗口的估价"，
  在截止时间允许且节省超过阈值时**推迟到低谷**。价格来源只用既有
  `ChannelTimePricing` / `deepseekPeakMultiplierAt` / `PeakMultiplierAt`，**不新建价格体系**。
- 新增**容量可见性**：能力快照补上"当前价格窗口"与"各渠道余量"，让调用方在提交前就能
  自己决定 `now` 还是 `defer`。
- 新增**任务观测**：每任务的尝试链（哪次、哪个渠道、为什么失败、估价 vs 实扣）。

**明确不做**：不修上游不稳定；不改 chat 语义；不改 `PricingAt` 的取时刻规则；
不新建价格表；不做跨用户抢占。

## Capabilities

### New Capabilities
- `ciyuan-task-lane`：任务提交/查询的对外契约、紧急度与截止时间语义、入队与出队的判据、
  幂等键、以及"估价 ≠ 账单"的诚实口径。
- `ciyuan-capacity-pricing-visibility`：能力快照上的价格窗口与吞吐观测字段（只读、advisory）。

### Modified Capabilities
- `ciyuan-capability-endpoint`：新增 `pricing` 与 `capacity` 两段（**只增不改**既有字段；
  不加认证快照字段，故不触发 `apiKeyAuthSnapshotVersion`）。原提案写的是 `headroom`
  （按平台给可用/占用槽位），实施时按「不暴露槽位与账号数」改判为观测口径 `capacity`。

## Impact

- **代码**：新增 `backend/internal/lanes/task_*.go`（fork 自有）；`lanes/router.go` 加路由；
  调度侧复用 `tryAcquireAccountSlot` / `RecordUsage`，不复制网关热路径。
- **数据库**：新增 `task_jobs`、`task_attempts` 两张表（迁移编号从 **250** 起，遵号段冻结）。
- **计费**：不新增计价逻辑；实扣仍走既有 `PricingAt` + `RecordUsage`。任务车道只负责
  "何时发起"，不接管"怎么计价"。
- **对外契约**：新增两个端点；`/v1/chat/completions`、`/v1/models`、三车道、admin 面零变化。
- **风险面**：任务车道会成为第二个写账入口——必须复用同一 `RecordUsage`，否则出现两套账。
