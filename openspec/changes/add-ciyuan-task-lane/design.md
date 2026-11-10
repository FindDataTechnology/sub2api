# Design: add-ciyuan-task-lane

## Context

### 今天的形状（实证，2026-11-11）

```
调用方 --POST /v1/chat/completions--> apiKeyAuth --> 分组/白名单门
                                        |
                                        v
                            SelectAccount（按 priority，**不看价格**）
                                        |
                                        v
                         tryAcquireAccountSlot(account.ID, account.Concurrency)
                              |                               |
                          拿到槽位                        队满 → ErrQueueFull
                              |                               |
                              v                               v
                        转发上游 → RecordUsage          429 + Retry-After 给调用方
                                              （PricingAt 在请求开始捕获，峰谷按它算）
```

三个已核实的事实：

1. **价格是完整且正确的**：`ChannelModelPricing.TimePricing{Timezone, WeekdaysOnly, Periods[]}`
   + DeepSeek 官方峰谷硬编码（低谷 $0.15/$0.60/$0.003 per MTok，高峰 2×，时段
   01:00–04:00 与 06:00–10:00 UTC 工作日，北京周末全低谷）；结算用**请求级 `PricingAt`**
   （`billing_service.go:1631` "优先请求级 PricingAt，零值回退当前时刻"），
   `account_stats_pricing.go:21` 注释："pricingAt 与本次客户计费使用同一时刻，避免跨峰谷请求的
   成本与售价错位"。**所以不存在"用完成时刻计价"的缺陷，不要改它。**
2. **容量闸是每账号的、且是快速失败的**：bound = `account.Concurrency`（缺省 env
   `UPSTREAM_CONCURRENCY_MAX=8`），队列深 = bound×4，满即 `ErrQueueFull` →
   handler 映射 429 + Retry-After（终止性 failover，不推压力给下一个账号）。
3. **调度不读价格**：`PeakMultiplierAt` 的全部调用点都在计费/展示
   （`gateway_key_billing.go:86`、`gateway_profit_control.go:44`、`openai_profit_control.go:260`），
   账号选择路径零引用。

### 由此产生的真实浪费（不是推测）

- 可延后的工作（索引、批处理、评测、回填）在高峰提交 → 按 **2×** 付费，且无任何机制提示它"等 4 小时能省一半"。
- 容量吃紧时，调用方收到 429 只能盲目重试——重试放大（thundering herd）反而更容易队满。
- 调用方无法从网关得知"现在便宜吗 / 还有余量吗"，只能自己猜。

## Goals / Non-Goals

**Goals**

- G1 让"有截止时间的任务"能被**跑完**，而不是把 429 摔回调用方。
- G2 让"可以等的任务"**自动等到低谷**，且节省可量化（估价 vs 实扣）。
- G3 复用既有价格与容量机器，**不新建第二套计价或第二套账**。
- G4 旧调用方零改动：不声明 `mode=defer` 的请求行为与今天逐字一致。

**Non-Goals**

- 不修上游模型不稳定（用户明确排除）。
- 不改 `/v1/chat/completions` 语义、不改 `PricingAt` 取时刻规则、不新建价格表。
- 不做跨用户抢占/优先级剥夺（谁能插队是产品问题，本 change 不碰）。
- 不做前端控制台（先给 API）。

## Decisions

**D1 新开车道，不改 chat。**
`POST /v1/ciyuan/tasks` + `GET /v1/ciyuan/tasks/{id}`，与仓内 `batch_image_jobs` 的
submit/poll 同形。备选「在 chat 上加重试/排队参数」被否：chat 是 OpenAI 兼容面，
加非标语义会污染兼容性，且同步请求本来就不该等 4 小时。

**D2 容量不足时"等等"而不是"429"，但只对显式声明 defer 的任务。**
`mode: "now"`（默认）= 今天的行为（满即 429，快速失败）；`mode: "defer"` + `deadline`
= 入队等待，直到拿到槽位或到截止时间。备选「全局改成排队」被否：交互式请求等 30 秒
比立刻 429 更糟，而且会改掉既有 429 契约。

**D3 价格决策只用既有倍率，且只影响"何时发起"，不影响"怎么计价"。**
读 `ChannelTimePricing.MultiplierAt` / `deepseekPeakMultiplierAt` / `PeakMultiplierAt`；
实扣仍在请求发起后由既有 `PricingAt` 决定。这样即使价格配置后来改了，也不会出现
"估价口径"与"账单口径"两套账——账单永远是账单。

**D4 估价必须自证是估价。**
提交时只知 prompt 长度、不知输出长度 → 用调用方声明的 `max_tokens` 作为**上界**估价，
字段名带 `_estimate`，并同时给 `price_window`（当前峰谷）。任务完成后写
`actual_cost`（来自 RecordUsage）。**禁止**把估价当账单展示（沿用「估算不冒充账单」纪律）。

**D5 任务车道不绕过容量闸。**
发起时仍走 `tryAcquireAccountSlot` 与既有转发路径；任务车道只决定"何时试"，不自己
探测上游、不自建并发计数。备选「任务车道自带并发控制」被否：会与 AIMD 打架，出现两套限流。

**D6 存储沿用既有异步范式，幂等键沿用既有纪律。**
新表 `task_jobs`（任务）与 `task_attempts`（每次尝试），`external_id` 唯一做幂等
（与边界结算、usage 镜像同一纪律）。迁移从 **250** 起（号段冻结于 249）。

## 排程策略（核心，写清判据而不是"智能"）

任务进入 `pending` 后，调度器每次 tick 做三件事，顺序固定：

1. **可行性**：现在有可用账号吗（`ListSchedulableByPlatform` + 模型映射 + 分组权利）？
   没有 → 若 `now()` + 退避 < `deadline`，保持 `pending`；否则 `failed(no_capacity_by_deadline)`。
2. **价格决策**（仅 `mode=defer` 且任务有 `price_policy`）：
   - 估价 `cost_now = Σ(price × est_tokens × multiplier(now))`
   - 估价 `cost_offpeak = Σ(price × est_tokens × 1.0)`（低谷因子取配置的基准值）
   - 若 `saving = cost_now - cost_offpeak >= min_saving`（默认 5%）**且**
     `next_offpeak_start + est_duration <= deadline` → 置 `deferred_until = next_offpeak_start`；
     否则立即执行。
3. **发起**：到期且拿到槽位 → `running`；上游失败按既有类型化原因分流
   （`ErrQueueFull` 视为可重排；5xx/超时按退避重试；4xx 除 429 外直接 `failed`，不重试）。

**可观测**：每次 tick 写 `task_attempts`（`reason`、`channel/account`、`started_at`、
`outcome`、`estimated_cost`、`actual_cost`）。不允许"任务卡住但无记录"——
`pending` 超过 `deadline` 必须落 `failed(reason)`，不允许静默滞留（本仓已多次栽在
"任务挂着但不在链上"）。

## Risks / Trade-offs

- **[第二写账入口]** 若任务车道自己写用量，会出现两套账。缓解：D5 强制复用 `RecordUsage`；
  判据=任务产生的用量行与同步请求同表同字段，且 `request_id` 可回溯到 `task_jobs.id`。
- **[推迟导致错过 SLO]** 价格决策必须被 `deadline` 硬约束包裹；`deferred_until + est_duration > deadline`
  时**禁止**推迟。判据=任何 `deferred_until` 都由 deadline 反推得出，不接受"看起来便宜就等"。
- **[估价失真]** 输出长度不可预测。缓解：`max_tokens` 上界 + 字段名带 `_estimate` +
  完成后写实扣；估价只用于"值不值得等"的决策，不用于对账。
- **[与 AIMD 打架]** 若任务车道自建并发计数，AIMD 会失效。缓解：D5。
- **[队满风暴转移]** 排队后压力从"429 给调用方"变成"任务堆在库里"。缓解：任务数上限 +
  `deadline` 淘汰 + 观测；不允许无界排队。
- **[迁移编号]** 从 250 起；须先确认 `harden-sub2api-credential-storage` 未再推进编号。

## S0 实测结果（2026-11-11，定量依据；不是估算）

### 峰谷确实是 2×，且实测吻合

按 UTC 小时统计 `deepseek-v4.1-flash` 近 7 天的单位成本（USD/MTok）：

| UTC 时段 | 单位成本 | 判定 |
|---|---|---|
| 01–03、06–09 | **0.49 – 0.58** | 高峰 |
| 00、04–05、10–23 | **0.29 – 0.38** | 低谷 |

实测比 ≈ **1.9×**，与文档写的 2× 吻合（差异来自缓存命中占比与 token 结构）。线路窗口
**01–03 与 06–09 UTC** 与 `deepseekPeakMultiplierAt` 的实现完全一致——没有配置漂移。

### 高峰的"钱占比"远高于"请求占比"——这就是本 change 的收益来源

近 7 天 `deepseek-v4.1-flash`（`actual_cost>0`）：

| | 金额 | 请求数 |
|---|---|---|
| 高峰（01–03 / 06–09 UTC） | **$117.62** | 19,010 |
| 低谷 | $119.14 | 38,961 |
| 合计 | $236.76 | 57,971 |
| **高峰占花费** | **49.7%** | 占请求仅 32.8% |

**高峰时段用三分之一的请求吃掉一半的钱。** 把其中"可延后"的部分推到低谷，单次成本降
≈47%（0.55 → 0.29）。即使只有 1/3 的峰时任务可延后，7 天也能省 ≈ $19（年化 ≈ $1,000）；
可延后比例越高，线性放大。这是本 change 唯一需要证明的收益，判据在 S5.3。

### 迁移编号：main 上是 248，249 已被在途分支占用

- `feat/ciyuan-capabilities`（基于 main）里 `migrations/` 最大号 = **248**。
- `249_credential_at_rest_columns.sql` 在 **`harden/credential-at-rest` 分支**上（尚未合并）。
- 故本 change 的迁移用 **250** 起：既避开在途分支，也留出 249 给先合并的一方。
  **合并前必须重核一次**（谁先落地谁占 249）。

## Migration Plan

分四步，每步可独立上线与回滚：

1. **S1 可见性**（零 schema、零写路径）：能力快照加 `pricing`（当前峰谷因子、低谷窗口起止）
   与 `headroom`（各渠道可用槽位估算）；`_meta` 标注 advisory。回滚=删字段。
2. **S2 任务车道骨架**（加两张表 + 提交/查询 + `mode=now` 直通）：`mode=now` 行为等价于
   一次同步调用，用来验证写账与幂等。回滚=停用端点（表可留）。
3. **S3 入队与重试**（`mode=defer` + 截止时间 + 退避 + `failed(reason)`）。
4. **S4 价格感知推迟**（`deferred_until` + 节省阈值）。

**回滚**：端点可关闭（feature flag `CIYUAN_TASK_LANE=off`）；表与用量行保留即可，
不影响同步路径。

## Open Questions

- **Q-a** 任务的最大排队时长与最大排队数？需实测单账号吞吐后再定（不拍脑袋）。
- **Q-b** `min_saving` 默认 5% 是否合适？取决于调用方对延迟的容忍度。
- **Q-c** 任务是否需要"取消"？v1 建议只做 `deadline` 淘汰，不做显式取消（少一条状态机）。
