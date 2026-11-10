# Tasks: add-ciyuan-task-lane

> 谱系纪律：实现落 `backend/internal/lanes/`（fork 自有）；上游唯一触碰=已登记的
> 「路由注册」缝点（`RegisterGatewayLanes` 内加路由，不改 `routes/gateway.go`）。
> 迁移从 **250** 起（号段冻结于 249）。零认证快照字段变更。
> 用户边界：**不处理上游模型不稳定**；本 change 只做软件侧调度。

## S0 前置核查（先做，否则后面全白）

- [x] 0.1 确认迁移编号（**2026-11-11 完成**）：`feat/ciyuan-capabilities`（基于 main）里最大号 = **248**；
  `249_credential_at_rest_columns.sql` 属 `harden/credential-at-rest` 分支、尚未合并。
  → 本 change 用 **250** 起，避开在途分支并留 249 给先落地的一方；合并前须重核一次
- [ ] 0.2 实测单账号吞吐与队列行为（决定 Q-a 的排队上限）：对某账号以 `Concurrency` 满载
  发起 N 并发，记录 `queue_depth` / `queue_rejected_total` / 端到端耗时——验证：给出
  「单账号每秒可完成多少请求」与「队列多深开始必然超 deadline」两个数
- [x] 0.3 实测峰谷影响面（**2026-11-11 完成**）：按 UTC 小时统计 `deepseek-v4.1-flash` 近 7 天单位成本——
  高峰（01–03 / 06–09）**0.49–0.58 USD/MTok**，低谷 **0.29–0.38**，实测比 **≈1.9×**（与文档 2× 吻合，
  窗口与 `deepseekPeakMultiplierAt` 实现零漂移）。**收益基数**：高峰 7 天花 **$117.62 / 19,010 请求**，
  低谷 $119.14 / 38,961——**高峰用 32.8% 的请求吃掉 49.7% 的花费**；把可延后部分推到低谷单次省 ≈47%。
  即使只 1/3 峰时任务可延后，7 天省 ≈$19（年化 ≈$1,000）。判据落在 S5.3

## S1 可见性（零 schema、零写路径）

- [ ] 1.1 能力快照加 `pricing` 段（`peak` / `multiplier` / `next_offpeak_start` / `next_offpeak_end` / `timezone`），
  数据源只用 `PeakMultiplierAt` 与 `deepseekPeakMultiplierAt`——验证：高峰时段实调返回
  `multiplier=2.0`，低谷返回 `1.0`；不新增任何配置项
- [ ] 1.2 能力快照加 `headroom` 段（按平台 `available_slots` / `in_use_slots`），不可得时 `null`
  ——验证：单测覆盖「统计未装配 → null」与「已装配 → 数字」两态；**不得**用 0 冒充
- [ ] 1.3 契约文档同步（paas 侧 `add-ciyuan-capability-contract` 补一条口径：
  `headroom` 是 advisory，不承诺发起时仍成立）——验证：paas `validate --strict` 通过

## S2 任务车道骨架（`mode=now` 直通）

- [ ] 2.1 迁移 `250_task_jobs.sql` + `251_task_attempts.sql`（`external_id` 唯一、
  `status` 枚举、`deadline`、`deferred_until`、`estimated_cost`、`actual_cost`、`reason`）
  ——验证：真 PG scratch 库跑 DDL + 幂等重跑；唯一索引允许多 NULL
- [ ] 2.2 `POST /v1/ciyuan/tasks` + `GET /v1/ciyuan/tasks/{id}`（`mode=now` 时直通同步路径）
  ——验证：`mode=now` 成功即返回结果；容量不足返回 429 且**不建任务行**；相同
  `external_id` 二次提交返回 `duplicate:true` 且不新增行
- [ ] 2.3 写账复用：任务发起走既有转发 + `RecordUsage`，用量行 `request_id` 可回溯到
  `task_jobs.id`——验证：任务完成后 `usage_logs` 有对应行，字段与同步请求一致；
  **不得**出现任务车道自写的第二套用量

## S3 入队与重试（`mode=defer`）

- [ ] 3.1 `defer` 入队与退避：容量不足保持 `pending`，按指数退避重试——验证：单测覆盖
  「持续无容量 → deadline 到 → failed(deadline_exceeded)」与「中途拿到槽位 → 完成」两条路径
- [ ] 3.2 终态不可滞留：`pending` 超过 `deadline` 必落 `failed` 且带 `reason`——验证：
  构造一个永远不会拿到槽位的任务，断言它在 deadline 后 T 秒内变 failed；查询端点返回原因
- [ ] 3.3 失败分流：`ErrQueueFull` → 可重排；5xx/超时 → 退避重试；4xx（除 429）→ 直接 failed
  ——验证：表测四种失败各自落到预期状态，且 `task_attempts` 每次都有记录
- [ ] 3.4 队列上界：任务总数上限 + 超过即拒绝新提交（返回明确错误，不入库）——验证：
  打满上限后提交被拒且错误可读；上限值来自 0.2 的实测，不拍脑袋

## S4 价格感知推迟

- [ ] 4.1 估价：用 `max_tokens` 作输出上界，按当前与低谷两个因子各算一次；字段名带
  `_estimate`——验证：单测断言字段名与「估价≠账单」的独立性（可同时存在且允许不等）
- [ ] 4.2 推迟判据：`saving >= min_saving` **且** `next_offpeak_start + est_duration <= deadline`
  才推迟——验证：两条负例（节省不够 / 时间不够）都必须**立即执行**，并在响应里留下
  「推迟被否决」的原因（可观测，不静默）
- [ ] 4.3 完成后写实扣：任务完成时把 `actual_cost` 从用量行回填——验证：与估价并列可读，
  且不覆盖估价字段

## S5 验收

- [ ] 5.1 本地快包：`go build ./...` + `go test ./internal/lanes/ ./internal/server/routes/` + `go vet`
  ——验证：全绿；`gofmt -l` 对新增文件为空
- [ ] 5.2 出片上产（专用线 `law-ai-official/sub2api-images`，`ciyuan-tag` 递增）——验证：
  GHA test+build-push 双绿、digest 一致、rollout 绿
- [ ] 5.3 真机验证：①`mode=now` 与今天等价；②`mode=defer` 在人为制造容量紧张时能等满；
  ③高峰提交一个 `deadline` 充裕的任务，确认它被推迟到低谷且实扣低于高峰估价
  ——验证：三条各留一份 JSON 存档，第③条的 `actual_cost` 显著低于 `estimated_cost`（高峰期估价）
- [ ] 5.4 观测落地：`tools/line-metrics.sql` 增一节「任务车道」——验证：能回答
  「多少任务被推迟、省了多少钱、多少任务死在 deadline」
