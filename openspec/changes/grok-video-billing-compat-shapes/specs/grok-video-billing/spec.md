## Purpose

规定 video 异步任务（`/v1/videos` 协议族）的计费判据：什么形状算「可计费的完成」、
计量单位从哪些字段取、以及计费身份用请求模型还是上游承做模型。存在的意义是把
「上游形状与官方文档不一致 ⇒ 静默零计费」这一类故障变成一条可断言、可复验的规则。

## ADDED Requirements

### Requirement: 视频完成态的词表容错

计费判据 SHALL 把 `status` 为 `done`、`completed` 或 `succeeded`（大小写不敏感）视为成功终态；
`pending`、`in_progress`、`queued`、`failed`、`expired`、`cancelled` 以及任何未知值
MUST NOT 被视为可计费。词表比较 MUST 大小写不敏感，且 MUST 以集合而非单值断言实现，
使新增同义终态时只改一处常量。

#### Scenario: 官方形状

- **WHEN** status body 为 `{"status":"done","video":{"url":"https://vidgen.x.ai/x.mp4","duration":8}}`
- **THEN** 该任务 SHALL 被判为可计费

#### Scenario: relay 兼容形状

- **WHEN** status body 为 `{"status":"completed","model":"kling-video-v3","url":"/v1/videos/vid_x/content","seconds":"3"}`
- **THEN** 该任务 SHALL 被判为可计费

#### Scenario: 非终态与失败态不计费

- **WHEN** status body 的 `status` 为 `pending` / `in_progress` / `queued` / `failed` / `expired` / `cancelled`
- **THEN** MUST NOT 产生任何计费单位，即便 body 里带 `url`

### Requirement: 计费要求终态且有取件地址

判据 MUST 同时要求「成功终态」与「存在非空取件地址」：取件地址来源 SHALL 为
`video.url`（官方）或扁平 `url`（兼容/relay），按此顺序取第一个非空值。
只有终态而无取件地址 MUST NOT 计费（说明上游未真正产出）；只有取件地址而非终态
MUST NOT 计费（占位 URL 不代表产物可用）。

#### Scenario: 终态但无取件地址

- **WHEN** status body 为 `{"status":"completed"}`（无 url、无 video.url）
- **THEN** MUST NOT 计费，且系统 SHALL 保持等待后续轮询而非报错终止

#### Scenario: 非终态但带 url

- **WHEN** status body 为 `{"status":"in_progress","url":"/v1/videos/vid_x/content"}`
- **THEN** MUST NOT 计费

### Requirement: 时长来源的优先级与字符串解析

时长（秒）SHALL 按下列顺序取第一个可用值：`video.duration`（数字）、扁平 `seconds`
（**字符串或数字**）、扁平 `duration`（数字）、create 快照中的请求时长。字符串形式的
数字 MUST 被解析为整数（上游实测回 `"seconds":"3"`）；解析不出 MUST 视为该来源不可用
并继续下一个来源，MUST NOT 当作 0 参与计价。所有取值仍 MUST 经过既有的时长归一化
（上下限夹紧）后再落账。

#### Scenario: 字符串时长

- **WHEN** status body 为 `{"status":"completed","url":"/v1/videos/v/content","seconds":"3"}`
- **THEN** 计费时长 SHALL 为 3 秒

#### Scenario: 状态无时长时回落快照

- **WHEN** status body 为 `{"status":"completed","url":"/v1/videos/v/content"}` 且 create 快照记录请求时长 10 秒
- **THEN** 计费时长 SHALL 为 10 秒

#### Scenario: 无法解析的时长不作数

- **WHEN** `seconds` 为 `"abc"` 且快照无时长
- **THEN** 该来源 MUST NOT 被当作 0 秒，系统 SHALL 按既有 fail-closed 分支处理（不计费并记录原因）

### Requirement: 计费身份取请求侧，承做模型只做记录

存在 create 快照时，`billing_model` MUST 优先取快照中记录的请求模型；status body 回显的
模型 MUST 只写入 `upstream_model`（观测事实），MUST NOT 覆盖计费身份。无快照时
（content 下载路径）才可回落到 status 模型。理由：relay 会回显实际承做模型
（实测 `kling-video-v3`），按它计价会查到不存在的价格（漏收或报错），写进 model
字段则会污染模型归属审计。

#### Scenario: relay 回显承做模型

- **WHEN** create 快照的请求模型为 `grok-imagine-video`，status body 回 `"model":"kling-video-v3"`
- **THEN** 计费身份 SHALL 为 `grok-imagine-video`
- **THEN** `upstream_model` SHALL 为 `kling-video-v3`

#### Scenario: 无快照时回落状态模型

- **WHEN** 计费在无 create 快照的路径上被判据触发（如 content 下载路径）
- **THEN** 计费身份 MAY 回落到 status body 的模型，且 MUST 记录「无快照」这一事实

### Requirement: 防重复计费与 fail-closed 语义不变

本次容错 MUST NOT 改变既有的一次性计费结构：同一任务 MUST 通过 claim 只落一条用量，
去重键 MUST 仍为稳定的任务标识（`grok-video:<task>`）。缺 create 快照且 status 也无法
给出时长时 MUST 保持 fail-closed（不计费 + 记录错误日志），MUST NOT 用默认值静默补齐。
分辨率口径 MUST 仍按请求侧（create 快照），交付分辨率与请求不一致时 SHALL 记录该事实
供对账，MUST NOT 静默改写计费档位。

#### Scenario: 多次轮询只计一次

- **WHEN** 同一任务被轮询多次且每次都观察到完成态
- **THEN** 只 SHALL 产生一条用量记录

#### Scenario: 缺快照且无时长

- **WHEN** 完成态观察到取件地址，但既无 create 快照、status 也无任何时长字段
- **THEN** MUST NOT 计费，且 SHALL 记录「缺少创建时快照」的错误日志
