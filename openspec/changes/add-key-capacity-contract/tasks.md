# Tasks: add-key-capacity-contract

> 用户点名重点：**让用户能申请/创建不同 key 并管理 concurrency 等配置；平台侧能快速说清
> 一个新 key 的基础情况；以及 key 的生效时间与稳定性。**
> 谱系纪律：实现落 `backend/internal/lanes/`（fork 自有）+ 已登记缝点；迁移从 250 起。
> **本 change 必须升 `apiKeyAuthSnapshotVersion`（v26→v27）**——没升就是静默失效。

## M0 前置核查（决定多实例形态，影响限流实现）

- [x] 0.1 确认生产副本数（**2026-11-11 完成**）：**1 replicas / 1 ready** → key 级并发计数
  用**进程内**即精确，不需要 Redis。若将来扩副本必须改共享计数并在 state 端点回报后端类型
  （否则低估 N 倍=限流失效）
- [x] 0.2 认证快照 TTL 与生效时间（**2026-11-11 完成**）：默认 **L1=15s（jitter 10%）/ L2=300s /
  negative=30s**（`config.go:2372-2378`）。但改 key 走**主动失效**：`deleteAuthCache`
  （`api_key_auth_cache_impl.go:258`）同时清 L1 + 负缓存 + Redis L2，调用点覆盖
  update/delete/重置/分组批量改。**生产单副本 → 改配置即时生效，TTL 只是兜底（≤15s）**。
  结论：**不改 TTL 默认值**——单副本下改小只增 DB 压力不改善行为；扩副本时应加失效广播而非降 TTL。
  另测：按 `accounts.platform` 只有 `openai` 有量（46,010，p50 10.1s / p95 86.1s / p99 171.3s），
  故"不同渠道不同"今天无数据支撑，结构上按 `(platform, model)` 支持分化、v1 只对 openai 给基线
- [x] 0.3 存量上限分布（**2026-11-11 完成，改变了本 change 的定位**）：`users.concurrency`
  **5 × 89 人**、0 × 29、30 × 1、1 × 1；`users.rpm_limit` **0 × 118**（几乎全不限）、10 × 2；
  `groups.rpm_limit` 0 × 9 组、600 × 1。→ 并发**已在 user 层限成 5**，RPM 基本虚拟。
  **解释了一个此前没解释的现象**：`ErrQueueFull` 不是没有闸，是闸在 user 层而调用方以为在用自己的 key。
  故本 change 定位从"再加限制"改为**把已有限制变可见/可分配/可归因**（见下 1.3b 与 M2.2）

## M1 字段 + 快照 + 只读暴露（行为零变化）

- [ ] 1.1 迁移 `250_api_key_capacity.sql`：`concurrency_limit int NOT NULL DEFAULT 0`、
  `rpm_limit int NOT NULL DEFAULT 0`、`effective_at timestamptz NULL` + ent schema + `go generate`
  ——验证：真 PG scratch 库跑 DDL、幂等重跑；存量行全为 0/null
- [ ] 1.2 `APIKeyAuthSnapshot` 加三字段并**升 `apiKeyAuthSnapshotVersion` v26→v27**，注释写明来源
  ——验证：单测断言「v26 快照被拒绝重建」；部署后首次鉴权触发重建且新字段非零（设一个非零值验证）
- [ ] 1.3 `capabilities.limits` 增 `concurrency_limit` / `rpm_limit` / `effective_at`
  ——验证：真实 key 调用返回三字段；默认值为 0/0/null 时不误报为"受限"
- [ ] 1.3b **有效上限与来源**：`effective_concurrency_limit` / `effective_rpm_limit` /
  `limit_source`（`key|user|group|unlimited`）——验证：构造「key=0 但 user=5」的用例，
  断言 `effective=5` 且 `limit_source=user`，且响应中**不出现"不限"**（这是最容易骗到人的一处）

## M2 key 级准入（默认不限，存量零影响）

- [ ] 2.1 准入闸：`apiKeyAuth` 之后、分组 allowlist 之前，按 `concurrency_limit`（进行中计数）
  与 `rpm_limit`（滑动一分钟）判定——验证：单测覆盖「未超限放行」「并发超限 429」「rpm 超限 429」
  三条；超限时**不触达上游**（无用量行、不占账号槽位）
- [ ] 2.2 超限错误体与既有 429 同形，带 `retry_after`、`limit_kind`（concurrency|rpm）与
  **`limit_scope`（key|user|group）**——验证：对比 chat 入口 429 的键集合仅多这两个；
  三种 scope 各造一例，断言调用方能看出是哪一层挡的（否则永远在猜）
- [ ] 2.3 fail-open：计数后端不可用时放行 + 递增可观测计数器——验证：注入后端故障，
  请求全部通过且计数器非零；**不得**出现全站 429
- [ ] 2.4 429 计入 `ops_error_logs`——验证:超限请求在错误表可见（否则稳定性口径漏掉最该看的一类）

## M3 生命周期：生效时刻 + 传播状态

- [ ] 3.1 `effective_at` 语义：未到时刻返回 `KEY_NOT_YET_EFFECTIVE`（**不是 401**），回传该时刻
  ——验证：构造未来生效的 key，鉴权通过但请求被拒；到点后自动放行（不重启）
- [ ] 3.2 `GET /api/v1/keys/{id}/state`：`config_applied` / `observed_snapshot_version` /
  计数后端类型与可用性——验证：改 concurrency 后立即查为 `false`，快照重建后为 `true`
  （这条直接回答"我怎么知道改的生效了"）

## M4 自助创建（用户面）

- [ ] 4.1 **扩展既有端点**（`/api/v1/keys` 已存在，见 `routes/user.go:76`）：`POST` 与
  `PUT /:id` 接受 `concurrency_limit` / `rpm_limit` / `effective_at`；超过
  `effective_user_limit` 返回 `LIMIT_EXCEEDS_UPSTREAM` 并回传两个数——验证：正例创建成功、
  负例被拒且错误含 `requested` 与 `upstream_limit`；**不得**静默取 min
- [ ] 4.1b **同额度内分配**：用户在其 5 并发额度内可给不同 key 分配（如 A=3、B=2），
  且 A+B 不超过 user 有效上限——验证：分配成功；超额分配被拒并说明剩余可分配额度
  （这是 key 级契约唯一真正新增的能力，前面那些只是把已有约束变可见）
- [ ] 4.2 既有 `GET /api/v1/keys` 已按身份隔离（沿用）；为新增的 `state` 端点补同样隔离：
  越权查他人 key 的 state 返回 **404**——验证：A 查 B 的 id 得 404 且响应无 B 的任何字段
- [x] 4.3 key 数上限与每小时频次（**已有**，`APIKeyCreateConfig.MaxActivePerUser` /
  `MaxPerUserPerHour`，`validateAPIKeyCreateRequest` 处校验）——本 change 只需确认扩容后仍生效
- [ ] 4.4 明文只在创建响应出现一次，后续查询一律投影（沿用 `hasKey` 式）——验证：创建响应外
  任何端点均不含明文；日志中亦无

## M5 基线与稳定性

- [ ] 5.1 `baseline`：按 `(platform, model)` 类取中位数；新 key 给 `source: population` +
  `confidence`；样本不足时数值为 `null` 且 `confidence: low`——验证：新 key 首次查询返回群体值；
  构造小样本类断言返回 `null` 而非假精确数
- [ ] 5.2 个人样本达阈值后切 `source: personal`——验证：造够样本后切源，且数值随自身数据变化
- [ ] 5.3 `stability`：`success_ratio` / `p95_latency_ms` / `throttle_ratio` / `active_hours`，
  窗口 1h/24h/7d——验证：**分母含失败**（有 429 时 `success_ratio` 严格 <1）；
  无数据时为 `null` 而非 0/1（"无数据"不等于"稳定"）

## M2b 长期机制（池 + 席位）

- [ ] 2b.1 两级准入实现：`admit = (池==0 ∨ userInflight<池) ∧ (硬顶==0 ∨ keyInflight<硬顶)`，
  两级计数均为进程内原子（生产单副本精确）——验证：单测覆盖四种组合
  （池满/key 满/两者皆满/皆未满）；多 key 合计不得超池的用例必须存在
- [ ] 2b.2 **计数泄漏防护**（长期方案的核心风险）：所有路径 `defer` 释放 + 周期性对账回收
  超时槽位 + `key_capacity_leaked_slots` 指标——验证：注入 panic/超时/客户端断开三条路径，
  计数归零；再注入一次"故意不释放"，对账在 N 秒内回收且指标非零（**泄漏必须可见**）
- [ ] 2b.3 分配不变量事务校验：`Σrequest ≤ 池`、`limit ≤ 池`、`request ≤ limit`，持锁，
  违反回传 `requested/remaining/capacity`——验证：三条各一负例；并发两个请求同时分配不得超卖
  （用两个事务并发跑，断言只有一个成功）
- [ ] 2b.4 剩余额度用查询聚合，不做物化计数器——验证：删除 key 后剩余额度立即回升，
  无补偿任务；代码里搜不到"剩余额度"字段

## M2c 容量规划助手

- [ ] 2c.1 `GET /api/v1/keys/capacity-plan`：返回 `capacity` / `allocated` / `remaining` /
  各 key 的 request+limit 表——验证：与手工 SQL 一致；剩余为 0 时给出"要提升需扩池"的明确提示
- [ ] 2c.2 针对现状给出可执行建议：89 人池=5 而人均 key 数未知 → 先测"每人 key 数分布"，
  据此给出"池容量该不该提、提到多少"的依据（不拍数字）——验证：给出分布与建议区间

## M6 验收

- [ ] 6.1 本地快包：`go build ./... && go test ./internal/lanes/ ./internal/server/routes/ && go vet`
  ——验证：全绿；`gofmt -l` 对新增文件为空
- [ ] 6.2 出片上产（专用线，`ciyuan-tag` 递增，迁移 250 执行核对）——验证：GHA 双绿、
  digest 一致、rollout 绿、`api_keys` 三列已建
- [ ] 6.3 真机四件：①存量 key 行为不变；②设 `concurrency_limit=1` 并发打两发，第二发 429 且
  `limit_kind=concurrency`；③未来 `effective_at` 的 key 被拒且到点自动放行；④新 key 的
  `baseline` 返回群体值 + 置信度——验证：四条各留 JSON 存档
- [ ] 6.4 稳定性口径上报表：`tools/line-metrics.sql` 或新 `tools/key-health.sql` 增一节按 key——
  验证：能回答"这把 key 成功率多少、p95 多少、429 占多少、什么时候最忙"
