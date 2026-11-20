## Why

fork 的**生产源** `c217716b0`（= `origin/release/grok-video-billing-0219-5` 尖端，镜像 `sha-9660ac8`，
自报 VERSION `ciyuan-v0.2.19-17`）相对上游 `v0.2.23` 落后 **413 个提交**，而上一次月度合并窗的合流提交
（`7993f1c99`，2026-11-05）已过去两周。上游 `v0.2.20`–`v0.2.23` 中含本线直接受益的内容：

- **Claude Fable / Mythos 5.2**、**GPT-6.2 Sol / Astra / Ultrafast**
- **GPT Image 2.6 原生图片链路**、图片侧计费三修
- **上游自己修了我们一直用补丁兜着的那处**：`cca745e8b fix(scheduler): keep openai_capabilities in the
  scheduler projection`——fork 的「调度投影凭据白名单」缝点因此可评估收编
- Go 工具链升 **1.27.4**（`ddb7eac63` / `e257b7811` 解 GO-2026-6859 安全告警）
- 一批 SSE/EventStream 边界、首个输出前空闲 failover 预算、零拷贝请求体读取、按用户合并结算等修复

漂移越久，`ciyuan-lane-upstream-registry` 与 `ciyuan-model-family-registry` 就要叠在更多未合并基线上，
冲突面更大——本 change 因此排在本次方案的第 0 位。

**本窗同时纠正两处基线认知错误**（前一份文档按 `main` 与 `v0.2.21` 假设，实测均不成立）：

1. 合流基线 **MUST 是生产线**（`origin/release/grok-video-billing-0219-5`），不是 `main`。实测
   `main` 比生产线少 14 个提交，其中 4 个与生产线上的提交 **patch-id 相同**（同一作业的重复副本）；
   以 `main` 为基线合并会在出片时把生产线上的修复回滚掉。
2. 窗口终点是 **`v0.2.23`**（2026-11-18 11:43），不是 `v0.2.21`；上游 `main` 之前是陈旧引用，
   「落后 196」的旧读数来自该陈旧引用。

## What Changes

- merge 上游 `v0.2.23`（含 v0.2.20/v0.2.21/v0.2.22）into **生产线分支**，产出新的发布线后合回 `main`
- **合并提交，不 rebase**，保 fork 提交 SHA 的外部引用（快照仓提交信息、`fd-infra-deploy` 清单注释、运营手册）
- 三方演练已跑（2026-11-18，`git merge-tree --write-tree`）：**13 处真文本冲突**，逐个按 tag 归因（见 design）
- 逐条核验并保全 fork 不变量；其中 `openai_capabilities` 投影白名单一条因上游已自行修复，改为**收编上游实现并保留自己的回归断言**
- 迁移编号共存核对：本窗新增碰撞为 **249 三占、250 三占、251 双占**（上一窗只有 246 双占）
- 认证快照版本常数按「双侧同号必再升一版」规则处理并留双来源注释
- ent 生成代码对账（`go generate ./ent`）
- 结清上一窗 3 笔挂账（缝点外冲突补登记、5.3 终验未勾、窗延迟未定性）
- 出片：`ciyuan-v0.2.23-1` tag + `ciyuan-tag` 文件同步 → 快照仓 → GHA → hkccr→relay→ccr →
  `fd-infra-deploy` 清单 → ArgoCD 手动 sync（selfHeal=off）

## Capabilities

### New Capabilities

（无——本 change 只改实现与工作流，行为契约不变）

### Modified Capabilities

- `ciyuan-upstream-sync`: 新增「跨多 tag 窗口必须显式声明基线与中途对照点」「合并窗挂账不得跨窗结转」
  「凭证落盘加密路径在上游合流后必须在生产副本回归」三条要求；并把「基线 = 生产线而非 main」写成
  合流纪律的一部分

## Impact

- **基线拓扑**（实测 2026-11-18）：

  | 引用 | 与上游 v0.2.23 | 说明 |
  |---|---|---|
  | `origin/release/grok-video-billing-0219-5`（= 生产源 `c217716b0`） | 落后 413 提交 | **合流基线** |
  | `main`（本地/远端） | 落后 427 提交 | 比生产线少 14 个提交 |
  | 当前工作分支 `fix/grok-video-billing-compat-shapes` | 含 4 个与生产线 patch-id 相同的重复提交 | 不用于出片 |

- **代码**：13 处真文本冲突 + 大量自动合并；`backend/ent/` 生成代码需对账
- **数据库**：三处编号碰撞按文件名字典序共存执行
- **出片链**：镜像 tag 滚动 + ArgoCD 手动 sync；回滚退 `sha-9660ac8` 即回旧版
- **运营面**：Fable/Mythos 5.2、GPT-6.2 系、Ultra Fast 门控、Vertex 多区域加价的价目/白名单核对
- **风险**：跨四个 tag、413 提交，是历次窗口最大的一次；凭证加密路径（`api_key_repo.go` 冲突）与
  上游 `custom_admin_key_scope` 重构是最需要盯的两处
