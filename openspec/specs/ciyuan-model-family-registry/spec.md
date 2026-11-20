# ciyuan-model-family-registry Specification

## Purpose
定义「哪些模型名被本部署承认为某类模态」的可配置契约：图片与 embedding 的准入族表由配置决定而非
代码常量，默认值与改造前逐字节等价，且图片的三个判定点取自同一份配置，不允许出现半开状态。

## Requirements

### Requirement: 图片模型准入族表可配置且默认等价

图片端点对模型名的准入判定 MUST 依据一份可配置的模型族表（族 = 名称匹配规则），
MUST NOT 只依据代码中写死的前缀谓词。未配置时，族表 MUST 等价于改造前的行为：
`gpt-image-*`、`grok-imagine*`、`gemini-*-image*` 三类被认同，其余被拒。

#### Scenario: 未配置时既有三族行为逐字节等价

- **WHEN** 部署未修改族表配置
- **THEN** 三个既有族的模型名 SHALL 与改造前一样通过准入
- **THEN** 不属于任何族的模型名 SHALL 与改造前一样被拒（400）
- **THEN** 存在测试断言该等价性（族集合 + 拒绝文案不变）

#### Scenario: 登记新族后该族模型能穿过准入

- **WHEN** 管理员在族表中登记一个新的图片模型名前缀族
- **THEN** 该族的模型名 SHALL 通过网关准入并触达上游
- **THEN** 该请求最终成功与否 SHALL 由上游决定，网关 SHALL NOT 在准入阶段替上游拒绝

#### Scenario: 移除某族后该族模型被拒

- **WHEN** 管理员从族表中移除一个此前存在的族
- **THEN** 该族模型名 SHALL 在触达上游前被拒
- **THEN** 该拒绝 SHALL 与「不属于任何族」的拒绝形状一致

### Requirement: 图片三处判定必须同源

图片族的判定 MUST 在请求解析、渠道映射后的复核、图片意图识别三处取自**同一份**配置；
MUST NOT 出现「端点放行但意图识别未放行」或反之的半开状态。

#### Scenario: 新族在三个判定点一致生效

- **WHEN** 某族被登记为图片族并发起一次该族模型的请求
- **THEN** 请求解析、映射复核、意图识别三处 SHALL 一致地判定该模型为图片模型
- **THEN** SHALL NOT 出现任一处判定为「非图片」的情况

#### Scenario: 三处判定不一致即视为缺陷

- **WHEN** 三处判定对同一模型名给出不同结论
- **THEN** 该情形 SHALL 被测试捕获（存在覆盖三处同源的断言）
- **THEN** 不得以「三处各自有测试」代替同源断言

### Requirement: embedding 模型目录可配置且默认等价

embedding 的「本部署已登记模型」目录 MUST 由配置决定，MUST NOT 是代码中的静态切片。
未配置时 MUST 等价于改造前的四条登记（Qwen3-Embedding-8B 与三个 OpenAI text-embedding 名）。

#### Scenario: 未配置时既有四条等价

- **WHEN** 部署未修改 embedding 目录配置
- **THEN** 改造前被识别为 embedding 的四个模型名 SHALL 仍被识别
- **THEN** 改造前不被识别的模型名 SHALL 仍不被识别

#### Scenario: 新登记模型在后台可见可配

- **WHEN** 管理员在目录中登记一个新的 embedding 模型名
- **THEN** 该模型 SHALL 出现在后台的账号可用模型与分组白名单候选中
- **THEN** 该模型的 provider 钉扎配置 SHALL 可被设置并生效

### Requirement: 放宽准入后错误归属必须可辨识

当模型名因新登记的族穿过网关准入而在上游失败时，响应 MUST 保留上游失败的真实归属
（状态码、错误类型），MUST NOT 被网关改写为「模型不存在」或「非图片模型」这类本应属于
准入层的错误。

#### Scenario: 新族模型的真实上游失败透出

- **WHEN** 一个由新登记族放行的图片模型在上游返回 4xx/5xx
- **THEN** 客户端收到的 SHALL 是反映上游失败的响应
- **THEN** SHALL NOT 收到「不属于任何图片族」这类准入层错误

#### Scenario: 错配族表时错误可归因

- **WHEN** 管理员登记了一个上游并不存在的模型名族并发起请求
- **THEN** 该笔失败 SHALL 在运维观测面可区分于「准入拒绝」
- **THEN** 该区分 SHALL NOT 依赖人工比对日志

### Requirement: 视频扩展仅限 OpenAI 兼容形状

本次放宽 MUST 只覆盖 OpenAI 兼容形状的视频端点；非 OpenAI 形状的厂商原生异步任务协议
MUST NOT 在本能力范围内被隐式支持。

#### Scenario: OpenAI 兼容视频端点可用

- **WHEN** 某平台登记为具有 OpenAI 兼容视频端点
- **THEN** 该平台账号 SHALL 能承接视频生成请求
- **THEN** 任务状态与内容取回 SHALL 走既有形状

#### Scenario: 原生协议不被隐式接入

- **WHEN** 某厂商只提供非 OpenAI 形状的原生异步任务协议
- **THEN** 该厂商 SHALL NOT 因本能力自动可用
- **THEN** 接入它 MUST 走独立的能力变更
