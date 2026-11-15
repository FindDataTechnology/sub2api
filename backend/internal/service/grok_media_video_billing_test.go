package service

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestGrokVideoE2EDurationFromCreatedAt(t *testing.T) {
	t.Parallel()
	created := time.Now().UTC().Add(-45 * time.Second)
	d := GrokVideoE2EDuration(created.Format(time.RFC3339Nano), time.Now().UTC())
	require.GreaterOrEqual(t, d, 44*time.Second)
	require.LessOrEqual(t, d, 47*time.Second)

	require.Equal(t, time.Duration(0), GrokVideoE2EDuration("", time.Now()))
	require.Equal(t, time.Duration(0), GrokVideoE2EDuration("not-a-time", time.Now()))
	// Future CreatedAt clamps to zero (clock skew).
	require.Equal(t, time.Duration(0), GrokVideoE2EDuration(time.Now().Add(time.Hour).UTC().Format(time.RFC3339Nano), time.Now()))
}

func TestGrokVideoPendingCreatedAtStampOnStoreShape(t *testing.T) {
	t.Parallel()
	// GrokVideoPendingCreatedAtNow must be parseable by GrokVideoE2EDuration.
	stamp := GrokVideoPendingCreatedAtNow()
	require.NotEmpty(t, stamp)
	d := GrokVideoE2EDuration(stamp, time.Now().UTC().Add(2*time.Second))
	require.GreaterOrEqual(t, d, time.Second)
	require.LessOrEqual(t, d, 3*time.Second)
}

func TestIsGrokVideoStatusBillable(t *testing.T) {
	t.Parallel()
	// Official success: status=done + video.url
	require.True(t, IsGrokVideoStatusBillable([]byte(`{
		"status":"done",
		"model":"grok-imagine-video-1.5",
		"video":{"url":"https://vidgen.x.ai/x.mp4","duration":8,"respect_moderation":true}
	}`)))

	// Official non-success states
	require.False(t, IsGrokVideoStatusBillable(nil))
	require.False(t, IsGrokVideoStatusBillable([]byte(`{"status":"pending"}`)))
	require.False(t, IsGrokVideoStatusBillable([]byte(`{"status":"expired"}`)))
	require.False(t, IsGrokVideoStatusBillable([]byte(`{"status":"failed"}`)))
	// done without video.url is not billable
	require.False(t, IsGrokVideoStatusBillable([]byte(`{"status":"done"}`)))
	// A URL alone, without a success terminal, is not billable (placeholder URLs exist
	// while the task is still running).
	require.False(t, IsGrokVideoStatusBillable([]byte(`{"url":"https://example.com/v.mp4"}`)))
	require.False(t, IsGrokVideoStatusBillable([]byte(`{"status":"in_progress","url":"/v1/videos/x/content"}`)))
	require.False(t, IsGrokVideoStatusBillable([]byte(`{"status":"failed","url":"/v1/videos/x/content"}`)))

	// FLIPPED 2026-11-15 — deliberately, not a mistake: this used to assert
	// "completed is not the official enum value" and therefore not billable. Production
	// proved the upstream we serve answers exactly this shape, so the old assertion was
	// silently blessing a lane that billed nothing (group 19 relay, task
	// vid_84655a2f5a1f13ee: status completed + flat url + seconds "3", real 3.1MB MP4).
	require.True(t, IsGrokVideoStatusBillable([]byte(`{
		"status":"completed",
		"model":"kling-video-v3",
		"url":"/v1/videos/vid_84655a2f5a1f13ee/content",
		"seconds":"3"
	}`)))
	// Completed with the official nested url stays billable too.
	require.True(t, IsGrokVideoStatusBillable([]byte(`{"status":"completed","video":{"url":"https://vidgen.x.ai/x.mp4"}}`)))
	// Relay variants: `succeeded` and mixed case.
	require.True(t, IsGrokVideoStatusBillable([]byte(`{"status":"succeeded","url":"/v1/videos/x/content"}`)))
	require.True(t, IsGrokVideoStatusBillable([]byte(`{"status":"COMPLETED","url":"/v1/videos/x/content"}`)))
	// A success terminal without any url is still not billable.
	require.False(t, IsGrokVideoStatusBillable([]byte(`{"status":"completed"}`)))
}

func TestExtractGrokVideoBillingFromStatusBodyPrefersUpstreamParams(t *testing.T) {
	t.Parallel()
	pending := &GrokVideoPendingBilling{
		Model:                "pending-model",
		BillingModel:         "pending-billing",
		UpstreamModel:        "pending-upstream",
		VideoResolution:      VideoBillingResolution720P,
		VideoDurationSeconds: 8,
	}
	// Official completed body from docs.x.ai Video Generation.
	body := []byte(`{
		"status":"done",
		"model":"grok-imagine-video-1.5",
		"video":{"url":"https://vidgen.x.ai/signed.mp4","duration":12,"respect_moderation":true}
	}`)
	result := ExtractGrokVideoBillingFromStatusBody(body, pending, "req-1")
	require.NotNil(t, result)
	require.Equal(t, 1, result.VideoCount)
	// FLIPPED 2026-11-15 — deliberately: with a create-time snapshot the billing identity
	// is the requested model, not the status body's model. For the official shape the two
	// usually agree; for the relay they do not (status reports the serving model, e.g.
	// kling-video-v3) and pricing is keyed on what the customer bought. The status model
	// is preserved in UpstreamModel.
	require.Equal(t, "pending-billing", result.BillingModel)
	require.Equal(t, "pending-billing", result.Model)
	require.Equal(t, "grok-imagine-video-1.5", result.UpstreamModel)
	// Resolution is not in official status response — use create-time request.
	require.Equal(t, VideoBillingResolution720P, result.VideoResolution)
	// Duration prefers official video.duration.
	require.Equal(t, 12, result.VideoDurationSeconds)
}

func TestExtractGrokVideoBillingFromStatusBodyFallsBackToPending(t *testing.T) {
	t.Parallel()
	pending := &GrokVideoPendingBilling{
		Model:                "create-model",
		BillingModel:         "create-billing",
		UpstreamModel:        "create-upstream",
		VideoResolution:      VideoBillingResolution1080P,
		VideoDurationSeconds: 10,
	}
	// done + video.url, but no model/duration in body.
	body := []byte(`{"status":"done","video":{"url":"https://vidgen.x.ai/signed.mp4"}}`)
	result := ExtractGrokVideoBillingFromStatusBody(body, pending, "req-2")
	require.NotNil(t, result)
	require.Equal(t, "create-billing", result.BillingModel)
	require.Equal(t, "create-upstream", result.UpstreamModel)
	require.Equal(t, VideoBillingResolution1080P, result.VideoResolution)
	require.Equal(t, 10, result.VideoDurationSeconds)
}

func TestExtractGrokVideoBillingRejectsNonSuccessStatus(t *testing.T) {
	t.Parallel()
	pending := &GrokVideoPendingBilling{Model: "m", VideoDurationSeconds: 8, VideoResolution: "720p"}
	require.Nil(t, ExtractGrokVideoBillingFromStatusBody(
		[]byte(`{"status":"pending","video":{"url":"https://vidgen.x.ai/x.mp4","duration":8}}`),
		pending, "req",
	))
	require.Nil(t, ExtractGrokVideoBillingFromStatusBody(
		[]byte(`{"status":"in_progress","url":"/v1/videos/x/content","seconds":"8"}`),
		pending, "req",
	))
	require.Nil(t, ExtractGrokVideoBillingFromStatusBody(
		[]byte(`{"status":"failed","url":"/v1/videos/x/content","seconds":"8"}`),
		pending, "req",
	))
	// FLIPPED 2026-11-15 — deliberately: `completed` used to be asserted nil here because
	// it is not the official enum value. It is what our upstream actually answers, so
	// treating it as non-billable was the silent-zero-billing defect itself.
	require.NotNil(t, ExtractGrokVideoBillingFromStatusBody(
		[]byte(`{"status":"completed","video":{"url":"https://vidgen.x.ai/x.mp4","duration":8}}`),
		pending, "req",
	))
}

func TestGrokMediaUsageFromResponseVideoCreateDoesNotBill(t *testing.T) {
	t.Parallel()
	info := GrokMediaRequestInfo{Model: "grok-imagine-video", Resolution: "720p", DurationSeconds: 10}
	meta := grokMediaUsageFromResponse(GrokMediaEndpointVideosGenerations, info, []byte(`{"request_id":"v1"}`))
	require.Equal(t, "v1", meta.ResponseID)
	require.Equal(t, 0, meta.VideoCount)
	require.Equal(t, 10, meta.VideoDurationSeconds)
	require.Equal(t, VideoBillingResolution720P, meta.VideoResolution)
}

func TestGrokMediaUsageFromResponseVideoStatusBillsOnOfficialDone(t *testing.T) {
	t.Parallel()
	meta := grokMediaUsageFromResponse(
		GrokMediaEndpointVideoStatus,
		GrokMediaRequestInfo{},
		[]byte(`{"status":"done","model":"grok-imagine-video-1.5","video":{"url":"https://vidgen.x.ai/a.mp4","duration":9}}`),
	)
	require.Equal(t, 1, meta.VideoCount)
	require.Equal(t, 9, meta.VideoDurationSeconds)
	require.Equal(t, "grok-imagine-video-1.5", meta.Model)

	// Official non-done must not set billable units.
	pendingOnly := grokMediaUsageFromResponse(
		GrokMediaEndpointVideoStatus,
		GrokMediaRequestInfo{},
		[]byte(`{"status":"pending"}`),
	)
	require.Equal(t, 0, pendingOnly.VideoCount)

	// FLIPPED 2026-11-15 — deliberately: `completed` is the shape our upstream relay
	// actually returns, so it must bill. Keeping the old "not official → 0" assertion is
	// what made every completed video free.
	completed := grokMediaUsageFromResponse(
		GrokMediaEndpointVideoStatus,
		GrokMediaRequestInfo{},
		[]byte(`{"status":"completed","model":"kling-video-v3","url":"/v1/videos/x/content","seconds":"3"}`),
	)
	require.Equal(t, 1, completed.VideoCount)
	require.Equal(t, 3, completed.VideoDurationSeconds)
}

// TestExtractGrokVideoBillingFromRelayShape is the production incident reproduced
// offline: status completed + flat url + string seconds + serving-model echo.
func TestExtractGrokVideoBillingFromRelayShape(t *testing.T) {
	t.Parallel()
	pending := &GrokVideoPendingBilling{
		Model:                "grok-imagine-video",
		BillingModel:         "grok-imagine-video",
		VideoResolution:      "480p",
		VideoDurationSeconds: 5,
	}
	body := []byte(`{
		"id":"vid_84655a2f5a1f13ee",
		"status":"completed",
		"model":"kling-video-v3",
		"url":"/v1/videos/vid_84655a2f5a1f13ee/content",
		"seconds":"3",
		"resolution":"720P"
	}`)
	result := ExtractGrokVideoBillingFromStatusBody(body, pending, "")
	require.NotNil(t, result)
	require.Equal(t, 1, result.VideoCount)
	// Billing identity = what the customer bought; serving model is recorded separately.
	require.Equal(t, "grok-imagine-video", result.BillingModel)
	require.Equal(t, "grok-imagine-video", result.Model)
	require.Equal(t, "kling-video-v3", result.UpstreamModel)
	// String `seconds` must parse (the relay answers "3", not 3).
	require.Equal(t, 3, result.VideoDurationSeconds)
	// Billing tier stays on the requested resolution even though 720P was delivered.
	require.Equal(t, "480p", result.VideoResolution)
	require.Equal(t, "vid_84655a2f5a1f13ee", result.ResponseID)
}

// TestExtractGrokVideoBillingRelayUnparsableDurationFailsClosed pins the fail-closed
// edge: a garbage duration must not be billed as 0 seconds, and with no snapshot there
// is nothing to fall back to.
func TestExtractGrokVideoBillingRelayUnparsableDurationFailsClosed(t *testing.T) {
	t.Parallel()
	body := []byte(`{"status":"completed","url":"/v1/videos/x/content","seconds":"abc"}`)
	result := ExtractGrokVideoBillingFromStatusBody(body, nil, "req-x")
	require.NotNil(t, result)
	require.Equal(t, 0, result.VideoDurationSeconds) // caller's fail-closed check sees 0
	// With a snapshot the snapshot duration is used instead.
	withPending := ExtractGrokVideoBillingFromStatusBody(body, &GrokVideoPendingBilling{
		Model: "grok-imagine-video", BillingModel: "grok-imagine-video",
		VideoDurationSeconds: 10, VideoResolution: "720p",
	}, "req-x")
	require.NotNil(t, withPending)
	require.Equal(t, 10, withPending.VideoDurationSeconds)
}

// TestGrokVideoDeliveredResolution covers the shape-reading half of the delivery
// disclosure: what upstream claims it produced, verbatim, and empty when it says nothing.
func TestGrokVideoDeliveredResolution(t *testing.T) {
	t.Parallel()
	// Relay answers the flat field with an upper-case tier.
	require.Equal(t, "720P", GrokVideoDeliveredResolution([]byte(`{"status":"completed","resolution":"720P"}`)))
	// Nested variant, for shapes that nest resolution under video.
	require.Equal(t, "1080p", GrokVideoDeliveredResolution([]byte(`{"status":"done","video":{"resolution":"1080p"}}`)))
	// Official status response carries no resolution at all → unknown, not 480p.
	require.Equal(t, "", GrokVideoDeliveredResolution([]byte(`{"status":"done","video":{"url":"https://x/y.mp4","duration":8}}`)))
	// Unparseable / empty bodies are unknown too.
	require.Equal(t, "", GrokVideoDeliveredResolution(nil))
	require.Equal(t, "", GrokVideoDeliveredResolution([]byte(`not-json`)))
	// An unrecognized word must stay visible; collapsing it to the 480p default would
	// make every unknown delivery look like a 480p one and hide the mismatch entirely.
	require.Equal(t, "4k", GrokVideoDeliveredResolution([]byte(`{"status":"completed","resolution":"4k"}`)))
}

// TestGrokVideoSameVideoResolution pins the comparison used to decide whether a delivery
// mismatch deserves a log line: same tier in a different case is the same tier; words
// outside our vocabulary fall back to case-insensitive text comparison.
//
// Named with the GrokVideo prefix on purpose: a targeted run of the video-billing family
// (`-run 'GrokVideo|GrokMediaUsageFromResponseVideo'`) must not silently skip this one.
func TestGrokVideoSameVideoResolution(t *testing.T) {
	t.Parallel()
	require.True(t, sameVideoResolution("720p", "720P"))
	require.True(t, sameVideoResolution("480p", "480p"))
	require.False(t, sameVideoResolution("480p", "720P"))
	require.True(t, sameVideoResolution("4k", "4K"))
	require.False(t, sameVideoResolution("4k", "8k"))
}

// TestGrokVideoResolutionsMismatchMarksDelivery 钉住「交付与计费档不一致」的判据本身。
//
// 为什么这条判据要有自己的用例：它此前写在提取器的 pending!=nil 分支里，而生产两个
// 调用点都传 pending=nil ⇒ 那段从未执行过（2026-11-15 实调用 480p、上游交 720P，
// 账单正确但一行留痕都没有）。现在判据被搬到同时握着「创建快照」与「状态体」的完成路径，
// 这里的断言保证「未知不当成不同」和三档实际比较都不回归。
func TestGrokVideoResolutionsMismatchMarksDelivery(t *testing.T) {
	t.Parallel()
	// The production shape: billed 480p from the create request, upstream delivered 720P.
	require.True(t, VideoResolutionMismatch("480p", "720P"))
	require.True(t, VideoResolutionMismatch("1080P", "720p"))
	// Same tier in a different case is not a mismatch.
	require.False(t, VideoResolutionMismatch("720p", "720P"))
	require.False(t, VideoResolutionMismatch("480p", "480p"))
	// Unknown on either side must never manufacture a mismatch: the official status body
	// carries no resolution at all, and that is the common case for xAI-native responses.
	require.False(t, VideoResolutionMismatch("480p", ""))
	require.False(t, VideoResolutionMismatch("", "720P"))
	require.False(t, VideoResolutionMismatch("", ""))
}

// TestGrokVideoExtractCarriesDeliveredResolution 保证「交付了什么 / 承做是谁」被带出提取器。
//
// 提取器本身拿不到创建快照（生产调用点传 nil），所以它不能自己比较；它唯一能做且必须
// 做的是把状态体里的交付档与承做模型原样放进结果，让完成路径去比。缺了这一步，判据再
// 对也无数据。这里同时钉住一个真实的返工点（2026-11-15 15:0xZ 线上实测）：首轮只在
// 提取器加了字段，两个组装点（status-poll 的 grokMediaUsageFromResponse 与 content
// 下载路径）都是逐字段显式拷贝、没有新字段 ⇒ 线上日志 delivered_resolution=""、
// resolution_mismatch=false，看起来像「查过了、没差异」。
func TestGrokVideoExtractCarriesDeliveredResolution(t *testing.T) {
	t.Parallel()
	relay := []byte(`{"status":"completed","model":"kling-video-v3","seconds":"3","resolution":"720P","url":"/v1/videos/vid_x/content"}`)
	got := ExtractGrokVideoBillingFromStatusBody(relay, nil, "vid_x")
	require.NotNil(t, got)
	require.Equal(t, "720P", got.VideoResolutionDelivered, "delivered tier must survive extraction verbatim")
	require.Equal(t, "kling-video-v3", got.VideoServingModel, "status self-reported model must survive extraction")
	require.Equal(t, 3, got.VideoDurationSeconds)

	official := []byte(`{"status":"done","model":"grok-imagine-video-1.5","video":{"url":"https://x/y.mp4","duration":8}}`)
	got = ExtractGrokVideoBillingFromStatusBody(official, nil, "vid_y")
	require.NotNil(t, got)
	require.Equal(t, "", got.VideoResolutionDelivered, "official shape carries no resolution → unknown, not a default")
	require.Equal(t, "grok-imagine-video-1.5", got.VideoServingModel)
}

// TestGrokVideoUsageMetaCarriesDeliveredResolution 钉住两个组装点里最容易被漏的那个。
//
// grokMediaUsageFromResponse 的 VideoStatus 分支是状态轮询路径上唯一能把交付档/承做
// 模型带出 ForwardGrokMedia 的地方；首轮返工漏掉的正是它（2026-11-15 线上实测日志为
// 空）。这条用例的存在理由：任何往这几个结构里加字段的人，都会在改完这里后看到测试
// 红——而不是在上线后看到空日志。
func TestGrokVideoUsageMetaCarriesDeliveredResolution(t *testing.T) {
	t.Parallel()
	info := GrokMediaRequestInfo{Model: "grok-imagine-video", Resolution: "480p", DurationSeconds: 3}
	body := []byte(`{"status":"completed","model":"kling-video-v3","request_id":"vid_meta","seconds":"3","resolution":"720P","url":"/v1/videos/vid_meta/content"}`)
	meta := grokMediaUsageFromResponse(GrokMediaEndpointVideoStatus, info, body)
	require.Equal(t, "720P", meta.VideoResolutionDelivered)
	require.Equal(t, "kling-video-v3", meta.VideoServingModel)
	require.Equal(t, 3, meta.VideoDurationSeconds)
}
