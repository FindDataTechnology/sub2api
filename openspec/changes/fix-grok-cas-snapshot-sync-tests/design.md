## Context

见 proposal — Why。设计所需现状（2026-11-19 实证）：

| 事实 | 证据 |
|---|---|
| 三处 `t.Skip` 位置 | `account_repo_grok_models_integration_test.go`（2 处）、`account_repo_integration_test.go`（1 处） |
| 冲突的设计 | `accountTransactionScope()`：ctx 带 tx 时 `scoped.schedulerCache = nil`（注释「禁止在提交前发布缓存」）——**上游原样，非 fork 改动** |
| 谁造成了失配 | fork 的凭据加密（迁移 251 删明文列）；测试需要 cryptor 才能读回凭据，而加了 cryptor 后事务内的 `GetByID` 才通得过 —— 但 `schedulerCache=nil` 让「事务内快照已写」的断言永远不成立 |
| 现有 outbox 机制 | `enqueueSchedulerOutbox` 在 35 处调用；`scheduler_snapshot_outbox_integration_test.go` 有 `TestSchedulerSnapshotOutboxReplay` |
| 三个用例的现象 | ①`"[]" should have 1 item(s)` ②并发 CAS 30s 超时 ③「期望 error 得 nil」 |

## Goals / Non-Goals

**Goals**

- 三处 `t.Skip` 移除，CI 重新覆盖这三条路径
- 每条用例的新断言都含负向场景（该失败时确实失败）

**Non-Goals**

- 不改 `accountTransactionScope` 的设计（「提交前不发布缓存」是对的：事务回滚时不该留下脏缓存）
- 不改 outbox 机制
- 不追查「为什么上游这三个测试本来能过」——上游跑在明文列 schema 下，
  与加密后不是同一套前提，没有可比性

## Decisions

### D1 断言口径改为「outbox 入队 + 提交后派发」

原先断「事务内 `schedulerCache.SetAccount` 被调过」，新口径断两件事：

1. 事务内 MUST 有对应的 `scheduler_outbox` 行（用 `scheduler_outbox WHERE account_id = $1` 查）
2. **不**断言订阅缓存被写（那是 `schedulerCache=nil` 明令禁止的）

**理由**：这才是 `accountTransactionScope` 的实际契约。断「缓存被写」等于要求它
违反自己的设计。

**备选**：改成不绑 tx（`testEntClient`）→ 否决。那会让测试失去事务隔离，
且 `BulkUpdate` 等路径的行为与生产不同。

### D2 第三个用例（期望 outbox 失败 → 整体回滚）改成断言回滚结果

它现在报「期望 error 得 nil」，说明模拟的 outbox 失败没触发。
新写法：注入一个**确定会让 INSERT 失败**的 executor（如只读事务或违反约束），
然后断言 **账号行未被修改**（回滚生效）——这比断言 `err != nil` 更贴近要保护的属性。

**理由**：「返回了 error」不等于「数据没被改」；回滚才是要验证的东西。

### D3 并发 CAS 超时的用例：先定性再改

30 秒超时的原因未查清（可能是行锁等待，也可能是测试自身同步逻辑）。
本 change 的**第一个任务**是定位它，而不是直接改断言。若确认是「加密后 GetByID 变慢
导致两个 goroutine 互相等待」，则在测试里放宽超时并注明；若是锁语义变化，按新锁语义重写。

## Risks / Trade-offs

- [新断言写出「永远通过」的形状] → D1 明确要求含负向断言；每个用例必须有
  「不该成功时确实失败」的分支
- [并发用例超时只是放宽了事] → D3 要求先定性；放宽超时必须在注释里写明实测数值与原因
- [改测试掩盖了真 bug] → 三个用例的失败现象里，只有第三个「期望 error 得 nil」可能是真 bug
  的证据；D2 改成断言回滚结果，若回滚真没生效，新断言会红——不会掩盖

## Migration Plan

纯测试改动，无迁移。三步：
1. 定位并发超时原因（D3）
2. 三个用例按 D1/D2 重写断言，移除 `t.Skip`
3. 本地跑不了（用户约束），由 CI 验证

**回滚**：还原三个测试文件即回跳过状态。

## Open Questions

无。
