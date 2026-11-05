# Tasks: merge-upstream-v0-2-19

## 1. 合流执行

- [x] 1.1 `git fetch upstream --tags` 后在 fork main 建合流分支，执行 `git merge v0.2.19`；三方演练已知真冲突 3 文件：`backend/internal/service/api_key_auth_cache_impl.go`、`frontend/src/components/account/EditAccountModal.vue`、`backend/internal/service/antigravity_gateway_service_test.go`
- [x] 1.2 解决 `api_key_auth_cache_impl.go`：快照版本常数升 v26，注释同时写明 fork 侧 `rerank_price_per_request`（原 v25）与上游侧 `image_count_billing`（原 v25）两字段来源（spec：认证快照版本号碰撞）
- [x] 1.3 解决 `EditAccountModal.vue`：fork 车道/provider 钉扎字段与上游按张计费开关字段并存；`EditAccountModal.spec.ts` 同步双字段断言
- [x] 1.4 解决 `antigravity_gateway_service_test.go`：两侧用例并存，fork 侧断言语义不弱化
- [x] 1.5 迁移文件不做任何重命名：确认 fork `246_group_rerank_price_per_request.sql` 与上游 `246_enable_gemini_image_generation_groups.sql`/`247_scoped_admin_api_keys.sql`/`248_group_image_count_billing.sql` 同树共存，跑迁移测试套 + checksum 校验（spec：迁移编号共存）
- [x] 1.6 `go generate ./ent`（backend）对账 ent 生成代码与双侧 group schema 变更，diff 应与合并树一致或为空
- [x] 1.7 调度投影回归：补/保留单测断言 `filterSchedulerCredentials` 白名单含 `openai_capabilities`（上游 scheduler memo 重写回归防线，spec：fork 不变量）
- [x] 1.8 AIMD 合流对账：上游 `openai_gateway_service` 错误路径变更后，确认类型化 Reason 分流点仍在，`repository` 7 件 + service 全量单测绿；`upstream-queue-metrics` 端点契约不变

## 2. 构建与 CI

- [x] 2.1 go.mod 合流后为 1.27.3：本地全量 `go test -tags=unit ./...` 绿；重点盯 `billing_inflight_reservation_test` 内存上界在 1.27.3 分配器下的数值
- [x] 2.2 images 仓 workflow Go 钉版决策：与上游 1.27.3 对齐并更新钉版注释（旧注释前提「钉 1.27.0」已被 go.mod 1.27.2+ 失效化），必要时同步 inflight 边界
- [x] 2.3 frontend build + vitest 全绿（含 fork 前端测试 16 件 + 上游新增用例）

## 3. 出片与部署

- [x] 3.1 bump 根目录 `ciyuan-tag` 文件至 `ciyuan-v0.2.19-1` 并打同名 git tag（治 `-16`/`-17` 漂移；spec：语义 tag 一致）
- [x] 3.2 快照仓 law-ai-official/sub2api-images 提交合流快照 → GHA unit+build → hkccr→relay→ccr 镜像就绪核对
- [x] 3.3 fd-infra-deploy `all-services/sub2api/01-sub2api-deployment.yaml` 镜像 tag 与注释更新（注释写明合流版本与快照 sha）
- [x] 3.4 ArgoCD 首滚手动 sync（selfHeal=off）：迁移 246_enable/247/248 执行结果核对 + 健康探针 + `upstream-queue-metrics` 哨兵面 200
- [x] 3.5 部署后鉴权缓存重建验证：v26 升版生效（旧快照拒绝重建）、rerank 价与按张计费字段非 nil（spec：双侧同升 v25 的合流）

## 4. 运营跟进（配置面）

- [x] 4.1 GPT-6.1 Terra：生产分组未设模型限制（core/image-video restrict=false），模型自动可用；价目随 model_pricing.json 266 模型载入——无须后台配置
- [x] 4.2 生产映射核对三项全无影响：gpt-5.5 仅账号 46（API Key 账号不受退役影响）映射且 30 天零调用；gemini-2.x 零引用；grok 零账号
- [x] 4.3 scoped admin API key：迁移 247 表已建（scoped_admin_api_keys=0 行）；发 key 待巡检自动化消费方（ops-mcp 对账面）接入时按「操作+资源+过期+IP 白名单」发放——发 key 本身是运营动作，表能力已就绪
- [x] 4.4 生图开关核对：生产无 gemini/antigravity 分组；composite 双组（dev-sandbox/paas-legacy-b）生图=关且 30 天零出图调用，v0.2.18 收紧无影响；image-video（openai）本就开
- [x] 4.5 per-image billing 全组维持默认关（image_count_billing=false ×10 组），挂账评估单随本 change 归档备注

## 5. 收口

- [x] 5.1 结清 `codex-manifest-pinned-accounts` 6.4：集群一次性栈等价验证通过（并集 [a,b]+TTL 窗吸收实证），详见其 tasks.md
- [x] 5.2 v0.2.20 前瞻挂账：上游 unreleased `bound gateway resources`（457602c8f）与 fork AIMD 语义同族，下次同步须对账合流（记入本仓 backlog/README 备忘）
- [ ] 5.3 全量回归：conformance 相关（若有）+ 本 change 三份 spec scenario 逐条核验后 `openspec archive merge-upstream-v0-2-19`
