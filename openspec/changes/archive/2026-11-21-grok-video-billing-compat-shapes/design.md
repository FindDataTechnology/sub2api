## Context

`grok_media.go` 的视频计费是「等到轮询观察到一个可计费的完成态，才落 `usage_logs`」的结构：
创建时只存一份 create 快照（`GrokVideoPendingBilling`），完成时按 status body + 快照合成
计量单位（count/resolution/duration/model），由 handler 用 claim 保证只记一次。

这个结构本身没问题。问题在**判据只认官方文档的形状**，而生产上游是经过 relay 的兼容形状。
三个字段各差一点，三者同时落空 ⇒ 零计费且零报错。

## Decisions

### D1 容错只加在「终态词表 / 取件地址 / 时长来源」三处

不改计费流水线结构，不改 claim/dedup 键，不改 fail-closed 分支。容错范围就是这三个
形状差异，且每一个都有实测样本支撑：

| 字段 | 官方 | 实测 relay | 取值优先级 |
| --- | --- | --- | --- |
| 终态 | `status:"done"` | `status:"completed"` | `done` / `completed` / `succeeded` 均算成功 |
| 取件 | `video.url` | 扁平 `url` | `video.url` → 扁平 `url` |
| 时长 | `video.duration:8`（数字） | `seconds:"3"`（**字符串**） | `video.duration` → 扁平 `seconds` → 扁平 `duration` → create 快照 |

`seconds` 是字符串这一点单独写进判据：只按数字解析会让这条链路继续静默漏单，
而它对 `strconv`/`gjson` 的写法有要求（`gjson` 的 `.Int()` 对字符串返回 0）。

### D2 计费身份取请求侧，承做模型只做记录

relay 在 `status.model` 回显的是**实际承做模型**（实测 `kling-video-v3`），
而客户买的是 `grok-imagine-video`。两者混用会有两个后果，方向都错：

- 按承做模型计价 ⇒ 价格表里没有 `kling-video-v3`，落 $0 或计费报错（漏收）；
- 把承做模型写进 `model` 字段 ⇒ 模型审计显示「客户买了 kling」（归因污染）。

所以：`billing_model` 有 create 快照时 MUST 取快照里的请求模型；
status 回显的模型只写 `upstream_model`（观测事实）。无快照时才回落到 status 模型
（content 下载路径的既有行为，保持不变，但 handler 合并时同样优先快照）。

### D3 分辨率仍按请求侧，交付分辨率只记录

实测请求 480p、交付 720P。计费档位跟**请求**走（客户按请求档下单），这与既有
「Resolution is request-only per docs」的口径一致，本 change 不翻转。
但要求把交付分辨率写进日志，让「客户按要求付费、实际拿到更高档」这件事可对账——
不静默。

**这条日志落在 service 提取器，不落 handler**，原因是 handler 里那个 `body` 是
**请求体**（`pkghttputil.ReadRequestBodyWithPrealloc(c.Request)`），不是上游响应体；
把它当「交付值」来比，比的是客户自己写的 480p，与请求值永远相等 ⇒ 这条判据会永远绿。
真正同时握着「交付值（status body 的 `resolution`）」与「请求值（create 快照）」的
只有 `ExtractGrokVideoBillingFromStatusBody`，所以判据放那里。

交付值**不归一化**再比：`NormalizeVideoBillingResolutionOrDefault` 把未登记词一起
映射到 480p，`4k` 与 `480p` 会被判成同一档，不一致就被藏起来了。改用
`sameVideoResolution`（两侧都能登记则按档位比，否则大小写不敏感文本比），
日志里同时打原文与计费档位。

### D4 计费仍要求「终态 + 存在取件地址」

不接受「只要终态就计费」：没有取件地址的完成态说明上游没真正产出（历史上
11-06 那种「HTTP 200 + 站点页面 + 正常计费但零产物」的软封锁形态就是靠这条挡住）。
也不接受「只要 URL 就计费」：`pending`/`in_progress` 阶段上游也可能给占位 URL。

### D5 既有单测的「completed 不计费」断言是刻意反的，必须显式翻转

那三条断言（`TestIsGrokVideoStatusBillable` / `TestExtractGrokVideoBillingRejectsNonDoneStatus` /
`TestGrokMediaUsageFromResponseVideoStatusBillsOnOfficialDone`）当时按官方文档写，
现在被生产证伪。翻转时必须在测试里写清「为什么翻转」，否则下一个人会以为是误改。

## Risks / Trade-offs

- **补收会变成真实扣费**：此前这些视频单是 $0，修复后开始扣。上线需财务/客服知情；
  若要灰度，可按分组或账号先开（本 change 不做开关，避免多一条常年不翻的分支）。
- **兼容形状可能被滥用**：接受扁平 `url` 后，一个伪造的 `{"status":"completed","url":"..."}`
  也会计费。但 status 来自我们自己的上游账号，且仍要求终态；伪造面没有比现在更大。
- **relay 可能再变形状**（例如改回 `succeeded` 或加 `data.` 前缀）：词表按集合匹配而非
  单值比较，新形状只需要改一处常量，不需要再动结构。
