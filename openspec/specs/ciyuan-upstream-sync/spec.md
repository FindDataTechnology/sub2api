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

### Requirement: 缝点集合不得在无人察觉的情况下扩大

fork 相对上游合并基线的改动文件集合中，任何**上游文件**（上游基线已存在的文件）MUST 要么属于
已登记的缝点白名单，要么该检查失败。白名单 MUST 以机器可读形式维护，并 MUST 能与人工台账
（`paas/docs/sub2api-fork-patches/README.md` §二）逐条对照。

#### Scenario: 修改白名单外的上游文件导致检查失败

- **WHEN** fork 修改了一个上游基线中存在、但不在缝点白名单中的文件
- **THEN** 缝点检查 SHALL 失败并列出该文件
- **THEN** 失败信息 SHALL 指出「要么回退该改动，要么显式登记为新缝点」

#### Scenario: 新增文件不算缝点

- **WHEN** fork 新增了一个上游基线中不存在的文件
- **THEN** 该文件 SHALL NOT 触发缝点检查失败
- **THEN** 它 SHOULD 被单独盘点（新增文件数不进入缝点判据）

#### Scenario: 白名单扩大必须显式提交

- **WHEN** 需要修改一个新的上游文件
- **THEN** 该文件的登记 MUST 作为独立可见的变更提交（白名单文件的 diff）
- **THEN** 该变更 MUST 同时在人工台账 §二 有对应行

### Requirement: 白名单与人工台账必须可核对

机器白名单与人工台账 §二 MUST 可互相核对：检查 SHOULD 校验台账中出现的路径都存在于白名单，
且白名单中的路径都能在台账找到说明。两者出现漂移时检查 SHOULD 报告，而不是静默以白名单为准。

#### Scenario: 只改台账不改白名单

- **WHEN** 有人在台账 §二 增加一行缝点，但没有更新机器白名单
- **THEN** 核对 SHALL 报告「台账有、白名单无」的差异
- **THEN** 该差异 SHALL 可被人读到（而非只记一条 warn 日志后放过）

#### Scenario: 只改白名单不改台账

- **WHEN** 有人扩大机器白名单但台账未同步
- **THEN** 核对 SHALL 同样报告相反方向的差异

### Requirement: 合并窗触发与归档前置条件必须机械化

上游出现新 tag（或上游变更说明含安全修复字样）MUST 产生一条可追踪的开窗提示，MUST NOT 仅依赖
人工记忆。合并 change 归档前 MUST 通过 tasks 勾账核查：所有任务项已勾选或标注明确跳过理由。

#### Scenario: 上游新 tag 出现即产生开窗提示

- **WHEN** 上游仓库出现晚于 fork 已消纳基线的新 tag
- **THEN** 系统 SHALL 产生一条开窗提示（含 tag 名与日期）
- **THEN** 窗口延迟（tag 日期 → 合流提交日期）SHALL 可被测得

#### Scenario: 有未勾任务时归档被拒

- **WHEN** 一个合并类 change 仍存在未勾选且无跳过理由的任务项
- **THEN** 归档动作 SHALL 被拒绝并列出未勾项
- **THEN** 不得以「先归档后补」的方式绕过

### Requirement: 跨多 tag 窗口必须显式声明基线与中途对照点

当一次上游合并窗跨越**两个或以上上游 tag**（例如同时消纳 v0.2.20 与 v0.2.21）时，合流
change MUST 在 proposal/design 中显式声明：①目标上游 tag（窗口终点）；②窗口内各中途 tag
（用于逐段对照，避免「一次合并两个版本、坏了不知道哪一版引入」）；③每个中途 tag 的可核验
证据（合流树、冲突清单或回归结果）。窗口 MUST NOT 以「反正一起合了」为由省略中途对照。

#### Scenario: 跨两 tag 的窗口登记中途对照点

- **WHEN** 一次合流窗的目标为 v0.2.21 且包含 v0.2.20 的全部提交
- **THEN** change 文档列出 `v0.2.20` 作为中途对照点
- **THEN** 该对照点附可核验证据（例如以 v0.2.20 为基线的合成树/冲突清单/关键回归结果）

#### Scenario: 单 tag 窗口不受此要求约束

- **WHEN** 一次合流窗只消纳一个上游 tag
- **THEN** 本要求不适用，无需登记中途对照点

### Requirement: 合并窗挂账不得跨窗结转

每窗遗留的挂账项（缝点外冲突未登记、终验项未勾、窗延迟未定性等）MUST 在下一次合并窗
**开始前**逐条处置并留结论（关闭 / 明确归入具体规则 / 单独立项）；MUST NOT 以「随下窗
一起看」的形式继续结转。每条挂账的处置结论 MUST 可被第三方在台账中核验（有结论、有
位置、有判据）。

#### Scenario: 上一窗的缝点外冲突挂账在开窗前被处置

- **WHEN** 上一窗遗留「缝点外冲突若干处未补登记」的挂账
- **THEN** 本窗开窗前逐条决定「补进缝点登记表」或「明确归入 runbook 快照坑」
- **THEN** 结论写入台账对应小节，注明判据与位置，不留二选一的模糊表述

#### Scenario: 未处置的挂账阻断开窗

- **WHEN** 上一窗存在未处置挂账
- **THEN** 本窗 MUST NOT 以「已开始合并」状态推进到出片阶段
- **THEN** 该阻断事实记入 change 文档，供审计时区分「违规」与「挂账已清」

### Requirement: 凭证落盘加密路径在上游合流后必须回归验证

当上游变更触及账号/API key 仓储、凭据读写或认证快照投影时，合流后 MUST 在**生产副本**
上验证 fork 的凭证落盘加密路径（派生列读写、明文回落已摘除、触发器按派生键失效）仍然成立，
且 MUST 用真实备份恢复的库验证，不得仅以单测绿灯代替。

#### Scenario: 上游改动账号仓储后的加密回归

- **WHEN** 上游对账号仓储/凭据字段的读写实现做了变更并合入 fork 主线
- **THEN** 合流后在真实生产库副本上走一遍读路径与写路径
- **THEN** 验证结论（可解、无明文列依赖、触发器按派生键生效）记入 tasks 与台账

#### Scenario: 仅单测通过不视为该验证完成

- **WHEN** 该 change 的单测与集成测试全绿但未在库副本上实跑
- **THEN** 该项 MUST 标记为未完成，不得勾账
