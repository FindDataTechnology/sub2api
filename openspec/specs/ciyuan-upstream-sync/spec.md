# ciyuan-upstream-sync Specification

## Purpose
规定词元 fork 消纳上游 sub2api 版本的合流纪律：fork 不变量在上游合流后必须保全、快照版本号与迁移编号
碰撞时的处理规则、出片滚版链的可追溯性，以及把「定期跟上游」从人工记忆升级为可失败检查（窗口触发、
缝点集合不扩大、归档前 tasks 勾账）。

## Requirements

### Requirement: fork 不变量在上游合流后必须保全

每次把上游 Wei-Shaw/sub2api 版本合入 ciyuan fork 主线后，以下 fork 自有行为 MUST 保持且各有测试或探针证明：三车道（rerank/audio/websearch）端点与统一错误面；车道池多账号轮转与失败分级冷却；per-account 有界排队与 AIMD 容量状态机（含 `ErrQueueFull→429 Retry-After` 终止性 failover 与观测面哨兵字段）；embedding 模型目录后台可见可配与账号级 provider 钉扎；OIDC 多宿主回调派生；调度投影凭据白名单保留 `openai_capabilities`（Seedance 能力账号不得被候选阶段恒久排除）。

#### Scenario: 上游重写调度快照缓存后投影白名单保全

- **WHEN** 上游对 `scheduler_cache` 投影逻辑做了进程内缓存或字段白名单改动并合入 fork 主线
- **THEN** `openai_capabilities` 键仍在投影凭据白名单中，配置了该能力的账号在候选阶段可见
- **THEN** 存在单测断言投影结果包含该键（上游重写回归防线）

#### Scenario: AIMD 喂点在上游网关服务变更后仍分流

- **WHEN** 上游改动了 openai 网关服务的错误处理路径并合入
- **THEN** fork 的有界排队快拒结果与真上游限流 429 在 AIMD 记账处分流（类型化 Reason），`ErrQueueFull` 终止性 failover 行为不回退
- **THEN** `upstream-queue-metrics` 观测端点仍返回 `queue_depth`/`queue_rejected_total`/`aimd_concurrency`/`aimd_state`

### Requirement: 认证快照版本号碰撞时合并体必须再升一版

当 fork 与上游在同一上游基线之后各自把认证快照版本常数升到同一数值（各自为不同新字段），合流后的快照形状包含双方新增字段，其版本常数 MUST 再升一版并留双注释说明两侧来源；否则线上现存同号快照会被误判为当前版本、缺失字段以零值参与计费。

#### Scenario: 双侧同升 v25 的合流

- **WHEN** fork v25（`rerank_price_per_request`）与上游 v25（`image_count_billing`）合流
- **THEN** 合并体快照版本为 v26，注释同时说明 rerank 价与按张计费两字段来源
- **THEN** 部署后首次鉴权缓存重建前，旧 v25 快照因版本不符被拒绝并重建，按张计费分组计费字段非 nil

### Requirement: 迁移编号冲突按文件名共存执行

fork 与上游的 SQL 迁移编号发生占用碰撞时，MUST NOT 重命名任一侧已在线上应用过的迁移文件；迁移执行器按完整文件名记录版本、按文件名字典序执行，两侧同号不同名迁移 MUST 共存执行，顺序不影响结果时无须人工干预，顺序敏感时在合流 change 中显式记录用例。

#### Scenario: fork 246 与上游 246 同号合流

- **WHEN** fork `246_group_rerank_price_per_request`（已应用）与上游 `246_enable_gemini_image_generation_groups`（新）共存于合流后代码树
- **THEN** 两个文件都保留原名，执行器按字典序先后执行且各自只执行一次
- **THEN** 迁移测试套与 checksum 校验通过，重复部署不重复执行任一迁移

### Requirement: 上游合流走合并提交并保持出片链可追溯

上游版本合入 MUST 使用合并提交（MUST NOT rebase fork 自有提交），保留 fork 提交 SHA 的外部引用有效性（快照仓提交信息、部署清单注释、运营手册）；合流完成后按出片链滚动：fork main 打 `ciyuan-vX.Y.Z-N` tag 且根目录 `ciyuan-tag` 文件与该 tag 一致 → 快照仓提交 → GHA 单测+构建 → 镜像仓 tag → fd-infra-deploy 清单更新 → ArgoCD 首滚手动 sync。

#### Scenario: 语义 tag 与实际内容一致

- **WHEN** 任一次出片（快照仓提交）发生
- **THEN** `ciyuan-tag` 文件内容与 fork 当前 git tag 及镜像注释三者一致，不存在内容已前进而语义 tag 未动的漂移

#### Scenario: 合流后的部署首滚

- **WHEN** 合流镜像已在 ccr 就绪并更新 fd-infra-deploy 清单
- **THEN** 首次滚动经 ArgoCD 手动 sync 执行（该应用 selfHeal=off），滚动后健康探针与迁移执行结果核对通过

### Requirement: fork 改造必须同批对齐受影响的测试基线

任何改变数据库 schema、凭据读写路径或事务边界的 fork 改造 MUST 在同一批变更内
跑一次集成测试（`go vet -tags=integration ./...` + 受影响的集成用例），
MUST NOT 留到「下次跑 CI 时」才发现测试基线不兼容。

#### Scenario: 删列类改造在同批内验证

- **WHEN** 一个变更删除或重命名了生产列（如凭据落盘加密删掉明文列）
- **THEN** 该变更 MUST 同批更新所有引用该列的测试
- **THEN** MUST 在合入前跑通集成测试，而不是等到 CI 首跑

#### Scenario: 未对齐的改造不得归档

- **WHEN** 一个改 schema 的 change 归档时，集成测试仍因该改动失败
- **THEN** 该 change MUST NOT 归档，并记入台账作为挂账

### Requirement: 测试不得以跳过代替修复

因改造导致失配的测试 MUST 按新契约重设断言。
仅当该测试所验证的路径在新契约下**已不存在**时，才允许删除（并说明被谁取代）。
`t.Skip` MUST NOT 作为长期状态存在。

#### Scenario: 临时跳过必须带指向修复 change 的说明

- **WHEN** 一个测试因改造失配被暂时跳过
- **THEN** 跳过处 MUST 注明原因与承接的 change 名
- **THEN** 该 change MUST 在合理周期内关闭跳过（否则等同于门禁失效）

#### Scenario: 新断言必须含负向用例

- **WHEN** 一个被跳过的测试按新契约重设断言后恢复
- **THEN** 该测试 MUST 至少有一条负向断言（该失败时确实失败）
- **THEN** 不得只有「happy path 通过」的正向断言
