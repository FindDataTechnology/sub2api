# 已知红灯（非本仓引入）

本仓是上游 [Wei-Shaw/sub2api](https://github.com/Wei-Shaw/sub2api) 的增量谱系 fork。
上游的 CI **从未真正执行过单测**——所有 run 的状态是 `action_required`（首次贡献者待批准），
只有 release 构建在跑。因此上游主线长期带着红着没人知道的测试。

本文件登记这些**已知的、非本仓引入的**失败，用途只有一个：让 CI 的红是可辨识的——
一眼能分出「上游欠的」与「我们弄坏的」。

## 判别方法（任何一条红灯先做这三步）

```bash
# 1. 这个测试是上游的还是我们的？
git cat-file -e "v0.2.23:backend/internal/xxx_test.go" && echo 上游 || echo fork

# 2. 相关生产代码我们改过吗？（空 = 没碰）
git diff v0.2.23..HEAD --name-only | grep -E "<相关路径>"

# 3. 上游 CI 真的跑过吗？
gh run list --repo Wei-Shaw/sub2api --workflow CI --limit 3
```

三步下来「测试是上游的 + 生产代码我们没碰 + 上游 CI 没跑过」= 上游自带红灯，登记到下方。

## 在册清单（基线 `v0.2.23`，2026-11-18）

| 失败测试 | 症状 | 引入提交 | 判断 |
|---|---|---|---|
| `TestOpenAIMessagesFailoverExhaustedAppliesFailedFrameRule` | 期望透传 418，实际 500 | `f1013f177` fix(openai): SSE errors after keepalive, passthrough rules on exhaustion | 改错误处理未同步测试期望 |
| `TestOpenAIMessagesFirstEventFailedFrameExhaustedAppliesRule` | 同上 | 同上 | 同上 |
| `TestOpenAIMessagesNonStreamFailedFrameExhaustedAppliesRule` | 同上 | 同上 | 同上 |
| `TestOpenAIMessagesHTTPStatusFailoverExhaustedIgnoresPassthroughRule` | 同上 | 同上 | 同上 |
| `TestOpenAINativeAnthropicEndpointDiagnostics/messages/stream=false/HTTP_error` | 期望 418，实际 500 | 同上 | 同上 |
| `TestNativeImagesRoutesDelayedResponse/{generations,edits}/400` | 期望 400，实际 502 | `7f2876def` fix(openai): 打通 GPT Image 2.6 原生图片链路 | 改图片链路未同步透传断言 |

**处置决定（2026-11-18，用户裁决）**：**不修，等上游**。

理由：这些测试与词元业务无关；我们改上游测试会造成每次月度合流都在这些文件上冲突，
维护成本高于收益。CI 保持红，但红的原因在册可查。

## 重新评估的时点

每个月度合流窗（见 `openspec/specs/ciyuan-upstream-sync`）复核一次：

- 上游若自己修了 → 从本表删除（`git log v0.2.24 --oneline -- <文件>` 确认）
- 上游仍未修且合流时冲突 → 重新评估是否接管
- 出现**新的**失败 → 先按上面的三步判别，是上游的才进表，是我们的就修

## 不属于本表的东西

- `golangci-lint` 的失败：那是**我们自己的代码**（`internal/lanes/`、`account_queue.go` 等），
  必须修，不得登记到这里。
- `Security Scan` 的依赖漏洞：需逐个评估是升依赖还是登记 exception（见 `.github/audit-exceptions.yml`）。
- 集成测试失败：**原先**是 fork 的凭据加密改造与上游测试不兼容（已在 `merge-upstream-v0-2-23` 修完
  那一批）；**现在**剩下的 `TestAccountRepoSuite` 快照同步冲突见下方「已知债务」。

## 已知债务（fork 侧，非上游）

### integration 已从 139 个失败清到 0（2026-11-19）

**根因**：fork 的凭据落盘加密改造（`openspec/specs/credential-at-rest`，迁移 249-254）
删掉了 `accounts.credentials` 与 `api_keys.key` 明文列，而上游那批测试假设明文列存在。

**修的四类**：
1. 测试构造 repo 时缺 cryptor（约 30 处）—— 写入/读取凭据派生列都要加密器
2. 测试直接引用已删的明文列（墓碑断言、schema 断言）
3. **可空 SQL 参数未标注类型** —— lib/pq 遇到 nil 会让整条 prepared statement
   的类型推断崩溃，且错误报在**别的参数**上（现象：7 个用例报
   `inconsistent types for parameter $4`，真因在 `$3`/`$6`/`$9`）。
   修法是给每个可空参数显式加 `::varchar` / `::text` / `::bigint`。
4. `BulkUpdate` 只改凭据时被早退分支静默跳过（**生产 bug**）

**最后 3 个也已修复**（2026-11-19，change `fix-grok-cas-snapshot-sync-tests`）。

初判「它们依赖事务内 schedulerCache 可用」是**错的**。真因两条：

1. `GrokCatalogCAS*` ×2：repo 用 `newAccountRepositoryWithSQL(..., nil)`（cryptor 为 nil）。
   `PersistGrokCatalogObservation` 第一步 `repo.GetByID()` 解不开密文凭据就提前返回，
   `CompareAndSwap` 根本没被调用 → 测试的 `entered` channel 收不到信号 → 30s 超时。
   补齐 cryptor 后断言**原样通过**，无需改动口径。
2. `GrokOAuthConditionalMutation*`：失败注入器靠「把末位参数置 nil」制造 outbox 失败
   （假设 event_type 是末位）。加密改造把 event_type 从 `$8` 挪到 `$7`，末位变成
   `credentials_hash`，置 nil 不再触发 NOT NULL 失败。改为从 SQL 解析
   `INSERT INTO scheduler_outbox ... SELECT $N` 的真实位置。

**教训**：又一次「错误现象指向错误位置」——「期望 error 得 nil」看起来像业务逻辑问题，
实际是测试夹具假设了参数位置。

**integration 已恢复为阻塞门禁**（`backend-ci.yml` 去掉了 `continue-on-error`）。

### golangci-lint 的 gofmt 检查与裸 gofmt 判据不同（**已解决**）

**症状**：CI 每轮只报 3 个 gofmt，按轮修了 4 轮还在报新的；而裸 `gofmt -l` 对全仓
报 95 个「未格式化」。

**真因**：`.golangci.yml` 的 `formatters.gofmt` 配了 `rewrite-rules`
（`interface{}` → `any`、`a[b:len(a)]` → `a[b:]`），裸 `gofmt` 不带这些规则。
所以两个工具对同一批文件给出相反结论——不是滚动上限，是判据不同。

**正解**：用 golangci 自己的格式化命令，与判据同源：

```bash
cd backend && GOTOOLCHAIN=go1.27.4 golangci-lint fmt
```

2026-11-19 用它一次清完 94 个文件（纯格式，`go build ./...` 仍 exit 0）。
**不要再按 CI 当轮报的文件逐个修**——那是打地鼠。
