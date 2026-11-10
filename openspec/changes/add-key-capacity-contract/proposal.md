## Why

用户提出：真正要的是**以 key 为单位的容量契约与生命周期**——让人能申请/创建不同用途的 key、
配置它的 concurrency 与限额，平台侧能**快速说清一个新 key 的基础情况**，并且知道
**key 什么时候生效、稳不稳定**。

对着生产库与代码核了一遍，这些**大部分今天不存在**：

> **先订正两处我自己的误判**（初查只 grep 了 `admin.go` 就下了结论）：
> ① 用户面 key 管理**已经存在**——`internal/server/routes/user.go:76` 有
> `GET/POST/PUT/DELETE /api/v1/keys` + `GET /api/v1/keys/:id`，连防滥用上限
> `APIKeyCreateConfig.MaxActivePerUser` / `MaxPerUserPerHour` 都已实现。
> ② 所以「让用户能创建 key」**不是缺口**；真正的缺口是**创建时无法声明容量契约**
> （concurrency / rpm / 生效时刻），以及**没有 key 级基线/稳定性口径**。

| 需求 | 现状（实证 2026-11-11） |
|---|---|
| 用户自助申请/创建 key | **已有**：`/api/v1/keys` 全套 CRUD + 每用户 key 数与频次上限。缺的是**容量契约字段** |
| **配置 key 的 concurrency** | **不存在**：`api_keys` 表 28 列里没有 concurrency；并发只在**账号层**（`account.Concurrency` → `tryAcquireAccountSlot`）与 **user 层**（`APIKeyAuthUserSnapshot.Concurrency`）。`CurrentConcurrency` 只是运行时展示计数，不可配 |
| 新 key 的"基础情况" | **算不出来**：新 key 无历史。同类 key 的实测只能靠人肉 SQL（我们已写过 `tools/line-metrics.sql`，但那是按线不是按键） |
| key 生效时间 | 只有 `expires_at`（失效），**没有"生效"语义**；改配置的传播时延也未量化（认证快照有 L1/L2 两级缓存 + Redis） |
| key 稳定性 | **无**：`usage_logs` 与 `ops_error_logs` 有原料，但没有聚合到 key 的稳定性口径 |

后果很具体：**今天想给某条线开一把"限 5 并发、每天 $20、只在低谷跑"的 key，做不到**——
只能建一把无并发上限的 key，然后靠账号层 429 反过来限它。这就是为什么错误面里
50% 是 403/429 这类"门挡住的"而不是业务失败。

本 change 把 **key 变成容量契约的载体**：创建时声明能力边界，平台能立即给出该 key 的
基线预期，运行期按 key 计量并反馈稳定性。

**明确不做**：不改账号层并发机制（`account.Concurrency` 保持原样，它是上游事实）；
不新建价格体系；不碰上游不稳定的问题。

## What Changes

- **key 容量契约**：`api_keys` 增三个可配字段——`concurrency_limit`（并发上限，0=不限，沿用
  `quota=0` 的既有语义）、`rpm_limit`（每分钟请求上限，0=不限）、`effective_at`（生效时刻，
  默认创建即生效）。三者都进认证快照（**需升 `apiKeyAuthSnapshotVersion`**）。
- **key 级准入闸**：在既有鉴权链上、分组门之前，按 key 的 `concurrency_limit` / `rpm_limit`
  做准入；超限返回与既有 429 同形的错误体并带 `retry_after`。
- **自助创建扩展**（端点已存在，只扩字段）：`POST /api/v1/keys` 与 `PUT /api/v1/keys/:id`
  接受 `concurrency_limit` / `rpm_limit` / `effective_at`，且**只能向下声明**——超过 user 或
  group 上限即拒绝（`LIMIT_EXCEEDS_UPSTREAM`，回传两个数），不静默截断。
- **新 key 基线（`baseline`）**：创建响应与查询端点给出该 key 的**预期基线**——从
  `(platform, model 类)` 的群体实测继承，带 `confidence` 与 `source`（`population` / `personal`）。
  数据不足时 `confidence: "low"`，MUST NOT 编造数字。
- **生效时间语义**：`effective_at` 之前 key 鉴权通过但拒绝（`KEY_NOT_YET_EFFECTIVE`）；
  配置改动通过快照版本号 + 精确失效传播，并提供 `GET /api/v1/keys/{id}/state` 回报
  "配置已生效/仍在旧快照"。
- **稳定性口径**：按 key 聚合 `成功率 / p95 延迟 / 429 占比 / 最活跃时段`，窗口 1h/24h/7d，
  缺数据如实 `null`。

## Capabilities

### New Capabilities
- `key-capacity-contract`：key 的容量与生命周期字段（concurrency/rpm/effective_at）、
  准入语义、超限错误形状。
- `key-self-service`：用户自助创建与查看 key，含"只能声明不超过上位上限"的约束。
- `key-baseline-and-stability`：新 key 的群体继承基线（带置信度）与运行期稳定性口径。

### Modified Capabilities
- `ciyuan-capability-endpoint`：`limits` 段增 `concurrency_limit` / `rpm_limit` /
  `effective_at`（只增不改既有字段）。

## Impact

- **数据库**：`api_keys` 加 3 列（迁移从 **250** 起；249 属在途分支）。
- **认证快照**：`APIKeyAuthSnapshot` 加 3 字段 → **必须升 `apiKeyAuthSnapshotVersion`**（v26 → v27），
  否则线上存量快照按旧形状反序列化会让新字段静默为 0（本仓已有 v25↔v25 碰撞先例）。
- **热路径**：key 级准入是新增一次判断（memcached 计数器 + 快照读），必须 O(1) 且可降级：
  计数后端不可用时**放行**并打点（限流器故障不应变成全站 429）。
- **对外契约**：新增用户面端点与三个响应段；admin 面字段只增。
- **计费**：不变（key 级并发/RPM 是准入，不是计价）。
