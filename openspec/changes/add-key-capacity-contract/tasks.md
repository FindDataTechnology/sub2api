# Tasks: add-key-capacity-contract

> 用户点名重点：**让用户能申请/创建不同 key 并管理 concurrency 等配置；平台侧能快速说清
> 一个新 key 的基础情况；以及 key 的生效时间与稳定性。**
> 谱系纪律：实现落 `backend/internal/lanes/`（fork 自有）+ 已登记缝点；迁移从 250 起。
> **本 change 必须升 `apiKeyAuthSnapshotVersion`（v26→v27）**——没升就是静默失效。

## M0 前置核查（决定多实例形态，影响限流实现）

- [x] 0.1 确认生产副本数（**2026-11-11 完成**）：**1 replicas / 1 ready** → key 级并发计数
  用**进程内**即精确，不需要 Redis。若将来扩副本必须改共享计数并在 state 端点回报后端类型
  （否则低估 N 倍=限流失效）
- [ ] 0.2 确认认证快照 TTL 现值（**部分完成**）：pod 内无 `*TTL*` 环境变量 → 走默认值；
  字段定位到 `config.go:1722-1724`（`l1_ttl_seconds` / `l2_ttl_seconds` / `negative_ttl_seconds`）。
  仍缺：三个默认值的确切数字（需读 config 默认或 admin 设置面）——补齐后写清"改配置最长多久生效"
- [ ] 0.3 摸清现有 user/group 上限字段的实际取值（决定自助创建的"向下声明"上限从哪读）
  ——验证：给出 user.RPMLimit / group.RPMLimit / user.Concurrency 的生产分布（各档多少 key）

## M1 字段 + 快照 + 只读暴露（行为零变化）

- [ ] 1.1 迁移 `250_api_key_capacity.sql`：`concurrency_limit int NOT NULL DEFAULT 0`、
  `rpm_limit int NOT NULL DEFAULT 0`、`effective_at timestamptz NULL` + ent schema + `go generate`
  ——验证：真 PG scratch 库跑 DDL、幂等重跑；存量行全为 0/null
- [ ] 1.2 `APIKeyAuthSnapshot` 加三字段并**升 `apiKeyAuthSnapshotVersion` v26→v27**，注释写明来源
  ——验证：单测断言「v26 快照被拒绝重建」；部署后首次鉴权触发重建且新字段非零（设一个非零值验证）
- [ ] 1.3 `capabilities.limits` 增 `concurrency_limit` / `rpm_limit` / `effective_at`
  ——验证：真实 key 调用返回三字段；默认值为 0/0/null 时不误报为"受限"

## M2 key 级准入（默认不限，存量零影响）

- [ ] 2.1 准入闸：`apiKeyAuth` 之后、分组 allowlist 之前，按 `concurrency_limit`（进行中计数）
  与 `rpm_limit`（滑动一分钟）判定——验证：单测覆盖「未超限放行」「并发超限 429」「rpm 超限 429」
  三条；超限时**不触达上游**（无用量行、不占账号槽位）
- [ ] 2.2 超限错误体与既有 429 同形，带 `retry_after` 与 `limit_kind`（concurrency|rpm）
  ——验证：对比 chat 入口 429 的键集合，仅多 `limit_kind`；调用方可区分我方限流与上游满
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
  `PUT /:id` 接受 `concurrency_limit` / `rpm_limit` / `effective_at`；超上位上限返回
  `LIMIT_EXCEEDS_UPSTREAM` 并回传两个数——验证：正例创建成功、负例被拒且错误含
  `requested` 与 `upstream_limit`；**不得**静默取 min
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
