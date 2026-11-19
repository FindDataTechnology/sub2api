## ADDED Requirements

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
