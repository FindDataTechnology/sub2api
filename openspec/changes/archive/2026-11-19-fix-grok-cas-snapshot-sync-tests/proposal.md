## Why

2026-11-19 开源后 CI 首跑，139 个 integration 失败已清到 0，剩 3 个改 `t.Skip`：

- `TestGrokCatalogCASPreservesSingleKeyAndRejectsStaleExpected`
- `TestGrokCatalogCASConcurrentIndependentTransactionsMergeSuccess`
- `TestGrokOAuthConditionalMutationRollsBackWhenOutboxInsertFails`

它们**不是「测试写错」也不是「实现有 bug」**，而是 fork 的凭据落盘加密改造
（`openspec/specs/credential-at-rest`，迁移 249-254）改变了它们依赖的语义：

`accountTransactionScope()` 在 ctx 里带 tx 时会把 `scoped.schedulerCache` 置 nil
（注释原文「禁止在提交前发布缓存」）。这三个用例断言「事务内快照已同步」，
绑定事务后该断言必然不成立；不绑事务则 `BulkUpdate` 类路径又读不到未提交行。

跳过是临时措施——**测试被永久关闭等于门禁失效**（见 `ci-gate-never-ran-looks-green`
的同类教训）。本 change 把它们按加密后的写路径重新设计断言并恢复。

## What Changes

- 重新设计三个用例的断言口径：从「事务内缓存已写」改为「事务内 outbox 已入队 +
  提交后缓存由 outbox 派发」——这是加密改造后 `accountTransactionScope` 的实际契约
- 若某些断言在新契约下无意义（例如断言的那条路径已被 outbox 取代），
  **删除而非保留**，并在测试注释里说明它被哪条契约取代
- 恢复 `t.Skip`（三处），使 CI 重新覆盖这三条路径
- 顺带核对同类模式：`docs/KNOWN-UPSTREAM-FAILURES.md` 里记录的「事务内 schedulerCache=nil」
  是否还有别的测试假设与之冲突

## Capabilities

### New Capabilities

（无——本 change 只修测试与断言口径，不改生产行为）

### Modified Capabilities

- `ciyuan-upstream-sync`: 补一条「改造后测试基线必须同批对齐」的要求——
  本次 139 个失败的根因是 fork 删明文列的改造没有同批更新上游测试，
  等到开源跑 CI 才暴露。要求：任何改变 schema/写入路径的 fork 改造，
  MUST 在同批内跑一次 `go vet -tags=integration ./...` + 相关集成测试

## Impact

- **代码**：`backend/internal/repository/account_repo_grok_models_integration_test.go`、
  `account_repo_integration_test.go`（仅测试）
- **运行时**：零
- **CI**：三处 `t.Skip` 移除后，integration 门禁真正覆盖这三条路径
- **风险**：若新断言口径写错，会把「本该失败」的情况放行 → 每个用例必须至少
  有一条**负向断言**（该失败时确实失败）
