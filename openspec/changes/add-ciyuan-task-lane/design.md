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

### 一条设计前提：账号是易变的，机制不得建在账号上（2026-11-11 用户指出）

账号会**一直变化**：新增/删除/换号、`Concurrency` 被运维调大调小、某账号冷却或熔开。
所以任何以账号为基准的判据都会随账号漂移而失真——「每账号队列该多深」「单账号吞吐是多少」
「`account.Concurrency` 就是上限」这类设计**不可作为机制的地基**。

本 change 因此把网关（含其 per-account 信号量、AIMD、冷却）视为**不透明执行器**：
任务车道只观测它的**外部可测量**（成败、延迟、单元时间的完成量），并据此闭环调节自己的准入，
**不读、不假设、不复制**账号层的任何配置。判据见「四条不变量」中的 I4。

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

**D5 任务车道建在"机制层"，不建在"账号层"。**
网关（per-account 信号量 / AIMD / 冷却 / 队列）被当作**不透明执行器**：任务车道不读
`account.Concurrency`、不假设账号数量、不按账号记配额，只消费外部可测量——成功/失败/延迟/
单位时间完成量——并据此闭环调节自己的准入速率。

备选「按账号建账、按账号排队」被否：账号是易变的（增删/换号/改并发/冷却），以账号为基准的
判据会随拓扑漂移而失真；且运维每改一次 `Concurrency` 就要重算一次阈值，等于把运维动作
变成调度参数。

备选「任务车道自带并发计数」同样被否：会与既有 AIMD 形成两套限流，且第二套必然与第一套打架。

**D6 存储沿用既有异步范式，幂等键沿用既有纪律。**
新表 `task_jobs`（任务）与 `task_attempts`（每次尝试），`external_id` 唯一做幂等
（与边界结算、usage 镜像同一纪律）。迁移从 **250** 起（号段冻结于 249）。

## 机制设计（账号无关；判据是不变量，不是账号数量）

### 工作类（work class）

调度的最小单位**不是账号，是"类"**：`class = (platform, model, mode)`。
每一类上有三个**观测量**（都不是配置）：

| 量 | 含义 | 怎么来 |
|---|---|---|
| `cap_rps` | 该类近窗可持续完成速率 | EWMA(最近 N 分钟的成功完成数 / 秒) |
| `err_ratio` | 该类近窗失败占比（含 503/429/超时） | 滚动窗口 |
| `p95_latency` | 该类近窗 p95 耗时 | 滚动窗口 |

`cap_rps` **从观测得出，不从 `account.Concurrency` 推导**。账号增删、并发被改、某账号冷却，
都会体现在 `cap_rps` 与 `err_ratio` 的变化里，机制自动跟上，无需改任何配置。

### 闭环准入（class-level，慢环）

```
admit_rps(class) = cap_rps(class) × util_target        # util_target 默认 0.8
每次窗口结算：
  err_ratio > err_high            → admit_rps ×= 0.5   （与既有 AIMD 同向，但不共用状态）
  连续 K 窗 err_ratio < err_low   → admit_rps += step  （加法恢复，避免抖动）
```

这是**第二层环**，与网关内 per-account AIMD 分工：内层管"某账号此刻能不能发"，
外层管"整类整体该发多快"。外层周期更长、只做乘性收缩/加性恢复，不与内层争抢。

### 队列与准入的判据：先判可行性，再决定收不收

排程按 **EDF（最早截止优先）**。提交时做**可行性判定**，不接受"先收下、到点再失败"：

```
feasible  ⟺  queue_wait_estimate + est_duration ≤ deadline
queue_wait_estimate = 队列中排在它前面的工作量 / admit_rps
```

不可行 → 提交即拒（`TASK_INFEASIBLE_BY_DEADLINE`，附 `queue_wait_estimate` 与 `deadline`）。
**这比"收下再报 deadline_exceeded"好**：调用方当场就知道要放松 deadline 或改用 `mode=now`。

### 价格维度（与账号完全正交）

对 `mode=defer` 且带 `price_policy` 的任务：
- `cost_now = est_tokens × price × multiplier(now)`
- `cost_offpeak = est_tokens × price × 1.0`
- 仅当 `saving ≥ min_saving` **且** `next_offpeak_start + est_duration ≤ deadline` 才置
  `deferred_until = next_offpeak_start`。

价格只影响"何时"，不影响"怎么计价"（实扣仍由请求级 `PricingAt` 决定）。

### 四条不变量（这才是本 change 的验收对象）

| # | 不变量 | 怎么验 |
|---|---|---|
| **I1** | 准入速率永不超该类实测可持续容量的 `util_target` 倍——不制造自伤过载 | 压测中 `err_ratio` 不因我方冲量而上升 |
| **I2** | 每个被接受的任务，在接受时刻都存在可行排程 | 抽样核对 `queue_wait_estimate + est_duration ≤ deadline` |
| **I3** | 没有任务会静默越过 deadline（要么成功、要么带 reason 的 failed） | 扫全表：`pending` 且 `deadline < now()` 的行数恒为 0 |
| **I4** | **账号拓扑变化（增/删/改并发/冷却）不需要改任务车道的任何配置** | 人为删一个账号 / 改一个 `Concurrency`，机制在 M 分钟内自愈且无配置变更 |

I4 是本 change 相对"按账号设计"的根本区别，也是最容易被后续维护破坏的一条——
所以它必须进回归用例，而不只是写在文档里。

## Risks / Trade-offs

- **[第二写账入口]** 若任务车道自己写用量，会出现两套账。缓解：D5 强制复用 `RecordUsage`；
  判据=任务产生的用量行与同步请求同表同字段，且 `request_id` 可回溯到 `task_jobs.id`。
- **[推迟导致错过 SLO]** 价格决策必须被 `deadline` 硬约束包裹；`deferred_until + est_duration > deadline`
  时**禁止**推迟。判据=任何 `deferred_until` 都由 deadline 反推得出，不接受"看起来便宜就等"。
- **[估价失真]** 输出长度不可预测。缓解：`max_tokens` 上界 + 字段名带 `_estimate` +
  完成后写实扣；估价只用于"值不值得等"的决策，不用于对账。
- **[两层环互相打架]** 外层 class 级准入与内层 per-account AIMD 若同周期调节会振荡。
  缓解：外层周期显著更长（分钟级 vs 秒级）、只做乘性收缩/加性恢复、不读账号状态；判据=I1。
- **[账号拓扑变化让机制失真]** 若任何判据依赖账号数量或 `Concurrency`，运维动一次账号就失真。
  缓解：D5 + I4（拓扑变化零配置自愈），并把它做成回归用例而非文档承诺。
- **[queue_wait_estimate 失真]** 估算基于 `admit_rps`，负载突变时会偏。缓解：可行性判定
  留安全系数；且**允许调用方拿到拒因后自己抬高 deadline 重提**（幂等键保护，不会重复执行）。
- **[队满风暴转移]** 排队后压力从"429 给调用方"变成"任务堆在库里"。缓解：提交即做可行性
  判定（不可行当场拒）+ 任务数上限 + `deadline` 淘汰；不允许无界排队。
- **[价格窗口配置漂移]** 若价格窗口改了而调度缓存未更新，会出现"按旧窗口推迟"。缓解：
  价格一律在决策时刻现读，不缓存整窗。
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

### 按类容量实测量级（2026-11-11，3 天窗口，分钟级 p95；`(platform, model)` 为类）

| class | 活跃分钟 | p50 rps | **p95 rps** | 单分钟峰值 |
|---|---|---|---|---|
| `openai / deepseek-v4.1-flash` | 3,576 | 0.117 | **0.400** | 71 |
| `openai / cohere/north-mini-code:free` | 151 | 0.033 | 0.400 | 28 |
| `openai / dots-studio/dots-3-note-preview:free` | 513 | 0.050 | 0.200 | 17 |
| `openai / glm-5.3-flash` | 335 | 0.050 | 0.188 | 21 |
| `openai / Qwen/Qwen3-Embedding-8B` | 3,033 | 0.033 | 0.133 | 19 |
| `tencent / audio:tts` | 21 | 0.067 | 0.083 | 6 |

**这个量级必须写进设计，因为它改变了方案的定位**：整类 p95 只有 **0.4 请求/秒**
（≈24 请求/分钟），单分钟峰值 71。也就是说今天的负载远未压满容量——
`ErrQueueFull` 之所以出现，不是因为总量高，而是**瞬时撞在同一个账号上**。

推论：
1. **按类排队就能吃掉绝大部分 429**，不需要加机器：类的 p95 容量还有 10 倍以上余量。
2. **`util_target` 初值不该定高**：以 p95 为基准的 0.8 就意味着 0.32 rps，仍远超实际达到的
   0.117 rps（p50）。故初值按 **p50 起、向 p95 缓慢加性爬**更安全（与 AIMD 同向）。
3. **持久化排队是必须的，但不是因为量大**：是因为任务可能等 4 小时到低谷，进程重启不能丢。

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

- **Q-a** `util_target` / `err_high` / `err_low` / 恢复 step 的初值？需先跑一段观测（按类统计
  完成速率与错误率）再定；**不以账号数量或 `Concurrency` 作为初值来源**。
- **Q-b** `min_saving` 默认按实测改：峰谷实测 1.9×（单次省 ≈47%），5% 明显偏低；
  建议 20% 起，仍需按调用方容忍度定。
- **Q-c** 任务是否需要"取消"？v1 建议只做 `deadline` 淘汰，不做显式取消（少一条状态机）。
