# Tasks: merge-upstream-v0-2-21

## 0. 开窗前清挂账（阻塞：未完成不得进入第 1 组）

- [ ] 0.1 处置上一窗挂账「缝点外冲突 2 处未补登记」：`api_key_auth_cache_impl.go` 补进台账 `paas/docs/sub2api-fork-patches/README.md` §二（判据：快照形状单一真源、每次窗口都可能碰）；`antigravity_gateway_service_test.go` 明确归入 runbook §3 快照坑。验证：台账 diff 中两条各有结论、位置、判据，无二选一表述
- [ ] 0.2 处置上一窗挂账「`merge-upstream-v0-2-19` 5.3 终验未勾即归档」：在归档目录 tasks.md 补勾或注明跳过理由，并把「归档前先核 tasks」写进本窗第 6 组收口项。验证：`openspec/changes/archive/2026-11-06-merge-upstream-v0-2-19/tasks.md` 5.3 状态与实际一致
- [ ] 0.3 处置上一窗挂账「窗延迟 8 天未定性」：以「上游 tag 日期 → 合流提交日期」同一口径给出本窗数据（tag v0.2.21 日期 vs 本窗合流提交日期），并在台账 §三 摘要中定性（触发晚 / 工作量大）。验证：台账 §三 有带日期的定性结论

## 1. 合流预演与冲突清单

- [ ] 1.1 `git fetch upstream --tags` 后以 `v0.2.20` 为中途对照点跑 `git merge-tree --write-tree upstream/v0.2.20 HEAD`，记录合成树 sha 与冲突文件清单，写入本文件或 design.md 回填区（spec：跨多 tag 窗口登记中途对照点）
- [ ] 1.2 以 `v0.2.21` 为终点再跑一次 `git merge-tree --write-tree`，产出真冲突清单；与 1.1 对比，标出「v0.2.20 之后新引入的冲突」。验证：两轮冲突清单并列可读，差异项各自标注来源 tag
- [ ] 1.3 探测上游认证快照版本常数（`backend/internal/service/api_key_auth_cache_impl.go`）与其新增字段；按 design D4 判定本窗是否需要再升一版。验证：结论 + 依据（上游常数数值、新增字段名）写入 tasks 勾账说明
- [ ] 1.4 核对三处迁移编号碰撞（249×3、250×3、251×2）的跨组顺序敏感性，逐条写明为何顺序无关（表/列不交叉）。验证：design D3 的三条结论各有实测依据，写进台账本窗备注

## 2. 合流执行

- [ ] 2.1 在 fork 建合流分支，`git merge upstream/v0.2.21`（合并提交，不 rebase）
- [ ] 2.2 逐条解决 1.2 列出的真冲突；每条冲突的解法在 commit message 或 tasks 说明中记一句「为何这样合」。验证：冲突文件全部解决，`git status` 干净
- [ ] 2.3 按 1.3 的判定处理认证快照版本常数（不改 / 再升一版）并留双来源注释。验证：版本常数与注释同时可读，注释写明两侧字段来源
- [ ] 2.4 确认三处迁移碰撞文件全部保留原名共存，跑迁移测试套与 checksum 校验。验证：测试套绿、无迁移被重命名
- [ ] 2.5 `go generate ./ent` 对账：生成代码 diff 必须与合并树一致或为空。验证：`git diff` 中 ent 目录无未预期变化
- [ ] 2.6 fork 不变量逐条核验（三车道端点与统一错误面、车道池轮转与冷却、AIMD 与 `ErrQueueFull→429`、`upstream-queue-metrics` 字段、embedding 目录与 provider 钉扎、维度漂移守卫、OIDC 多宿主、`openai_capabilities` 投影白名单、凭证派生列）。验证：对应单测/断言文件存在且绿；任一项缺失即红
- [ ] 2.7 复核车道挂载缝点（`server/routes/gateway.go`）在上游路由重排后的位置，跑 `gateway_model_allowlist_test.go` 的 400 字符断言窗口测试。验证：该测试绿，车道四端点仍挂在组门之前

## 3. 回归验证（出片前硬门）

- [ ] 3.1 本地全量 `go test -tags=unit ./...` 绿；重点盯 inflight 内存上界测试在新依赖版本下的数值
- [ ] 3.2 frontend `check:i18n` + `vue-tsc` + `vitest` 全绿（含 fork 新增用例与上游新增用例）
- [ ] 3.3 **生产库副本上做凭证加密回归**：从真实备份恢复库，验证凭据可解、明文列无依赖、缓存失效触发器按 `key_hash` 生效（spec：凭证落盘加密路径在上游合流后必须回归验证）。验证：实跑结论写入 tasks；仅单测绿不算完成
- [ ] 3.4 rerank 计费分支对账：`openai_gateway_usage.go` 的上游变更后，跑一次真实 rerank 请求并核对用量行金额。验证：金额与分组价目一致
- [ ] 3.5 图片侧计费三修复核：上游新增的承做方留痕/交付档接数据/视频完成态形状计费三条与 fork 现有计费路径不冲突。验证：相关单测绿 + 一条真实图片请求的用量行字段完整

## 4. 出片与部署

- [ ] 4.1 bump 根目录 `ciyuan-tag` 至 `ciyuan-v0.2.22-1` 并打同名 git tag。验证：`git tag --points-at HEAD` 与文件内容一致
- [ ] 4.2 快照仓 `law-ai-official/sub2api-images` 提交合流快照 → GHA unit+build → hkccr→relay→ccr 镜像就绪。验证：CI 双绿 + skopeo 核对 hkccr/ccr digest 一致
- [ ] 4.3 `fd-infra-deploy` `all-services/sub2api/` 镜像 tag 与注释更新（注释写明合流版本与快照 sha）
- [ ] 4.4 ArgoCD 手动 sync 首滚（selfHeal=off）。验证：rollout 完成，pod Running 且重启计数 0

## 5. 部署后验证

- [ ] 5.1 迁移执行结果核对：本窗新增/共存的迁移全部执行且各执行一次。验证：库内 `schema_migrations` 版本行与本窗文件清单一致
- [ ] 5.2 健康探针与 `upstream-queue-metrics` 200，四个哨兵字段（`queue_depth`/`queue_rejected_total`/`aimd_concurrency`/`aimd_state`）存在
- [ ] 5.3 鉴权快照按新版本重建生效：旧版本快照被拒，rerank 价与按张计费字段非 nil（spec：快照版本号碰撞）。验证：按版本不符拒绝的证据 + 字段非 nil 的查询结果
- [ ] 5.4 车道真回执：rerank / search / audio 各一次真实成功请求（audio 含 ASR 与 TTS）。验证：响应 200 且用量行入账
- [ ] 5.5 运营面核对（配置，非代码）：Fable/Mythos 5.2 的价目与白名单、GPT Image 2.6 模型可用性。验证：后台可见 + 一次真实调用成功或明确记录不支持原因

## 6. 收口

- [ ] 6.1 台账 `paas/docs/sub2api-fork-patches/README.md` §一 追加本窗发布记录行（基线、内容、验收），§三 追加本窗 merge 用时（同口径：合流提交时间 → 产出 tag 时间）
- [ ] 6.2 归档前核 tasks：确认 0.x–5.x 全部勾账或有明确跳过理由（上一窗 5.3 教训）
- [ ] 6.3 三份 spec scenario 逐条核验后 `openspec archive merge-upstream-v0-2-21`
