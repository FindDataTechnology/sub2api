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

## 长期模型：池 + 席位（request/limit 两级）

### 三层职责

| 层 | 字段 | 谁定 | 语义 |
|---|---|---|---|
| **池** | `users.concurrency`（既有，不新增） | 平台/运营 | 该用户**全部 key 合计**的最大并发，全局硬顶，恒生效 |
| **席位·保底** | `keys.concurrency_request`（新） | 用户/运营 | 该 key **保证拿到**的并发，不被别人抢；`Σ request ≤ 池容量` |
| **席位·硬顶** | `keys.concurrency_limit`（新） | 用户/运营 | 该 key 上限；0=不额外限制（可借支到池上限） |

### 运行时准入（O(1)，两级同时判）

```
admit ✓ 当且仅当
    (池容量 == 0 ∨ userInflight < 池容量)        # ① 全局硬顶，恒生效
  ∧ (硬顶 == 0 ∨ keyInflight < 硬顶)             # ② 该 key 的硬顶
```

只有这两个判据，**没有第三个**。理由见下——这是我在初稿里写错、自查后改正的一处。

**初稿错在哪**：我原来写了"池满时保底仍放行"。这是做不到的：池是硬顶，池满就是不能再进，
声称"保底能进"等于把池容量变成可超的，自相矛盾。

**由此必须说清的语义（诚实边界）**：

| 保底（request）保证什么 | **不**保证什么 |
|---|---|
| 保证**额度不被别人占用**（`Σrequest ≤ 池容量` 在分配时校验，别人无法把你这 3 个并发"申请走"） | 不保证**饱和度下的吞吐**——池被别人占满时，你的请求照样会被拒 |

**要吞吐保证，就设 `limit = request`**：此时该 key 被硬顶在自己的保底上，谁也抢不走它的份额；
代价是池里其余容量它也用不了。反之 `limit = 0` 表示"愿意共享"（work-conserving），
空闲时可用满池，但池被占满时也会被挡。

这样两个旋钮的含义是正交且可组合的：

| request | limit | 语义 |
|---|---|---|
| 0 | 0 | **等同今天**：不占席位、共享整池（存量 key 全部落在这里） |
| 3 | 0 | 保留 3 个并发额度给别的 key 用，自己空闲时仍可借满池 |
| 3 | 3 | **严格独占 3 个**：别人拿不走，自己也不多占 |
| 0 | 2 | 不保留额度，但自己最多 2（防止单 key 打满池） |

### 分配不变量（事务持锁）

```
Σ concurrency_request（该 user 全部 active key） ≤ user.concurrency   （池>0 时）
key.concurrency_limit ≤ user.concurrency                              （池>0 且硬顶>0 时）
key.concurrency_request ≤ key.concurrency_limit                        （硬顶>0 时）
```

违反 → 拒绝并回传 `requested` / `remaining` / `capacity` 三个数（不静默取 min）。

### 为什么"查询聚合"而不是"维护剩余额度计数器"

剩余 = 池容量 − Σrequest（查询得出）。**不做物化计数器**：漏减一次就永久漂移，且漂移后无自证
手段——本仓"计数器与实际不符"的教训已经够多。查询代价是一次索引扫描（key 量级几十到几百），可接受。

### RPM 形状不同，必须说清

RPM 是**速率**不是**池**：
- `key.rpm_limit` = 该 key 硬速率；`user.rpm_limit` = 全局速率（既有）。
- **不校验** `Σ key.rpm ≤ user.rpm`——速率可叠加（10 把 key 各 10rpm 合计 100rpm 物理上可能），
  求和校验会把合法用法判错。
- 但运行时**仍两级判**：key 超自己 → 429(`limit_scope=key`)；user 超全局 → 429(`limit_scope=user`)。

### 计数泄漏防护（长期方案必须回答）

并发计数"进 +1 / 出 −1"，泄漏一次该 key 就永久少一个并发且看不出来。三条防线：
1. 所有路径 `defer` 释放（含 panic / 上游超时 / 客户端断开）；
2. **周期性对账**：按最近活跃时间戳回收超时槽位，并递增 `key_capacity_leaked_slots` 指标
   ——泄漏必须可见，不能静默自愈；
3. `state` 端点回报 `inflight` 与 `leaked_recovered`，让运维看出"这个数在漂"。

## 存量上限分布（2026-11-11 实测）——它改变了本 change 的定位

| 字段 | 分布 | 读法 |
|---|---|---|
| `users.concurrency` | **5 × 89 人**、0 × 29、30 × 1、1 × 1 | 并发**已经在 user 层被限成 5**，且是绝大多数人的实际约束 |
| `users.rpm_limit` | **0 × 118 人**、10 × 2 | RPM 几乎全不限（0） |
| `groups.rpm_limit` | **0 × 9 组**、600 × 1 | 分组级 RPM 基本形同虚设 |

**这解释了一个此前没解释的现象**：`ErrQueueFull` → 429 不是"没有闸"，而是
**闸在 user 层（5 并发）而调用方以为自己在用 key**。用户在多把 key 之间分配工作时，
完全看不到那把 key 实际共享同一个 user 级 5 并发额度——一出问题就是 429，且归因不到自己头上。

**因此本 change 的定位要改**：不是"再加一层限制"，而是**把已存在的限制变得可见、可分配、可归因**：

1. **可见**：`capabilities.limits` 必须如实给出 key 实际生效的上限——**包含从 user/group 继承来的**
   有效值，而不只是 key 列上的值。key 列 0（不限）但 user 是 5，则 `effective_concurrency_limit: 5`，
   且 `limit_source: "user"`。**不这样做的就是欺骗**：告诉调用方"不限"而实际撞 5 并发 429。
2. **可分配**：用户能在自己的 5 并发额度内，给不同 key 分配（如 A key 3、B key 2），
   而不是所有 key 抢同一个池。
3. **可归因**：超限错误必须说明**是哪一层**挡的（`limit_scope: key|user|group`），
   否则调用方永远在猜。

### 由此新增一条硬要求

`concurrency_limit` 的语义必须是"**在该 key 的可用上限内**再切一刀"，而不是覆盖上位限制。
故校验规则：`key.concurrency_limit ≤ effective_user_limit`（user 为 0 时视为不限）。
超过则拒绝（`LIMIT_EXCEEDS_UPSTREAM`），并且 `state` 端点必须同时回报三层的值。

## 池容量怎么定：实测分布给出的答案（2026-11-11）

人均活跃 key 数分布（`api_keys where status='active'`）：

| key 数/人 | 用户数 |
|---|---|
| 1 | **37** |
| 2 | 7 |
| 3 | 4 |
| 8 | 1 |
| 11 | 1 |
| 19 | 1 |
| 22 | 1 |
| 42 | 1 |

**结论一：池=5 对大多数人不是瓶颈。** 37 人只用 1 把 key，池 5 意味着单 key 可用满 5，
完全够。对他们提池只会放大成本敞口。

**结论二：真正的问题是极端长尾。** 有人持 42 把 key——若按"每 key 独立上限"设计，
这人是 42×5=210 并发；而池模型下他仍然是 5。**这正是池模型必须存在的理由**，
也是为什么"给 key 加个 concurrency 字段"是错的方向。

**结论三：因此 v1 不改任何池容量**（89 人保持 5），只把机制建好：
- 需要多 key 分工的人，自己在 5 里分（A=3/B=2）；
- 需要更高额度的人，走"提池"（运营动作，有依据：他的 key 数与并发峰值）；
- 容量规划助手把"该不该提"变成查询，而不是谈判。

**给运营的建议区间（有依据，不是拍数）**：单 key 用户保持 5；多 key 用户（3+ key，本仓 5 人）
按"峰值并发 / key 数"评估后给 10–20。

## 生效时间：实测结论与定值（2026-11-11，回答"key 生效时间"）

### 三个默认值（源码实证，`config.go:2372-2378`）

| 项 | 默认 | 含义 |
|---|---|---|
| `api_key_auth_cache.l1_ttl_seconds` | **15**（jitter 10%） | 进程内 L1，命中不走 Redis/DB |
| `api_key_auth_cache.l2_ttl_seconds` | **300** | Redis L2，跨请求/跨重启存活 |
| `api_key_auth_cache.negative_ttl_seconds` | **30** | 无效 key 的负缓存 |

### 但**改 key 后是主动失效，不是等 TTL 过期**

`api_key_auth_cache_invalidate.go` → `deleteAuthCache`（`api_key_auth_cache_impl.go:258`）
**同时**清三处：`authCacheL1.Del`、`authNegativeCacheL1.Del`、`cache.DeleteAuthCache`（Redis L2）。
调用点覆盖 update/delete/重置用量/分组批量改（`api_key_service.go:596/934`、
`admin_group.go:1266/1446/1461/1484/1549`、`admin_user.go:397`）。

**且生产是单副本**（M0.1 实测 1 replicas），失效调用与被失效的进程是同一个 →
**改配置基本即时生效**，15s/300s 只是"没有主动失效时"的兜底。

### 定值（本 change 采用）

| 场景 | 生效时间 | 依据 |
|---|---|---|
| 单副本改 key 的 concurrency/rpm | **即时**（主动失效同进程） | 上面的实现实证 |
| 兜底上界 | **≤15s**（L1 TTL，jitter 后 ≤16.5s） | L1 默认 15s + 10% jitter |
| 若将来扩副本 | **≤15s**（其它实例的 L1 不可被动清） | 跨进程只能等 L1 过期或加广播失效 |

**故设计取值：不改 TTL 默认值。** 理由：单副本下主动失效已给到"即时"，改小 TTL 只会增加
DB/Redis 压力而不改善可观测行为；扩副本时真要"即时"，正确做法是**加失效广播**
（Redis pub/sub 或短周期版本轮询），而不是把 TTL 调到 1-2s——后者是拿数据库换正确性。

`state` 端点的 `observed_snapshot_version` 就是用来兜住这条的：即使有实例落后，
查询时能看出"它还在旧版本"，不需要运维靠等。

### 关于"不同渠道应该也不同"——目前的实测回答

按 `accounts.platform` 统计近 3 天延迟：

| platform | 样本 | avg | p50 | p95 | p99 |
|---|---|---|---|---|---|
| `openai` | 46,010 | 22.1s | 10.1s | 86.1s | 171.3s |

**只有一个渠道有量**（其余平台样本 <200，未入表）。所以"不同渠道生效时间/基线不同"这件事
**今天没有数据支撑**——不是不存在，是负载都压在同一条渠道上。因此本 change：
- 字段与端点按 `(platform, model)` 类设计，**结构上支持按渠道分化**；
- 但 v1 只对 `openai` 类给基线，其余类返回 `confidence: low` + `null`；
- **不**为"其它渠道"编造数值。等它们有量后同一套代码自动生效。

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
