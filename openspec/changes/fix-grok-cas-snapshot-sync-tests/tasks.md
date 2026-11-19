# Tasks: fix-grok-cas-snapshot-sync-tests

> **实际结果与初版计划的差异**已记在 design.md 的「实施结果」一节：
> 三个用例的真因都不是「断言口径与 schedulerCache=nil 冲突」，
> 而是缺 cryptor（×2）与失败注入器假设了错误的参数位置（×1）。
> 下面按**实际执行**的记录。

## 1. 定性（先查原因，不改代码）

- [x] 1.1 读 `TestGrokCatalogCASConcurrentIndependentTransactionsMergeSuccess` 的同步逻辑：两个 goroutine 各持一个 tx，靠 `grokCatalogGatingRepo` 的 `entered`/`release` channel 强制交错
- [x] 1.2 定性 30s 超时：**不是锁等待**。`PersistGrokCatalogObservation` 第一步 `repo.GetByID()` 因 cryptor=nil 解不开密文凭据而提前返回 → `CompareAndSwap` 未被调用 → `entered` 永不收到信号。证据：补齐 cryptor 后该用例直接通过
- [x] 1.3 第三个用例（回滚）定性：`failAtomicSchedulerOutboxSQLExecutor` 靠「末位参数置 nil」制造失败，但加密改造把 event_type 从 `$8` 挪到了 `$7`，末位变成 `credentials_hash`

## 2. 两个 Grok CAS 用例（真因：缺 cryptor）

- [x] 2.1 `account_repo_grok_models_integration_test.go` 5 处 repo 构造改用 `newAccountRepositoryWithSQLCrypto(..., credentialTestEncryptor(t))`。验证：文件内 `newAccountRepositoryWithSQL(` 归零
- [x] 2.2 移除两处 `t.Skip`。验证：文件中 `t.Skip(` 归零
- [x] 2.3 **断言原样保留**（不需要按 D1 改口径——问题不在口径）

## 3. 回滚用例（真因：失败注入器假设了末位）

- [x] 3.1 `failAtomicSchedulerOutboxSQLExecutor` 改为从 SQL 解析真实位置：正则 `INSERT INTO scheduler_outbox[^;]*?SELECT\s+\$(\d+)` 取出 event_type 的 `$N`，再置该位置为 nil
- [x] 3.2 移除 skip
- [x] 3.3 断言原样保留（`require.Error` + 「行未被修改」+「outbox 计数为 0」三条都在）

## 4. 同类模式核对

- [x] 4.1 全仓扫 `newAccountRepositoryWithSQL(` 在测试里的用法，涉密路径的已统一补 cryptor
- [x] 4.2 参数个数静态校验（`newAccountRepositoryWithSQLCrypto` 必须 4 个参数）全仓 0 异常

## 5. 验证与收口

- [x] 5.1 CI 全绿：`gh run view <run> --json jobs` 六个 job 全 success，`conclusion: success`
- [x] 5.2 三个用例**确实执行且通过**（无 `t.Skip`，`FAIL: 0`）
- [ ] 5.3 更新 `docs/KNOWN-UPSTREAM-FAILURES.md`：把这三个从「已知债务」移到「已修复」
- [ ] 5.4 归档本 change（`openspec archive fix-grok-cas-snapshot-sync-tests`）
