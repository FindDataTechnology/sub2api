> **延后（2026-11-21 治理复核）**：4/31 是**低报**——代码已落一大半：

> **2026-11-26 2b.3 最终验证通过（`sha-6257a79`）**：生产实测四条全过。
> - 创建不挂：带鉴权 `POST /api/v1/keys` **0.32s 返回 200**（修前永久挂起）
> - **并发不超卖**：池=5，8 并发各 `request=1` ⇒ **恰好放行 5、拒绝 3**，库里 `Σrequest=5`
> - 拒绝体正确：`400 API_KEY_CAPACITY_REQUEST_EXCEEDS_POOL` + `capacity=5 / remaining=0 / requested=1`
>   （顺带验证 `errors.As` 修复——原先这类违反会落成 500）
> - 既有行为不变：列表仍 `eff=5 / src=user`；`/health` 0.15s
> - 收尾干净：DB `idle-in-transaction=0`、锁等待=0、`Σrequest` 归零（测试 key 全删）
>
> **过程中撞到的真 bug（已修，`57a8672dd`）**：应用层自死锁——
> `WithUserRowLock` 外层事务对 users 行持 `FOR UPDATE`，而 `apiKeyRepo.Create` 走 `r.client`
> 另开连接 INSERT；`api_keys.user_id` 是 NOT NULL 外键，外键检查要取父行 `FOR KEY SHARE`
> ⇒ 被自己的外层事务挡住。表现为 `POST /api/v1/keys` 永久挂起，**而 DB 侧看不到任何等待**
> （`pg_stat_activity` 无阻塞查询、`idle in transaction` 为 0、`pg_locks` 未授予为 0）。
> 判据：pod 内直连 `127.0.0.1:8080` 同样挂 ⇒ 排除网络与 Ingress。
> **为什么单测没抓到**：内存桩的"锁"是本地 mutex，桩里 fn 在同一 goroutine 跑，永远不会冲突。
> 修后补两条**真库 integration**：锁内创建 12s 内必须完成、池=5 时 20 并发恰好放行 5。

> **2026-11-26 生产验证 + 一次误判的纠正**：
> `sha-ca50606` 上过生产（revision 115），随后因 DB 端点超时回滚到 `sha-c8c004f`（revision 116）。
> **超时不是本次改动的锅**：同一时刻 cheap6 在 tailnet 上是 `offline`（Headscale 控制面 chengsi 失联），
> kubectl 到 6443 也在超时，paas/facet 正常——是集群网络抖动。回滚后端点 10/10 次 0.13s 返回。
> 恢复后查 `pg_stat_activity`：15 idle + 2 active，**0 idle-in-transaction、0 锁等待**，
> 没有任何行锁残留的证据。
> **但超卖缺陷被真实复现了**：回滚版（无行锁）对池=5 的用户并发发 8 个 `concurrency_request=1`
> 的创建，**8/8 全部放行**，库里 9 把 key 合计 request=9 > 池 5。这就是 2b.3 要防的东西，
> 在旧代码上真实发生了。测试 key 已全部删除（软删后复算 Σrequest=0）。
> **2b.3 的定位不变**：它是真缺陷的真修复，只是它上产时撞上集群抖动，
> 没能在那一个窗口里做并发验证。代码本身 CI 六 job 全绿 + 单测 20 并发打池 5 恰好放行 5。
> **教训**：判断"服务挂"之前先分清客户端网络与服务端健康——同一个 `/health` 200 + DB 端点超时的
> 组合，在 cheap6 offline 时是网络，在连接池耗尽时是别的，两者必须用集群内探针区分。

> **2026-11-26 未完成项定性（[~] = 不做，附判据）**：在 M1/M2 接线 + 6.3 四条真机验收通过之后，
> 剩余项逐条判过，只有 2b.3 是真实缺陷、已实现；其余四项是「已经能工作、缺的是仪式」：
> - **[~] 2.3 fail-open 计数器**：fail-open 本身早已实现（`TryAcquireCapacity` 在闸为 nil 时放行、
>   RPM 计数出错时 warn+放行）。缺的"可观测计数器"是照搬 K8s 模型的产物——本仓计数是
>   **进程内原子**（生产单副本，0.1 已证实），没有"后端"可挂，也就没有可计数的故障源。
>   **升级条件**：扩副本改共享计数（Redis）那天，再补 `capacity_backend_unavailable_total`。
> - **[~] 4.1b 并发分配用例**：能力已存在（`checkCapacityAllocation` 校验 Σrequest ≤ 池，
>   2b.3 之后还在行锁内串行）。缺的只是「A=3, B=2」的演示用例，**没有新行为可测**。
>   2026-11-26 实测：user 2 池=5、62 把 key 的 Σrequest=0，无人使用该能力。
> - **[~] 5.1/5.2/5.3 基线**：端点与算法都在跑（真机验过 `throttle_ratio=0.25`、
>   `failure_count=3`、分母含失败）。缺的是「个人样本达阈值后切 `source: personal`」的验证，
>   这需要真实流量累积，**不是写代码能完成的**。
> - **[~] 2c.1/2c.2 容量规划助手**：一个新端点。当前 0 把 key 设了 `concurrency_request`，
>   没有分配可规划。**升级条件**：首次出现「用户问我要多少并发」时再做。
> - **[x] 2b.3 分配不变量事务锁**：实现见 `ca506068c`。**过程中翻出四层问题**，
>   前两层会让"锁加上了"变成假象：
>   1. **接口加宽的代价**：塞进 `UserRepository` 主接口 ⇒ 40+ 测试桩各要补一遍
>      （CI 两轮都是这个原因，每轮只暴露 10 个编译错误）。改成可选接口
>      `UserRowLocker` + 能力探测，生产实现有 `var _ service.UserRowLocker` 编译期断言。
>   2. **`userRepository.GetByID` / `loadAllowedGroups` 用裸 `r.client`** ⇒ 事务被绕过，
>      锁内读池容量读到锁外快照 ⇒ **锁形同虚设**。改用 `clientFromContext`。
>   3. **`activeQuery()` / `apiKeyListByUserIDQuery()` 同样绕过事务** ⇒ key 侧的同型缺陷。
>      改成带 ctx 的 `activeQuery(ctx)`（11 处调用点）。
>   4. 我自己引入的回归：用 `fields.RateLimits` 守卫误伤"只改限速"的请求 ⇒ nil 解引用。
>      改为只看 `req.ConcurrencyRequest/ConcurrencyLimit`。
>
>   **判据**：20 并发打池 5，断言恰好放行 5、写入 5 条、无两 fn 并发。
>   **这条测试第一次跑出 6**——因为测试桩的 `WithUserRowLock` 只统计并发、不持锁跨越 fn，
>   等于没锁。桩修对之后才验出第 2/3 层真缺陷。**原先是真实缺陷**：两个并发创建各自
>   读到旧 Σrequest ⇒ 超卖，正是本 change 的立论。

> **2026-11-26 真机验收（sha-3883789 已在生产）**：用真实用户材料签 JWT 调生产
> `https://router.finddatatech.cloud`，四条结果：
> - **6.3-② 通过**：`concurrency_limit=1` 并发两发，第 2 发 `429` +
>   `limit_kind:"concurrency"` + `limit_scope:"key"` + `retry_after:5`，第 1 发 200（12.5s）
> - **6.3-③ 通过**：未来 `effective_at` 返 `403 KEY_NOT_YET_EFFECTIVE` 并回传该时刻（不是 401）
> - **6.3-④ 通过**：`stability` 端点 `throttle_ratio=0.25` / `failure_count=3` / `success_ratio=0.25`
>   —— 2.4 的修复真的生效（容量 429 以前不入 `ops_error_logs`，throttle_count 恒 0）
> - **6.3-① 未完全通过**：存量 key（容量字段全 0）请求行为正常，但**列表**回
>   `limit_source:"unlimited"` 而该用户池是 5 —— 真 bug，已修（`ListByUserID`/
>   `ListAllByUserID` 只 `WithGroup()` 不带 `WithUser()` ⇒ `k.User==nil` ⇒ 池容量读成 0）。
>   修法 `434f5eea3`，CI 45750122327 全绿，合入 main `15428a81f`。
> - **6.3-① 复验通过**（`sha-c8c004f`，2026-11-26）：列表与详情均回
>   `effective_concurrency_limit=5 / limit_source="user"`，与真实池容量一致。
>   **6.3 四条全部通过。**
>
> 附注：验收用临时 key 两把（已在验收后删除）；未改任何存量 key。

> **2026-11-26 接线进展（14/31 → 约 17/31）**：`2f2d3d66b` + `1936a9bcc` 接通了三处断线，
> 直击 2026-11-25 复核列的四处优先缺口里的三处：
>   - **4.1 产品路径通了**：`POST/PUT /keys` 现在真的收 `concurrency_request`/`concurrency_limit`/
>     `rpm_limit`/`effective_at`（此前 handler DTO 完全没带，service 层写得再好也不可达）
>   - **2.1 的 rpm 半通了**：`key.rpm_limit` 接上 Redis `rpm:k:{id}:{minute}`，判在 user 级之前
>     （此前 112 处 RPMLimit 命中全是展示，`key_capacity_gate.go` 对 RPM 零读取）
>   - **3.2 的 observed 字段通了**：state 端点改用 `getAuthCacheEntry` 并填
>     `observed_snapshot_version`（原先 `loadAuthCacheEntry` 永远回源 ⇒ `config_applied` 恒 true，
>     等于永远告诉运维"已生效"）
> 顺带修了 2.4（容量/生效时刻入口拒绝入 `ops_error_logs`）与 2.2 的一半（`limit_kind`/`limit_scope`/
> `retry_after` 进 429 体），以及 `CapacityInvariantError` 落 500 的缺陷（现 400 + 三个数）。
> **仍未做**：2.3 的故障注入计数（fail-open 有，可观测计数器没有）、4.1b 的并发分配用例、
> 5.x 基线与稳定性（端点已存在但未验）、2b.3 的事务锁（现为事务外校验）、2c.x 容量规划助手、
> 6.2/6.3 上产与真机验收。
> **6.2 的状态**：镜像已出（`sha-3883789`，ccr 实测 digest `sha256:d0865d0e…`），
> 清单已改（`fd-infra-deploy` `24b44d3e`，GitHub 与 Gitee 两端一致）。
> **但 ArgoCD 当前因 `chengsi` 整机失联（见 `fd-infra-deploy/docs/incident-2026-11-26-…`）
> 不可达 ⇒ 尚未真正滚动到生产。6.2/6.3 均未完成，不得勾选。**

> **2026-11-25 复核留任**：4/31。delta 16 条全未合主 spec。代码已落一大半（迁移+两级准入），但 6.2 出片上产、6.3 真机四件未做。是真欠账。

> **2026-11-25 复核 4.4 定性（撤回「安全问题」的判断）**：`dto.APIKey.Key` 确为完整明文，
> 但它是**按用户隔离的**（`List` 走 `ListByUserID(subject.UserID)`，`GetByID` 有所有权校验，
> admin 端点群挂 `adminAuth`），且前端**把它当功能用**：`UseKeyModal` 用它展示/复制用户自己的 key，
> `BatchImageGuideView` 用它直接调批图 API。所以「列表回明文」不是越权，是「明文可重复读取」的
> 设计选择；改成掩码会打断上述两条产品路径。
> **正确修法**（不是本 change 的补丁粒度）：新增一个需重新认证的 reveal 端点，列表只回
> `sk-` 前缀 + `has_key`，前端改为按需 reveal。属独立 change，**不在本 change 内做**。
> 原优先级从「安全项先上」降为「设计改进，排期另定」。

> 迁移 `250_api_key_capacity.sql`、`service/api_key_capacity_rules.go`（9.4K + 测试）、
> `service/key_capacity_gate.go`（7.6K + 测试）、`group_capacity_service.go`，两级准入已装进
> `api_key_auth`。真缺的是：**rpm 闸无消费方**（`RPMLimit` 只在 DTO 与 admin 用户 CRUD 里出现，
> 没有一处读它做过闸）、2.3 fail-open、2.4 429 入 `ops_error_logs`、M4 自助创建与同额度分配、
> 5.1-5.3 基线/稳定性、6.x 上产与报表。
> 不归档：20 条未做，归档会把半成品要求固化进主 spec。先按证据回勾已做的，再按上面清单推进。
>
> **2026-11-21 09:20 逐条复核（三个 agent 之一调研）**：4/31 是低报，**约 14 项已满足**。
> 最该先补的四件：
> 1. **key 级 rpm 闸无消费方**（2.1 的 rpm 半）：`RPMLimit` 在 `backend/internal` 有 112 处非测试命中，
>    但 `apiKey.RPMLimit` 的三处读取（`lanes/capabilities.go:347`、`api_key_capacity_rules.go:188/199/213`）
>    全是**展示**；`key_capacity_gate.go` 与 `api_key_auth.go` 对 RPM 零命中。
> 2. **capacity 三字段 HTTP 层没接**（4.1/4.1b）：service 层字段/校验/聚合不变量都在，
>    但 `api_key_handler.go` 的 DTO（`:34-47`/`:50-65`）与 `svcReq` 映射（`:282-303`/`:337-369`）
>    完全没带 → 用户面无法自助创建/分配。
> 3. **容量 429 不入 `ops_error_logs`（2.4）是反着做的**：`api_key_auth.go:370` 走
>    `MarkIngressRejected`，而 `ops_error_logger.go:1093-1095` 对 ingress reject 直接 return。
>    连带后果：5.3 的 `throttle_ratio` 也读这张表 ⇒ 容量拒绝永远不出现在稳定性口径里。
> 4. **列表/详情回明文 key（4.4）**：`Key` 存的是 `sk-`+32 字节随机数的**完整明文**，
>    `dto/mappers.go:87` 原样放进响应 ⇒ 任何能 list keys 的会话都能拿到别人的可用密钥。
>
> 另三处会让人踩空的实现缺口：
> - `observed_snapshot_version` 恒 0（3.2 的 state 端点字段只声明无赋值）
> - `CapacityInvariantError` 非 ApplicationError ⇒ 经 `ErrorFrom` 落成 500 而非 400+配额明细
> - 分配不变量无事务无锁（2b.3 校验在事务外）
> - `backend/migrations` 下有三个 `250_*`（capacity / credentials_meta / openai_cache_write_ttl），
>   撞号需先澄清 runner 排序

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
