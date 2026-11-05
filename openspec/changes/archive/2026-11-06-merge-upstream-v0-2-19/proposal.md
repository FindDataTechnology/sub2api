## Why

fork（ciyuan 线）基于上游 v0.2.16 + 45 自有提交，已落后 3 个小版本 / 164 commits（v0.2.17–v0.2.19）。上游这两周修掉了与我们 AIMD 同族的稳定性病灶（模型列表并发冷启动重复打上游、流断连计费记 0、国产 429 冷却粒度），完成了调度快照进程内缓存大重写（多账号池调度延迟实降），并带安全依赖修复（GO-2026-6860、dompurify 双 GHSA）。漂移越久，后续合并成本与语义冲突风险越高；`git merge-tree` 三方演练证实本次合并便宜（真文本冲突仅 3 文件），现在动手。

## What Changes

- merge 上游 v0.2.19 into main（合并提交，不走 rebase，保 fork 审计链与快照仓 SHA 引用）
- 解决 3 处文本冲突：`api_key_auth_cache_impl.go`（快照版本常量）、`EditAccountModal.vue`、`antigravity_gateway_service_test.go`
- **BREAKING**（继承上游）：auth 快照版本 v25→v26——上游 v25=加 `image_count_billing`、fork v25=加 `rerank_price_per_request`，合并体双字段并存必须再升版，否则线上 v25 快照（仅 rerank 价）被误判当前、按张计费字段读 nil
- 迁移编号 246 双占共存：fork `246_group_rerank_price_per_request`（已应用）与上游 `246_enable_gemini_image_generation_groups` / `247_scoped_admin_api_keys` / `248_group_image_count_billing` 按文件名字典序共存执行（执行器版本键=完整文件名，TEXT PK 不撞）
- Go toolchain 对齐：上游 go.mod 升 1.27.3，images CI 钉版 1.27.0 需实测 inflight 内存上界测试后对齐（上游已统一 patch 版检查）
- `ciyuan-tag` 文件漂移治本：现停 `ciyuan-v0.2.16-16` 而线上内容已是 `-17`；本次升 `ciyuan-v0.2.19-1` 并核对与 git tag 一致
- ent 代码再生成校验（group schema 双侧变更自动合并后必须 `go generate` 对账）
- 运营跟进口（配置面，非代码）：GPT-6.1 Terra 入分组白名单/价目；gpt-5.5 订阅退役与 Gemini 默认模型 2.x 剔除的生产映射核对；scoped admin API key 为巡检自动化发受限 key；per-image billing 评估（默认关，本版不动）
- 结清挂账 change `codex-manifest-pinned-accounts` 6.4 手动验证
- v0.2.20 前瞻挂账：上游 unreleased `bound gateway resources`（457602c8f）与 fork AIMD 语义同族，下次同步须对账合流

## Capabilities

### New Capabilities
- `ciyuan-upstream-sync`: ciyuan fork 消纳上游版本的合流纪律——fork 不变量清单（三车道/车道池/AIMD/embedding 面板/OIDC 多宿主/openai_capabilities 投影白名单）、快照版本号碰撞规则（双侧同升版本号时合并体必须再升一版）、迁移编号共存规则、出片滚版链（快照仓→GHA→ccr→fd-infra-deploy→ArgoCD 手动 sync）与 ciyuan-tag 一致性要求

### Modified Capabilities

## Impact

- 代码：backend（scheduler/auth cache/billing/gateway 约 60 个重叠文件，3 处真冲突）、frontend（账号弹窗等 4 文件）
- 数据库：新增迁移 246_enable/247/248（启动自动执行；`scoped_admin_api_keys` 新表、`groups.image_count_billing` 新列）
- 出片链：images 仓快照提交→GHA unit+build→hkccr→relay→ccr→fd-infra-deploy 改 tag→ArgoCD（selfHeal=off，首次手动 sync）
- 运营面：分组白名单/价目配置、账号映射核对（gpt-5.5、Gemini 2.x、grok-4.7-fast 调价 2x）
- 风险：调度快照 memo 路径需回归验证 openai_capabilities 投影修复不回退；AIMD 喂点在上游 openai_gateway_service 变更后的 Reason 分流需全量测试对账
