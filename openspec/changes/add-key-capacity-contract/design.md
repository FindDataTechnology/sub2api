# Design: add-key-capacity-contract

## Context

### 为什么"以 key 为单位"是对的

上游并发（`account.Concurrency`）是**供给侧事实**，不由我们决定，且会随运维变化。
而 **key 是我们对外的承诺单位**：谁能用多少、用到什么时候、稳不稳定，都该落在 key 上。
今天的错位是——承诺在 key 上，控制却在账号上：所以挡住请求的是 429（账号满了），
而不是"你超过了你的额度"（key 的契约）。

### 实证现状（2026-11-11）

```
api_keys 的 28 列（生产库实查）：
  id, user_id, key, name, group_id, status, created_at, updated_at, deleted_at,
  ip_whitelist, ip_blacklist, quota, quota_used, expires_at, last_used_at,
  rate_limit_5h, rate_limit_1d, rate_limit_7d,
  usage_5h, usage_1d, usage_7d, window_5h_start, window_1d_start, window_7d_start
```

- **无 concurrency 列、无 rpm 列、无 effective_at**。USD 窗口三档（5h/1d/7d）已有且带用量与窗口起点。
- `CurrentConcurrency` 只出现在 DTO 里（`handler/dto/types.go:73` 注释 "real-time active request count"），**不可配**。
- 并发只在两处：`account.Concurrency`（`tryAcquireAccountSlot`，上游）、`APIKeyAuthUserSnapshot.Concurrency`（用户级）。
- 认证快照缓存：L1 + L2 + negative 三级 TTL（`api_key_auth_cache_impl.go:25-41`，来自 `auth.L1TTLSeconds` 等配置）→ **配置改动不是瞬时生效**，这正是"key 生效时间"必须显式建模的原因。
- **用户面自助通道已存在**：`internal/server/routes/user.go:76` 的 `/api/v1/keys` 提供
  `GET/POST/PUT/DELETE` + `GET /:id`，防滥用上限 `APIKeyCreateConfig.MaxActivePerUser` /
  `MaxPerUserPerHour` 已实现。（我初查只 grep 了 `admin.go`，误判为"只有 admin 面"——已订正。）
  真正的缺口是**创建/更新时无法声明容量契约**（`CreateAPIKeyRequest` 只有 quota 一类字段）。

### M0 已核（2026-11-11）

| 项 | 实测 | 对设计的影响 |
|---|---|---|
| 副本数 | **1 replicas / 1 ready** | key 级并发计数**用进程内即可精确**，不需要 Redis；Q-a 关闭 |
| 认证快照 TTL | 走默认配置（pod 内无 `*TTL*` 环境变量），字段为 `auth.l1_ttl_seconds` / `l2_ttl_seconds` / `negative_ttl_seconds` | "改配置多久生效"= TTL 上界；必须由 `state` 端点回报实际观测版本，不能让运维猜 |
| 用户面 key 端点 | 已存在（见上） | 自助创建不是新端点，是**扩字段** |

### 40 天数据告诉我们"没有 key 级闸"的代价

`ops_error_logs` 7 天 30,724 条：403 占 50.1%（其中 15,392 条来自一把
`wx-wizard-verify`——`No active subscription found`）、429 占 6.6%。这些不是业务失败，
是**门挡住的**。若 key 有明确的 concurrency/rpm 契约与可读的超限原因，
调用方看到的是"你超了你的 5 并发"，而不是"网关 429 你去重试吧"。

## Goals / Non-Goals

**Goals**

- G1 key 成为可声明的容量契约：并发上限、每分钟上限、生效时刻，三者可配、可查、可审计。
- G2 平台侧**立刻**能说清一个新 key 的基础情况（继承类基线 + 置信度），不靠人肉 SQL。
- G3 "生效时间"是明确语义：`effective_at` 之前拒绝、配置改动有可观测的传播状态。
- G4 稳定性按 key 可得：成功率 / p95 / 429 占比 / 活跃时段。
- G5 限流器故障时**放行并打点**，绝不因限流后端不可用导致全站 429。

**Non-Goals**

- 不改 `account.Concurrency` 与账号层 AIMD/队列（上游事实，另有其主人）。
- 不做价格；不做上游模型稳定性。
- 不做按 key 的计费改造（USD 窗口已存在，本 change 只加准入维度）。
- 不做前端（先给 API；控制台另立）。

## Decisions

**D1 三个新字段，语义对齐既有约定，不造新词。**
- `concurrency_limit int, 0=不限`（对齐 `quota=0` 的既有"0 即不限"语义）
- `rpm_limit int, 0=不限`（对齐 group/user 的 `RPMLimit` 语义）
- `effective_at timestamptz, null=立即生效`（对齐 `expires_at` 的反面；两者共同构成生命周期区间）

备选「复用 group 的 RPMLimit」被否：group 是**多个 key 共享**的，无法表达"这把 key 只给 5 并发"。

**D2 必须升认证快照版本号。**
三字段进 `APIKeyAuthSnapshot` → `apiKeyAuthSnapshotVersion` v26 → **v27**，并在注释里写明来源。
不升的后果本仓已实证过：线上存量同号快照按旧形状反序列化，新字段静默为 0 → 限流形同不存在。

**D3 准入放在鉴权链上、分组门之前，与既有门同一位置。**
顺序：`apiKeyAuth` → **key 准入（新）** → 分组 allowlist → 转发。
理由：key 的契约先于分组语义；且超限时不该先做一次 allowlist 计算。
超限返回与既有 429 同形错误体 + `retry_after`，并带 `limit_kind`（`concurrency`|`rpm`）
——调用方能区分"我太快了"与"上游满了"。

**D4 限流器故障 → 放行 + 打点（fail-open），不是 fail-closed。**
备选 fail-closed 被否：限流后端抖动会让**所有** key 全 429，比超限更糟。
判据：后端不可用计数器必须可在指标里看到（否则"放行"会变成永久静默失效）。

**D5 新 key 的基线 = 类群体 + 收缩，不是编造。**
新 key 无历史，基线从 `(platform, model 类)` 的群体统计继承：
- `sustainable_rps` / `p95_latency` / `error_ratio` 取该类近窗**中位数**（不是均值，避免长尾污染）
- `confidence`：样本分钟数 ≥ N 为 `medium`，≥ 3N 为 `high`，否则 `low`
- `source: "population"`
- 随着该 key 自身样本累积（≥ M 次调用），切换为 `source: "personal"` 并给出个人值

**MUST NOT** 在 `low` 置信度下给出小数精度的数字假装精确——给区间或 `null`。

**D6 生效时间分两层，都显式建模。**
- **首次生效**：`effective_at` 之前，鉴权通过但返回 `KEY_NOT_YET_EFFECTIVE`（不是 401，
  因为身份是有效的，只是时间未到）。
- **配置变更传播**：改 concurrency/rpm 后，快照缓存有 TTL。提供
  `GET /api/v1/keys/{id}/state` 回报 `config_applied: bool` + `observed_snapshot_version`，
  让运维能回答"我改的生效了吗"——而不是猜。

**D7 自助创建只能向下声明，不能向上。**
用户面 `POST /api/v1/keys`：请求的 `concurrency_limit` / `rpm_limit` 若超过其 user 上限或
group 上限 → **拒绝**（`LIMIT_EXCEEDS_UPSTREAM`，附两个数），不静默截断。
备选"静默取 min"被否：静默截断会让用户以为拿到了 20 并发，实际只有 5——
本仓对"静默降级"的教训已经够多。

## 稳定性口径（必须先定义，否则报表各解各的）

按 key、按窗口（1h / 24h / 7d）：

| 指标 | 定义 | 数据源 |
|---|---|---|
| `success_ratio` | 2xx 完成 / 全部尝试 | `usage_logs`（成功计费行）+ `ops_error_logs` |
| `p95_latency_ms` | 成功请求的 p95 | `usage_logs.duration_ms` |
| `throttle_ratio` | 429 占比 | `ops_error_logs.status_code=429` |
| `active_hours` | 有调用的 UTC 小时分布 | `usage_logs.created_at` |

**关键纪律**：`success_ratio` 的分母必须含失败（`usage_logs` 只有成功行，
只用它算会得到"100% 成功"的假象）——这正是本仓"读不到当成不存在"的同族陷阱。
两个来源都缺时给 `null`，不给 100%。

## Risks / Trade-offs

- **[快照版本号漏升]** 新字段静默为 0 → 限流失效且无报错。缓解：D2 + 单测断言
  "旧版本快照被拒绝重建"（沿用 `ciyuan-upstream-sync` 的 v25→v26 先例）。
- **[热路径多一次外部依赖]** key 准入需要并发计数（Redis/内存）。缓解：D4 fail-open +
  指标；且并发计数优先用进程内（单实例足够近似），Redis 只做跨实例聚合。
- **[限流粒度错配]** 若多实例部署，进程内计数会低估。缓解：明写"v1 以单实例精确、
  多实例近似"，并在 `state` 端点回报计数后端类型——不假装精确。
- **[基线误导]** 群体基线用在高异质类上会失真。缓解：D5 的中位数 + 置信度 + 切个人值。
- **[自助滥建]** 用户自助建 key 可能刷爆 key 数量。缓解：每用户 key 数上限（沿用既有配额思路），
  超限拒绝并说明。

## Migration Plan

1. **M1 字段与快照**（迁移 + 版本号 + 只读暴露）：新列默认 0/null，行为不变；
   `capabilities.limits` 增字段。回滚=删列 + 版本号回退（旧快照自然过期）。
2. **M2 准入**：key 级 concurrency/rpm 生效（默认 0=不限，故存量 key 零影响）。
3. **M3 自助创建**：用户面端点 + 向下声明约束。
4. **M4 基线与稳定性**：`baseline` / `stability` 两段。

**回滚**：任一阶段可关（`KEY_CAPACITY_ENFORCE=off` 即回到今天）。

## Open Questions

- **~~Q-a~~ 已答（2026-11-11）**：生产 **1 replicas**，故 key 级并发计数用进程内即精确；
  若将来扩副本，必须改为共享计数并在 `state` 端点回报计数后端类型（否则会低估 N 倍）。
- **Q-b** 每用户 key 数上限取值？（先按现状最大用户 key 数（29）留余量）
- **Q-c** `effective_at` 是否需要"预约失效"（future expiry）？`expires_at` 已支持未来时刻，
  故 v1 不额外做。
