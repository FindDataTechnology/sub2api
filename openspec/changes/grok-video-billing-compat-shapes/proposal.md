## Why

视频异步任务完成后**不记账**，而这条链路已经在生产跑真实单子。

2026-11-14/15 实测（组 19 `fed-workbuddy-media`，经词元网关 `POST /v1/videos`）：

```
创建  202 {"id":"vid_84655a2f5a1f13ee","status":"queued","model":"grok-imagine-video"}
状态  200 {"id":"vid_84655a2f5a1f13ee","status":"completed",
           "model":"kling-video-v3","url":"/v1/videos/vid_84655a2f5a1f13ee/content",
           "seconds":"3","resolution":"720P"}
取件  200 video/mp4  3,140,407 B（h264 1280x720 + aac，3.04s）
```

`usage_logs` 里**一行都没有**（2 小时内视频类 0 行），而产物是真的、上游已计费。

根因是计费判据按 xAI 官方文档写死，只认官方形状：

- `isOfficialGrokVideoStatusDone` 只认 `status == "done"`，上游实际回 `"completed"`；
- `IsGrokVideoStatusBillable` 只认 `video.url`，上游回**扁平** `url`；
- 时长只认 `video.duration`（数字），上游回扁平 `seconds:"3"`（**字符串**）。

三处任一不命中都不会计费——它们同时不命中，所以症状是「全静默零计费」。
更糟的是既有单测**刻意断言** `completed` 不计费（"completed is not the official enum value"），
于是这个缺陷在测试层是「被验证过的正确行为」，重构也不会红。

此外还有一个**计价口径**问题：relay 会在 `status.model` 回显**承做模型**
（`kling-video-v3`），而计费身份应取客户请求的模型（`grok-imagine-video`）。
若按 status 模型计价，价格表里查不到 `kling-video-v3` ⇒ 要么 $0、要么报错，
同时把模型审计也污染成「客户买了 kling」。

## What Changes

- **终态词表容错**：`done` / `completed` / `succeeded` 一律视为成功终态；
  `pending` / `in_progress` / `queued` / `failed` / `expired` / `cancelled` 一律不计费。
- **取件地址容错**：`video.url`（官方）或扁平 `url`（兼容/relay）任一非空即可计费。
- **时长容错**：`video.duration`（数字）→ 扁平 `seconds`（字符串或数字）→ 扁平 `duration`（数字）
  → create 快照，按此优先级取第一个可用值。
- **计费身份取请求侧**：有 create 快照时 `billing_model` MUST 优先取快照里的请求模型；
  上游回显的承做模型只记 `upstream_model`。无快照（content 下载路径）才回落到 status 模型。
- **不改变**：幂等/防重复计费结构（claim + `StableGrokVideoBillingRequestID`）、
  fail-closed 语义（缺快照且 status 无时长仍不计费并留错误日志）、分辨率口径（仍按请求侧）。
- 更新既有单测里「`completed` 不计费」的断言，并新增 relay 形状用例。

## Capabilities

### New Capabilities

- `grok-video-billing`：视频异步任务的计费判据（终态词表、取件地址、计量单位来源）
  与计费身份口径（请求模型 vs 承做模型）。

### Modified Capabilities

（无）

## Impact

- 后端：`backend/internal/service/grok_media.go`（`IsGrokVideoStatusBillable`、
  `isOfficialGrokVideoStatusDone`、`ExtractGrokVideoBillingFromStatusBody`）、
  `backend/internal/handler/grok_media.go`（快照合并时计费身份取请求侧）。
- 测试：`backend/internal/service/grok_media_video_billing_test.go`（改 3 处断言 + 新增用例）。
- 行为影响：此前不计费的视频任务将开始写入 `usage_logs` 并扣额度——这是修复漏收，
  可能会让历史「零成本」客户的费用口径变化；上线前需要知会计费/财务侧。
- 不影响：图片链路、别的平台、客户端协议。
