## 1. 判据容错（service 层）

- [x] 1.1 `grok_media.go`：终态词表改为集合 `done` / `completed` / `succeeded`
      （大小写不敏感），替换 `isOfficialGrokVideoStatusDone` 的单值比较
      （`grokVideoStatusSuccessWords` + `isGrokVideoStatusSuccess`）
- [x] 1.2 `IsGrokVideoStatusBillable`：取件地址接受 `video.url` 或扁平 `url`
      （第一个非空），仍要求成功终态（`grokVideoContentURLFromStatus`）
- [x] 1.3 `ExtractGrokVideoBillingFromStatusBody` 时长：`video.duration`（数字）→
      扁平 `seconds`（**字符串或数字**）→ 扁平 `duration`（数字）→ create 快照；
      解析不出的来源跳过，不当作 0（`grokVideoDurationSecondsFromStatus` 返回 0 表示
      「该来源不可用」，由调用点回落快照，而不是把 0 当值）
- [x] 1.4 计费身份：有快照时 `BillingModel` 取快照请求模型，status 模型只入
      `UpstreamModel`；无快照才回落 status 模型

## 2. 快照合并口径（handler 层）

- [x] 2.1 `handler/grok_media.go` 的完成态合并：`BillingModel` 在有快照时 MUST 优先
      取快照（现状是「仅当为空才填」，会被 status 承做模型占位）
- [x] 2.2 交付分辨率与请求分辨率不一致时记录日志（不改计费档位）
      —— 落点在 service 提取器 `ExtractGrokVideoBillingFromStatusBody`：
      只有那里同时握着 status body（`resolution` 交付值）与 create 快照（请求值）。
      handler 侧的 `body` 是**请求体**（`ReadRequestBodyWithPrealloc`），拿它比会把
      请求值当交付值、永远不报，故刻意不放 handler。
      判据：`GrokVideoDeliveredResolution` 取原文（不归一化，避免 `4k` 塌进 480p 默认
      值而把不一致藏起来），`sameVideoResolution` 按计费档位比较；不一致时打
      `service.grok_media video_resolution_delivered_differs`，**计费档位不动**。

## 3. 测试

- [x] 3.1 翻转 3 处既有断言（`TestIsGrokVideoStatusBillable` /
      `TestExtractGrokVideoBillingRejectsNonSuccessStatus`（原 `…RejectsNonDoneStatus`）/
      `TestGrokMediaUsageFromResponseVideoStatusBillsOnOfficialDone`），
      每处都写明「FLIPPED 2026-11-15 — deliberately, not a mistake」并附生产任务号
- [x] 3.2 新增 relay 形状用例（`TestExtractGrokVideoBillingFromRelayShape`）：
      `completed` + 扁平 `url` + `seconds:"3"` + `model:kling-video-v3` +
      快照 `grok-imagine-video` ⇒ 可计费、时长 3、
      billing_model=grok-imagine-video、upstream_model=kling-video-v3、分辨率留在请求档
- [x] 3.3 负向用例：`completed` 无 url 不计费；`in_progress`/`failed` 带 url 不计费；
      `seconds:"abc"` 时长为 0 且不宜静默（`…RelayUnparsableDurationFailsClosed`）；
      另补 `TestGrokVideoDeliveredResolution` / `TestGrokVideoSameVideoResolution` 覆盖 2.2 的判据
- [x] 3.4 `go test ./internal/service/ -run 'GrokVideo|GrokMediaUsageFromResponseVideo'`
      全绿（`GOPROXY=https://goproxy.cn,direct`，本机 Go 1.27.1 需让 toolchain 自动拉 1.27.3）
      —— 本机 swap 打满、load 峰值 50（8 核），`ent` + `modernc.org/sqlite/lib` 编不出来；
      改在 cheap9（8 核 / 5G 可用）上跑：`RUN=16 PASS=16 FAIL=0 EXIT=0`。
      同批 `go build ./internal/handler/ ./internal/service/` 与
      `go vet ./internal/handler/` 均通过。
      **踩到一次「用例存在但没跑」**：`TestSameVideoResolution` 名字不含 `GrokVideo`，
      被家族正则静默跳过（首次跑只有 15 条）；已改名 `TestGrokVideoSameVideoResolution`
      并在注释里写明「起名要落在家族正则里」，否则这条判据永远没人执行过。

## 4. 收口

- [x] 4.1 `openspec validate grok-video-billing-compat-shapes --strict` 通过
      —— 实跑输出 `Change 'grok-video-billing-compat-shapes' is valid`
- [x] 4.2 记一条上线注意：修复后此前 $0 的视频单开始扣费，需财务/客服知情
      （写进 proposal 的 Impact 与 `docs/debt-ledger.md` 相应条目，若存在）
      —— 本仓 `docs/` 下**没有** `debt-ledger.md`（已确认），已写进 proposal 的 Impact；
      上线前把这一句读给财务/客服：**同一批视频单从此开始产生费用**，不是新涨价
- [ ] 4.3 生产验证：上线后对一次性真实任务核对 `usage_logs` 出现
      `grok-video:<task>` 的用量行（本 change 不含出片/滚版，留给发布窗口）

## 归档豁免

归档豁免：2026-11-21 4.3 需一条真实视频单核对 `usage_logs` 的 `grok-video:<task>` 行；代码已随 03a489e43 进 ciyuan-v0.2.23-1 树，验证待下一笔真实订单
