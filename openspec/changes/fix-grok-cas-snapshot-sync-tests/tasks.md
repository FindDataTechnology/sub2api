# Tasks: fix-grok-cas-snapshot-sync-tests

## 1. 定位并发超时（D3，先定性再改）

- [ ] 1.1 读 `TestGrokCatalogCASConcurrentIndependentTransactionsMergeSuccess` 的同步逻辑，确认两个 goroutine 各自等什么。验证：能说出「谁在等谁」
- [ ] 1.2 判断 30s 超时的性质：行锁等待 / 测试自身同步 bug / 加密后 GetByID 变慢。验证：结论有证据（如日志时序、锁等待统计），不是猜测
- [ ] 1.3 若确属测试同步逻辑问题 → 修测试；若属锁语义变化 → 按新语义重写并在注释写明

## 2. 按 outbox 契约重设断言（D1）

- [ ] 2.1 `TestGrokCatalogCASPreservesSingleKeyAndRejectsStaleExpected`：把「断 schedulerCache 被写」改为「断 `scheduler_outbox` 有对应行」，移除 skip。验证：断言里能查到 outbox 行
- [ ] 2.2 同文件加一条负向断言：stale expected 时 CAS 必须被拒（已有 `applied == false` 的断言，确认保留且真实生效）
- [ ] 2.3 移除两处 `t.Skip` 的跳过说明

## 3. 回滚语义用例重写（D2）

- [ ] 3.1 `TestGrokOAuthConditionalMutationRollsBackWhenOutboxInsertFails`：改为注入「确定失败」的 executor（只读事务或违反约束），而不是靠模拟层
- [ ] 3.2 断言从 `require.Error(t, err)` 改为「账号行未被修改」——查 `credentials_encrypted` 与 `updated_at` 未变
- [ ] 3.3 移除 skip

## 4. 同类模式核对

- [ ] 4.1 全仓扫「断 schedulerCache 在事务内被写」的测试，确认没有别的漏网。验证：`grep -rn "setAccounts" backend/internal/**/*_test.go` 逐条看是否在事务内
- [ ] 4.2 若还有同类，一并按 D1 改或登记到本 change

## 5. 验证与收口

- [ ] 5.1 CI 全绿（含 integration，且三处 skip 已移除）。验证：`gh run view <run> --json jobs` 六个 job 全 success
- [ ] 5.2 确认三个用例**确实执行了**（不是被别的 skip 连带跳过）。验证：CI 日志里能找到这三个测试的 RUN/PASS 行
- [ ] 5.3 更新 `docs/KNOWN-UPSTREAM-FAILURES.md`：把这三个从「已知债务」移到「已修复」
- [ ] 5.4 归档本 change
