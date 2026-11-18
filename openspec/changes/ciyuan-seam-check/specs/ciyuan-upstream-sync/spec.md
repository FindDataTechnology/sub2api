## ADDED Requirements

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
