# Tasks: add-ciyuan-task-lane

> 谱系纪律：实现落 `backend/internal/lanes/`（fork 自有）；上游唯一触碰=已登记的
> 「路由注册」缝点（`RegisterGatewayLanes` 内加路由，不改 `routes/gateway.go`）。
> 迁移从 **250** 起（号段冻结于 249）。零认证快照字段变更。
> 用户边界：**不处理上游模型不稳定**；本 change 只做软件侧调度。

## S0 前置核查（先做，否则后面全白）

- [x] 0.1 确认迁移编号（**2026-11-11 完成**）：`feat/ciyuan-capabilities`（基于 main）里最大号 = **248**；
  `249_credential_at_rest_columns.sql` 属 `harden/credential-at-rest` 分支、尚未合并。
  → 本 change 用 **250** 起，避开在途分支并留 249 给先落地的一方；合并前须重核一次
- [x] 0.2 **按类测量可持续完成速率**（2026-11-11 完成，`(platform, model)` 类，3 天，分钟级 p95）：
  `openai/deepseek-v4.1-flash` p50 **0.117 rps** / p95 **0.400** / 单分钟峰 71；free 档 p95 0.2-0.4；
  embedding p95 0.133；`tencent/audio:tts` p95 0.083。**整类 p95 仅 0.4 rps（≈24 请求/分钟）**——
  负载远未压满容量，`ErrQueueFull` 的成因是**瞬时撞在同一账号**，不是总量高。推论：按类排队即可
  吃掉绝大部分 429；`util_target` 应由 p50 起向 p95 缓慢加性爬，不可直接取 p95×0.8。
  **口径纪律**：MUST NOT 用 `account.Concurrency` 或账号数量推导容量（账号会变）
- [x] 0.2b 校准两层环不打架（2026-11-11：本 change **未**落地 class 级第二层环，见下）：给定内层 AIMD 的既有参数，选定外层周期与收敛系数，使外层
  周期 ≫ 内层周期——验证：压测中两层同时收缩时不出现振幅放大的振荡（同尖峰下 err_ratio 单调收敛）
- [x] 0.3 实测峰谷影响面（**2026-11-11 完成**）：按 UTC 小时统计 `deepseek-v4.1-flash` 近 7 天单位成本——
  高峰（01–03 / 06–09）**0.49–0.58 USD/MTok**，低谷 **0.29–0.38**，实测比 **≈1.9×**（与文档 2× 吻合，
  窗口与 `deepseekPeakMultiplierAt` 实现零漂移）。**收益基数**：高峰 7 天花 **$117.62 / 19,010 请求**，
  低谷 $119.14 / 38,961——**高峰用 32.8% 的请求吃掉 49.7% 的花费**；把可延后部分推到低谷单次省 ≈47%。
  即使只 1/3 峰时任务可延后，7 天省 ≈$19（年化 ≈$1,000）。判据落在 S5.3

## S1 可见性（零 schema、零写路径）

- [x] 1.1 能力快照加 `pricing` 段（`peak` / `multiplier` / `next_offpeak_start` / `next_offpeak_end` / `timezone`），
  数据源只用 `PeakMultiplierAt` 与 `deepseekPeakMultiplierAt`——验证：高峰时段实调返回
  `multiplier=2.0`，低谷返回 `1.0`；不新增任何配置项
- [x] 1.2 能力快照加 `capacity` 段：按 `(platform, model)` 类给 `sustainable_rps`（观测值）与
  `confidence`（样本量档）；不可得时 `null`——**不暴露账号数、槽位数或 `Concurrency`**
  ——验证：单测覆盖「观测不足 → null」与「充足 → 数值」两态；**不得**用 0 冒充；字段名避免
  暗示与账号数量绑定
- [ ] 1.3 契约文档同步（paas 侧 `add-ciyuan-capability-contract` 补一条口径：
  `headroom` 是 advisory，不承诺发起时仍成立）——验证：paas `validate --strict` 通过

## S2 任务车道骨架（`mode=now` 直通）

- [x] 2.1 迁移 `251_lanes_task_jobs.sql` + `252_lanes_task_attempts.sql`（250 已被容量契约占用，顺延；真 PG 跑过两遍幂等）（`external_id` 唯一、
  `status` 枚举、`deadline`、`deferred_until`、`estimated_cost`、`actual_cost`、`reason`）
  ——验证：真 PG scratch 库跑 DDL + 幂等重跑；唯一索引允许多 NULL
- [x] 2.2 `POST /v1/ciyuan/tasks` + `GET /v1/ciyuan/tasks/{id}`（`mode=now` 时直通同步路径）
  ——验证：`mode=now` 成功即返回结果；容量不足返回 429 且**不建任务行**；相同
  `external_id` 二次提交返回 `duplicate:true` 且不新增行
- [x] 2.3 写账复用：任务发起走既有转发 + `RecordUsage`，用量行 `request_id` 可回溯到
  `task_jobs.id`——验证：任务完成后 `usage_logs` 有对应行，字段与同步请求一致；
  **不得**出现任务车道自写的第二套用量

## S3 入队与重试（`mode=defer`）

- [x] 3.1 `defer` 入队与退避：容量不足保持 `pending`，按指数退避重试——验证：单测覆盖
  「持续无容量 → deadline 到 → failed(deadline_exceeded)」与「中途拿到槽位 → 完成」两条路径
- [x] 3.2 终态不可滞留：`pending` 超过 `deadline` 必落 `failed` 且带 `reason`——验证：
  构造一个永远不会拿到槽位的任务，断言它在 deadline 后 T 秒内变 failed；查询端点返回原因
- [x] 3.3 失败分流：`ErrQueueFull` → 可重排；5xx/超时 → 退避重试；4xx（除 429）→ 直接 failed
  ——验证：表测四种失败各自落到预期状态，且 `task_attempts` 每次都有记录
- [x] 3.4 队列上界与可行性拒绝：提交时做 `queue_wait_estimate + est_duration ≤ deadline` 判定，
  不可行当场拒（`TASK_INFEASIBLE_BY_DEADLINE` 并回传估算值）；另有任务总数上限
  ——验证：两条负例（时间不可行 / 上限打满）都在**提交时**被拒且错误可读，不入库
- [ ] 3.5 **I4 回归用例**：人为删除一个账号、并修改另一个账号的 `Concurrency`，断言任务车道
  在 M 分钟内自愈且**零配置变更**——验证：用例在 CI 可跑（用 stub 执行器模拟拓扑变化），
  断言调节后的 `admit_rps` 跟上新的实测容量

## S4 价格感知推迟

- [x] 4.1 估价：用 `max_tokens` 作输出上界，按当前与低谷两个因子各算一次；字段名带
  `_estimate`——验证：单测断言字段名与「估价≠账单」的独立性（可同时存在且允许不等）
- [x] 4.2 推迟判据：`saving >= min_saving` **且** `next_offpeak_start + est_duration <= deadline`
  才推迟——验证：两条负例（节省不够 / 时间不够）都必须**立即执行**，并在响应里留下
  「推迟被否决」的原因（可观测，不静默）
- [x] 4.3 完成后写实扣：任务完成时把 `actual_cost` 从用量行回填——验证：与估价并列可读，
  且不覆盖估价字段

## S5 验收

- [x] 5.1 本地快包：`go build ./...` + `go test ./internal/lanes/ ./internal/server/routes/` + `go vet`
  ——验证：全绿；`gofmt -l` 对新增文件为空
- [ ] 5.2 出片上产（专用线 `law-ai-official/sub2api-images`，`ciyuan-tag` 递增）——验证：
  GHA test+build-push 双绿、digest 一致、rollout 绿
- [ ] 5.3 真机验证：①`mode=now` 与今天等价；②`mode=defer` 在人为制造容量紧张时能等满；
  ③高峰提交一个 `deadline` 充裕的任务，确认它被推迟到低谷且实扣低于高峰估价
  ——验证：三条各留一份 JSON 存档，第③条的 `actual_cost` 显著低于 `estimated_cost`（高峰期估价）
- [x] 5.4 观测落地：`tools/line-metrics.sql` 增一节「任务车道」——验证：能回答
  「多少任务被推迟、省了多少钱、多少任务死在 deadline」

## 实现注记（2026-11-11，实做后补写）

**执行方式改了设计里的一条隐式假设。** 原设计只写「复用 RecordUsage」，
实现时发现最干净的做法是**把请求体重放进同一个 gin 引擎**的 `/v1/chat/completions`：
鉴权、分组门、IP 白名单、账号调度、per-account 有界队列、失败转移、RecordUsage、
峰谷 `PricingAt` 全部走既有那一套，「第二套账」在物理上不存在。
代价是每次尝试多一次进程内 ServeHTTP（相对 LLM 往返可忽略）。

**class 级第二层环（0.2b 提到的）本 change 未落地，是刻意的。**
理由：重放路径下，上游是否吃纳由网关自己回答（429 就是回答），
车道只需退避，不需要再猜一个 `admit_rps`。所以没有 I1 意义上的第二层环，
也就没有「两层环打架」的风险。留下的是**待裁决项**：
若将来要主动压慢某类（而非被动退避），再引入外层环，届时必须重新验 0.2b。

**I4 的保证方式变了：从「自适应收敛」变成「结构上不存在」。**
调度参数里没有任何账号维度（不读账号数、不读 `account.Concurrency`、不读槽位），
容量只用实测或保守常量。故 3.5 的回归用例应当写成**结构性断言**
（调度参数表里不得出现账号维度）+ 一条真机拓扑变更观察，而不是「断言 admit_rps 收敛」。

**两个实现期抓到的坑（都已在代码里钉住）**
1. `usage_logs` 有 `(request_id, api_key_id)` 唯一索引（迁移 027）：
   重试若复用 request_id，第二次的用量行会**静默写不进去** —— 任务成功、账单缺失。
   故 `task-<job>-<attempt>` 按尝试号唯一，并有单测钉住。
2. 容量闸豁免必须做 `path.Clean` 规范化校验：否则
   `/v1/ciyuan/tasks/../chat/completions` 这类穿越形态会拿着任务路径的豁免
   去跑真正的上游调用，等于把席位契约作废。

**迁移编号已按设计文档要求重核**：250 被 `add-key-capacity-contract` 占用（已上产），
本 change 用 251/252。
