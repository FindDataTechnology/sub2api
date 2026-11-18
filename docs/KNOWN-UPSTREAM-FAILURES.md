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
- 集成测试失败：fork 的凭据加密改造与上游测试不兼容，已在 `merge-upstream-v0-2-23` 修完。
