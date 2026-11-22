## 状态：**在途，已有进展（2026-11-20 治理复核）**

M0 前置核查已 4/31 完成（`0.1` 副本数=1、`0.2` 认证快照 TTL、`0.3` 存量上限分布），
且 0.3 的结论**改变了本 change 的定位**（见 tasks）。M1/M2 未开工。

定位：这是词元**唯一的容量治理** change，不是可选优化——`users.concurrency=5 × 89 人`、
`rpm_limit=0 × 118 人` 是当前的真实形态，加 key 即 N×5 的绕过风险是结构性的。
保留在途。**下一动作**：M1（字段 + 快照 + 只读暴露，行为零变化）可以先做，风险最低。

## Why

用户要的是**长期方案**。之前几版都在"加字段、曝光限制"这一层打转，那只解决"看见"，
不解决结构问题。真正的结构问题是：

**今天的容量是一层看不见的天花板，不是一份可分配的额度。**

生产实证（2026-11-11）：

| 事实 | 数据 |
|---|---|
| 并发其实早就在 user 层限死 | `users.concurrency = 5` × **89 人**、0 × 29、30 × 1、1 × 1 |
| RPM 基本等于没有 | `users.rpm_limit = 0` × **118 人**、10 × 2 |
| 分组 RPM 形同虚设 | `groups.rpm_limit = 0` × 9 组、600 × 1 |
| key 层没有任何容量字段 | `api_keys` 28 列无 concurrency / rpm / 生效时刻 |

由此产生三个**结构性**毛病，都不是加字段能修的：

1. **看不见**：一个人开 N 把 key 分头干活，实际共用同一个 5 并发；撞墙收到 429，
   归因不到自己头上（7 天错误面里 403 占 50.1%、429 占 6.6%，大量属于这一类）。
2. **不可分配**：想把 5 并发分成"实时对话 3 + 批量索引 2"做不到——所有 key 抢同一个池，
   于是批处理饿死实时。
3. **可被绕过（最严重）**：一旦把 `concurrency_limit` 做成"每 key 独立上限"，
   一个用户开 N 把 key 就等于把额度变成 **N × 5**。只在分配时校验拦不住运行时并发——
   必须**两级同时强制**。

所以长期方案必须是：**以 user 为容量池、key 为池内席位；席位有保底（request）与硬顶（limit）；
运行时两级同时强制；保底之和不得超池；空闲可借支。** 这是最成熟的资源分配模型
（Kubernetes 的 request/limit、YARN 的 queue capacity 同一形状）。

> **两处订正**（初查只 grep 了 `admin.go` 就下结论）：
> ① 用户面 key 管理**已存在**——`internal/server/routes/user.go:76` 有 `/api/v1/keys` 全套 CRUD，
> 连 `APIKeyCreateConfig.MaxActivePerUser` / `MaxPerUserPerHour` 都已实现；
> ② 所以"能创建 key"不是缺口，缺口是**创建时无法声明容量**、**看不到有效上限**、
> **无法在池内分配**。

**明确不做**：不改 `account.Concurrency`（上游事实）；不做价格；不碰上游模型稳定性。

## What Changes

- **容量台账**：`user` 是池（复用既有 `concurrency` 字段作池容量），`key` 是席位
  （`concurrency_request` 保底 + `concurrency_limit` 硬顶），`rpm_limit` 同构。
  **不变量：活跃 key 的 `concurrency_request` 之和 ≤ 池容量**，事务内持锁校验。
- **两级运行时闸**：同时判 user 池与 key 席位。全局硬顶恒生效；保底只保证"不被抢"；
  空闲时允许借支到池上限（work-conserving）；设了 `concurrency_limit` 则是该 key 的硬顶。
- **有效值可解释**：任何上限同时给 `effective_*` 与 `limit_source`（key|user|unlimited），
  超限错误给 `limit_scope`（挡你的是哪一层）。
- **生命周期与回收**：`effective_at` / `expires_at` / 停用 → 席位自动回收。额度靠**查询聚合**
  得出，不维护易漂移的计数器。
- **容量规划助手**：给定池容量与各席位 request，返回剩余可分配与超额原因——让
  "5 并发是否要提到 20"变成可计算问题，而不是拍脑袋。
- **可观测**：按 key 的 `实际使用 / 保底 / 硬顶` 三元组 + 稳定性（成功率/p95/限流占比，分母含失败）。

## Capabilities

### New Capabilities
- `capacity-ledger`：user 池 / key 席位的分配台账、不变量校验、回收与一致性核对。
- `key-capacity-contract`：两级准入算法、超限归因、fail-open、计数泄漏防护。
- `key-self-service`：用户在池内自助分配席位（"请求不得超池""总额不得超额"两条硬校验）。
- `key-baseline-and-stability`：新 key 的群体基线（带置信度）与稳定性口径。

### Modified Capabilities
- `ciyuan-capability-endpoint`：`limits` 段增 `effective_concurrency_limit` /
  `concurrency_request` / `limit_source` / `effective_at`（只增不改）。

## Impact

- **数据库**：`api_keys` 加 3 列；`users` 复用既有 `concurrency` 作池容量（不新增列）。
  迁移从 **250** 起（249 属在途分支）。**不做数据回填**：存量 key 一律 `request=0 / limit=0`，
  语义="不占席位、可借支到池上限"，与今天行为完全一致。
- **认证快照**：三字段进快照 → **必须升 `apiKeyAuthSnapshotVersion` v26→v27**（不升则
  新字段静默为 0，限流形同不存在）。
- **热路径**：两级计数（user 池 + key 席位）用进程内原子计数（生产单副本，精确）；
  扩副本必须换共享计数——写进迁移条件，不作为默认假设。
- **对外契约**：能力快照与用户面 key 端点扩字段；admin 面只增；chat 语义零变化。
- **计费**：不变（容量是准入，不是计价）。
