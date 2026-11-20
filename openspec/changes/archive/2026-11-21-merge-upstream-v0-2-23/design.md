## Context

见 proposal.md — Why。此处只记本窗设计所需现状（2026-11-18 实测，非推断）。

| 事实 | 证据 |
|---|---|
| fork main 相对 `upstream/main` | 196 behind / 173 ahead；`backend/cmd/server/VERSION` 本地 0.2.18、上游 0.2.21 |
| 两侧都改过的文件（重叠面） | 67 个；其中非 ent、非测试的 backend/frontend 文件约 45 个 |
| 上一窗合流提交 | `7993f1c99`（2026-11-05 23:52）→ tag `ciyuan-v0.2.19-1`（11-06 01:15），用时约 1.4h |
| fork 新增迁移占用 | 246、249、250×2、251×2、252、253 |
| 上游 v0.2.19 之后新增迁移 | `249_add_usage_logs_api_key_cost_index_notx`、`249_mcp_oauth`、`250_openai_cache_write_ttl` |
| 编号碰撞 | **249 三占、250 三占、251 双占**（全为不同列/不同表，语义不互斥） |
| 凭证加密路径 | fork `251_credential_drop_plaintext_columns` 已摘明文列；`253_fix_api_key_cache_invalidation_trigger` 触发器改按 `key_hash` |
| 上游本窗内容 | Fable/Mythos 5.2、GPT Image 2.6 原生链路、图片侧计费三修、请求体内存预算自适应、异步图片独立预留、SSE/EventStream 边界修复、若干 perf（认证快照读分组隐私门、RPM 覆盖查询消除） |
| 重叠的高风险文件 | `repository/api_key_repo.go`（fork 改了 368+/202−）、`repository/scheduler_cache.go`、`service/api_key_auth_cache_impl.go`（版本常数）、`server/routes/gateway.go`（车道挂载缝点、400 字符断言窗口）、`service/openai_gateway_usage.go`（rerank 计费分支） |

## Goals / Non-Goals

**Goals**

- 上游 v0.2.21 合流，fork 不变量零回退，出片上产一次滚成。
- 三处编号碰撞（249/250/251）按共存规则执行且可核验。
- 上一窗 3 笔挂账在本窗开窗前全部处置完毕（spec：挂账不得跨窗结转）。

**Non-Goals**

- 不 rebase 重写 fork 历史。
- 不在本窗内做注册表化 / 模型族配置化（那是 `ciyuan-lane-upstream-registry` 与
  `ciyuan-model-family-registry` 的事；本窗先换基线，否则新缝点会叠到未合并基线上）。
- 不消化上游 unreleased 内容（本窗终点固定为 tag `v0.2.21`）。

## Decisions

### D1 合并提交而非 rebase

延续上一窗 D1：173 个 fork 提交 rebase 会重写全部 SHA，快照仓提交信息、`fd-infra-deploy`
清单注释与运营手册的外部引用全断。merge 的历史可审计收益远大于线性历史的装饰收益。

**备选**：rebase → 否决；重建叠加（cherry-pick fork 提交到上游新基线）→ 否决（同样重写 SHA，
且 fork 提交间存在依赖链，重放易产生中间态编译失败）。

### D2 中途对照点 = v0.2.20，且只做冲突预演不做单独出片

本窗跨 v0.2.20/v0.2.21 两 tag。处置：以 `v0.2.20` 为中途对照点跑一次三方演练
（`git merge-tree --write-tree v0.2.20`）并记录合成树与冲突清单，然后一次性 merge `v0.2.21`。
**不为 v0.2.20 单独出片**——出片有整链成本（快照→GHA→relay→ccr→清单→ArgoCD），而 v0.2.20→v0.2.21
之间无安全修复需要提前落地。

**理由**：spec「跨多 tag 窗口必须显式声明基线与中途对照点」要的是**可归因**，不是多一次部署。
中途演练把「坏在哪个版本」的定位成本压到最低，而不引入第二笔出片成本。

### D3 迁移编号共存，且本窗补一条顺序敏感性核查

三处碰撞（249×3、250×3、251×2）全部保留原名，执行器按完整文件名记录版本、按字典序执行。
本窗新增一项**显式**核查（上一窗只核了「是否撞号」，没核跨组顺序）：

- `249_add_usage_logs_api_key_cost_index_notx` 在 `usage_logs` 上建索引，而 fork 的
  `253_fix_api_key_cache_invalidation_trigger` 改的是 `api_keys` 触发器 —— 不同表，顺序无关。
- `250_openai_cache_write_ttl` 加 `groups` 列，fork `250_api_key_capacity` 加 `api_keys` 列 —— 不同表。
- `249_mcp_oauth` 建新表 —— 与两侧无交叉。

结论：本窗三处碰撞**顺序无关**，但核对过程必须写进 tasks 留痕（下次碰撞不一定无关）。

### D4 认证快照版本常数：先探测碰撞再定处理

上一窗是 fork v25 与上游 v25 同号碰撞 → 合并体升 v26。本窗上游是否再升版本常数需在三方演练
后实测确认：

- 若上游仍为 v26 且无新字段 → 不改（当前 fork 已是 v26）。
- 若上游升到 v27（不同新字段）→ 按「双侧同号必再升一版」处理，合并体升 v28 并留双来源注释。
- 若上游未动但 fork 侧本窗有快照字段变更（本窗无）→ 不改。

**理由**：版本常数是快照形状的判据，猜错会导致线上同号旧快照被误判当前、缺失字段以零值参与
计费。这是上一窗唯一一个「文本合并成功但行为会坏」的坑，本窗先探测再动手，不预设结论。

### D5 凭证加密路径是本窗头号回归点，必须库副本实证

上游本窗动了 `repository/api_key_repo.go`（fork 侧同时改了 368+/202−，是重叠面里改动最大的文件）
与 `service/api_key_auth_cache_impl.go`。fork 的明文列已在 `251` 摘除、触发器按 `key_hash`。
因此本窗把「生产副本上走一遍读写路径」列为**出片前**的硬门（spec：凭证加密回归验证），
不接受单测绿灯替代。

**理由**：这条路径坏了的形态是静默的——读不到凭据会让全站 503，或更坏：读到错值。

### D6 缝点外冲突在本窗开窗前处置，而不是合并完再说

上一窗遗留 2 处缝点外冲突未登记。本窗开窗前逐条决定：

- `backend/internal/service/api_key_auth_cache_impl.go` → **补进缝点登记表 §二**。理由：它是
  快照形状的单一真源，每次上游窗口都可能碰，属于结构性缝点，不是一次性快照坑。
- `backend/internal/service/antigravity_gateway_service_test.go` → **明确归入 runbook §3 快照坑**。
  理由：它是测试文件，改它不改产行为；登记成缝点会让缝点表被测试噪音稀释。

## Risks / Trade-offs

- [凭证加密路径静默回归] 上游改 `api_key_repo.go` 与本仓派生列实现重叠 → D5 硬门：生产库副本实跑读写，结论入 tasks；未实跑不得勾账
- [快照版本常数漏升] 若上游升版而合并体未再升 → 任务 1.x 先探测上游常数，部署后验证旧快照被拒重建
- [车道挂载缝点被上游路由重排挤走] `routes/gateway.go` 有 400 字符源码级断言窗口 → 合流后跑 `gateway_model_allowlist_test.go`；车道挂载位置以断言为中心复核
- [rerank 计费分支漂移] 上游改 `openai_gateway_usage.go` 成本分支 → 单测 + 一次真实 rerank 请求的对账
- [跨两 tag 的语义合并量大] 中途对照点（D2）把定位成本压到最低；真冲突清单由三方演练产出后回填 tasks
- [窗再次延迟] 本窗起以「上游新 tag 出现即开窗、当周内完成」为口径（spec 已写），延迟即记入台账

## Migration Plan

1. 开窗前：处置上一窗 3 笔挂账（D6 + 终验项补勾 + 窗延迟定性）
2. `git fetch upstream --tags`；三方演练 v0.2.20（中途对照）与 v0.2.21（终点），冲突清单入 tasks
3. 建合流分支 → `git merge v0.2.21` → 解冲突 → `go generate ./ent` 对账 → 全量单测
4. 生产库副本上做凭证加密回归（D5）
5. `ciyuan-tag` 与 git tag 同步为 `ciyuan-v0.2.22-1` → 快照仓 → GHA → hkccr→relay→ccr →
   `fd-infra-deploy` 改 tag+注释 → ArgoCD 手动 sync（selfHeal=off）
6. 部署后三核：迁移执行结果、健康探针与 `upstream-queue-metrics`、鉴权快照按新版本重建

**回滚**：`fd-infra-deploy` 镜像 tag 退回当前生产 tag，ArgoCD 手动 sync 即回旧版（旧镜像仍在 ccr）。
本窗新增迁移均为加列/加索引/建表，对旧版无破坏，无需迁移回滚。

## Open Questions

无。上游 v0.2.21 之后的新 tag 若在出片前出现，按 spec「挂账不得跨窗结转」与「新 tag 出现即开窗」
另立下一窗，不在本窗范围内追。
