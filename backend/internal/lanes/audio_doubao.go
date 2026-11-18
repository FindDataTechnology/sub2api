package lanes

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/Wei-Shaw/sub2api/internal/service"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"go.uber.org/zap"
)

// 豆包语音（openspeech）上游适配器。
//
// 与既有 OpenAI 兼容适配器的根本差异：**协议不是 HTTP 表单/JSON**。
//   - TTS：WebSocket 双向流 + V3 二进制帧（见 audio_doubao_frame.go）
//   - ASR：异步任务（submit 提交 → query 轮询），且上游只接受音频 URL
//
// 对外契约仍是 OpenAI 音频形状（由 audio.go 的分派层保证）——上游差异全部
// 吞在这里。计量口径复用既有 Group 音频三价（TTS 每百万字符 / STT 每小时）。
const (
	doubaoWSURL        = "wss://openspeech.bytedance.com/api/v3/tts/bidirection"
	doubaoASRSubmitURL = "https://openspeech.bytedance.com/api/v3/auc/bigmodel/submit"
	doubaoASRQueryURL  = "https://openspeech.bytedance.com/api/v3/auc/bigmodel/query"

	// 资源 ID 决定模型版本与计费商品（官方文档）。
	doubaoResourceTTS = "seed-tts-2.0"
	doubaoResourceASR = "volc.seedasr.auc"

	// 2.0 音色必须 *uranus_bigtts；1.0 的 *moon_bigtts 会报
	// "resource ID is mismatched with speaker related resource"（实测）。
	doubaoDefaultTTSVoice = "zh_female_shuangkuaisisi_uranus_bigtts"

	doubaoASRPollInterval = time.Second
	doubaoASRPollTimeout  = 120 * time.Second
	doubaoWSDialTimeout   = 15 * time.Second
	doubaoWSReadTimeout   = 120 * time.Second
)

// doubaoAdapter 豆包上游能力实现（由 audio.go 按账号协议分派调用）。
type doubaoAdapter struct {
	httpClient *http.Client
	dialer     *websocket.Dialer
	// 端点可注入（测试指向本地桩；生产保持官方常量）。
	wsURL     string
	submitURL string
	queryURL  string
	// submitAndPoll 允许测试注入（生产走真实 HTTP）。
	submitAndPoll func(ctx context.Context, apiKeyStr, audioURL string, opts doubaoASROptions) (*doubaoASRResult, error)
}

func newDoubaoAdapter() *doubaoAdapter {
	a := &doubaoAdapter{
		httpClient: &http.Client{Timeout: 60 * time.Second},
		dialer:     &websocket.Dialer{HandshakeTimeout: doubaoWSDialTimeout},
		wsURL:      doubaoWSURL,
		submitURL:  doubaoASRSubmitURL,
		queryURL:   doubaoASRQueryURL,
	}
	a.submitAndPoll = a.realSubmitAndPoll
	return a
}

// ── TTS（WebSocket 双向流）─────────────────────────────────────────────────────

// doubaoTTSOptions 一次合成的入参。
type doubaoTTSOptions struct {
	Text       string
	Voice      string // 2.0 音色名；空则用默认
	Format     string // mp3 / pcm / ogg_opus
	SampleRate int
}

// synthesize 完成一次 TTS：建连 → StartConnection → StartSession → TaskRequest
// → FinishSession → 收齐音频帧 → 返回拼接后的音频字节。
//
// 事件序列（实测）：发 StartConnection(1) 收 ConnectionStarted(50)；
// 发 StartSession(100) 收 SessionStarted(150)；发 TaskRequest(200)；
// 发 FinishSession(102) 收 SessionFinished(152)，其间收 TTSResponse(352) 音频帧。
func (a *doubaoAdapter) synthesize(ctx context.Context, apiKeyStr string, opts doubaoTTSOptions) ([]byte, error) {
	if strings.TrimSpace(apiKeyStr) == "" {
		return nil, errDoubaoMissingCredential
	}
	voice := strings.TrimSpace(opts.Voice)
	if voice == "" {
		voice = doubaoDefaultTTSVoice
	}
	format := strings.TrimSpace(opts.Format)
	if format == "" {
		format = "mp3"
	}
	sampleRate := opts.SampleRate
	if sampleRate <= 0 {
		sampleRate = 24000
	}

	headers := http.Header{}
	headers.Set("X-Api-Key", apiKeyStr)
	headers.Set("X-Api-Resource-Id", doubaoResourceTTS)
	headers.Set("X-Api-Connect-Id", newUUIDLike())

	conn, resp, err := a.dialer.DialContext(ctx, a.wsURL, headers)
	if err != nil {
		// 建连失败：把上游状态码上报给分级判据（401/403 → 长冷却）。
		status := 0
		if resp != nil {
			status = resp.StatusCode
		}
		return nil, &doubaoUpstreamError{status: status, err: err}
	}
	defer func() { _ = conn.Close() }()

	sessionID := newUUIDLike()
	deadline := time.Now().Add(doubaoWSReadTimeout)
	_ = conn.SetReadDeadline(deadline)

	// 1. 建连
	if err := conn.WriteMessage(websocket.BinaryMessage,
		buildDoubaoFrame(doubaoMsgTypeFullClient, doubaoFlagWithEvent, doubaoEventStartConnection, "", []byte("{}"))); err != nil {
		return nil, &doubaoUpstreamError{status: 0, err: err}
	}

	var audioFrames []*doubaoFrame
	connectionStarted := false
	sessionStarted := false
	textSent := false
	finished := false

	for !finished {
		msgType, raw, err := conn.ReadMessage()
		if err != nil {
			return nil, &doubaoUpstreamError{status: 0, err: err}
		}
		if msgType != websocket.BinaryMessage {
			continue
		}
		f, err := parseDoubaoFrame(raw)
		if err != nil {
			return nil, &doubaoUpstreamError{status: 0, err: err}
		}
		if f.isError() {
			return nil, &doubaoUpstreamError{status: 0, code: f.errorCode, message: f.errMessage()}
		}
		switch f.event {
		case doubaoEventConnectionStarted:
			connectionStarted = true
			// 2. 建会话（携带 2.0 音色与音频参数）
			startSession := map[string]any{
				"user":      map[string]any{"uid": "ciyuan-lane"},
				"event":     doubaoEventStartSession, // 必须是 int（实测：字符串被拒）
				"namespace": "BidirectionalTTS",
				"req_params": map[string]any{
					"speaker": voice,
					"audio_params": map[string]any{
						"format":      format,
						"sample_rate": sampleRate,
					},
				},
			}
			payload, _ := json.Marshal(startSession)
			if err := conn.WriteMessage(websocket.BinaryMessage,
				buildDoubaoFrame(doubaoMsgTypeFullClient, doubaoFlagWithEvent, doubaoEventStartSession, sessionID, payload)); err != nil {
				return nil, &doubaoUpstreamError{status: 0, err: err}
			}
		case doubaoEventSessionStarted:
			sessionStarted = true
			// 3. 下发文本
			taskReq := map[string]any{
				"event": doubaoEventTaskRequest,
				"req_params": map[string]any{
					"text": opts.Text,
				},
			}
			payload, _ := json.Marshal(taskReq)
			if err := conn.WriteMessage(websocket.BinaryMessage,
				buildDoubaoFrame(doubaoMsgTypeFullClient, doubaoFlagWithEvent, doubaoEventTaskRequest, sessionID, payload)); err != nil {
				return nil, &doubaoUpstreamError{status: 0, err: err}
			}
			textSent = true
			// 4. 立即声明结束（无更多文本）
			if err := conn.WriteMessage(websocket.BinaryMessage,
				buildDoubaoFrame(doubaoMsgTypeFullClient, doubaoFlagWithEvent, doubaoEventFinishSession, sessionID, []byte("{}"))); err != nil {
				return nil, &doubaoUpstreamError{status: 0, err: err}
			}
		case doubaoEventTTSResponse:
			audioFrames = append(audioFrames, f)
		case doubaoEventSessionFinished:
			finished = true
		case doubaoEventSessionFailed, doubaoEventConnectionFailed:
			return nil, &doubaoUpstreamError{status: 0, message: string(f.payload)}
		}
	}

	if !connectionStarted || !sessionStarted || !textSent {
		return nil, &doubaoUpstreamError{status: 0, message: "incomplete event sequence"}
	}
	audio := doubaoAudioChunks(audioFrames)
	if len(audio) == 0 {
		return nil, &doubaoUpstreamError{status: 0, message: "upstream returned no audio"}
	}
	return audio, nil
}

// ── ASR（异步任务：submit + query 轮询）───────────────────────────────────────

// doubaoASROptions 转写入参。
type doubaoASROptions struct {
	AudioURL string // 上游只接受公网可达 URL
	Format   string // wav / mp3 / ogg / spx / amr / aac / m4a
	Language string // 可选，空则由上游自动判定
}

// doubaoASRResult 转写结果。
type doubaoASRResult struct {
	Text           string
	DurationSecond float64 // 计费依据
}

// realSubmitAndPoll 真实实现：submit 拿受理 → 轮询 query 直到完成或超时。
func (a *doubaoAdapter) realSubmitAndPoll(ctx context.Context, apiKeyStr, audioURL string, opts doubaoASROptions) (*doubaoASRResult, error) {
	taskID := newUUIDLike()

	audio := map[string]any{
		"url":    audioURL,
		"format": opts.Format,
	}
	if opts.Format == "wav" || opts.Format == "pcm" {
		audio["codec"] = "raw"
		audio["rate"] = 16000
		audio["bits"] = 16
		audio["channel"] = 1
	}
	body := map[string]any{
		"user":    map[string]any{"uid": "ciyuan-lane"},
		"audio":   audio,
		"request": map[string]any{"model_name": "bigmodel", "enable_punc": true},
	}
	if opts.Language != "" {
		audio, ok := body["audio"].(map[string]any)
		if !ok {
			return nil, fmt.Errorf("doubao: audio payload is not an object")
		}
		audio["language"] = opts.Language
	}
	payload, _ := json.Marshal(body)

	if err := a.doubaoASRCall(ctx, a.submitURL, apiKeyStr, taskID, payload, true); err != nil {
		return nil, err
	}

	deadline := time.Now().Add(doubaoASRPollTimeout)
	interval := doubaoASRPollInterval
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(interval):
		}
		queryBody, err := a.doubaoASRQuery(ctx, apiKeyStr, taskID)
		if err != nil {
			return nil, err
		}
		if queryBody != nil {
			return queryBody, nil
		}
		// 未完成：小幅退避（1s → 2s → 3s 封顶 3s）
		if interval < 3*time.Second {
			interval += time.Second
		}
	}
	return nil, &doubaoUpstreamError{status: 0, message: "asr task polling timed out"}
}

// doubaoASRCall 提交任务（只判受理，不取内容）。
func (a *doubaoAdapter) doubaoASRCall(ctx context.Context, url, apiKeyStr, taskID string, payload []byte, isSubmit bool) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return &doubaoUpstreamError{status: 0, err: err}
	}
	req.Header.Set("X-Api-Key", apiKeyStr)
	req.Header.Set("X-Api-Resource-Id", doubaoResourceASR)
	req.Header.Set("X-Api-Request-Id", taskID)
	req.Header.Set("Content-Type", "application/json")
	if isSubmit {
		req.Header.Set("X-Api-Sequence", "-1")
	}
	resp, err := a.httpClient.Do(req)
	if err != nil {
		return &doubaoUpstreamError{status: 0, err: err}
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))

	statusCode := resp.Header.Get("X-Api-Status-Code")
	if statusCode != "" && statusCode != "20000000" {
		msg := resp.Header.Get("X-Api-Message")
		return &doubaoUpstreamError{
			status:  resp.StatusCode,
			code:    parseStatusHeader(statusCode),
			message: msg,
		}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return &doubaoUpstreamError{status: resp.StatusCode}
	}
	return nil
}

// doubaoASRQuery 查询一次：未完成返回 (nil, nil)；完成返回结果。
func (a *doubaoAdapter) doubaoASRQuery(ctx context.Context, apiKeyStr, taskID string) (*doubaoASRResult, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.queryURL, strings.NewReader("{}"))
	if err != nil {
		return nil, &doubaoUpstreamError{status: 0, err: err}
	}
	req.Header.Set("X-Api-Key", apiKeyStr)
	req.Header.Set("X-Api-Resource-Id", doubaoResourceASR)
	req.Header.Set("X-Api-Request-Id", taskID)
	req.Header.Set("Content-Type", "application/json")

	resp, err := a.httpClient.Do(req)
	if err != nil {
		return nil, &doubaoUpstreamError{status: 0, err: err}
	}
	defer func() { _ = resp.Body.Close() }()

	statusCode := resp.Header.Get("X-Api-Status-Code")
	// 20000000=完成；20000001/20000002=处理中（官方口径）；其余=失败。
	switch statusCode {
	case "20000000":
		// 继续解析
	case "20000001", "20000002", "":
		return nil, nil
	default:
		return nil, &doubaoUpstreamError{
			status:  resp.StatusCode,
			code:    parseStatusHeader(statusCode),
			message: resp.Header.Get("X-Api-Message"),
		}
	}

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, &doubaoUpstreamError{status: resp.StatusCode, err: err}
	}
	var out struct {
		AudioInfo struct {
			Duration float64 `json:"duration"` // 毫秒
		} `json:"audio_info"`
		Result struct {
			Text string `json:"text"`
		} `json:"result"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, &doubaoUpstreamError{status: resp.StatusCode, err: err}
	}
	return &doubaoASRResult{
		Text:           out.Result.Text,
		DurationSecond: out.AudioInfo.Duration / 1000, // ms → s
	}, nil
}

// ── 错误与辅助 ────────────────────────────────────────────────────────────────

// doubaoUpstreamError 豆包上游失败事实（映射到车道分级判据）。
type doubaoUpstreamError struct {
	status  int    // HTTP 状态码；0=传输层失败
	code    int    // 上游业务码（如 45000030）
	message string // 上游错误文本（仅进分级判据，不外泄）
	err     error
}

func (e *doubaoUpstreamError) Error() string {
	if e.err != nil {
		return fmt.Sprintf("doubao upstream error (status %d): %v", e.status, e.err)
	}
	return fmt.Sprintf("doubao upstream error (status %d, code %d): %s", e.status, e.code, e.message)
}

var errDoubaoMissingCredential = fmt.Errorf("doubao account missing api_key")

// newUUIDLike 生成 RFC4122 v4 形态的 UUID（连接/会话/请求 ID 用）。
func newUUIDLike() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		// 退化路径：时间驱动的十六进制，仍保证唯一性足够。
		return fmt.Sprintf("%x", time.Now().UnixNano())
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// parseStatusHeader 上游 X-Api-Status-Code 是十进制字符串（如 "20000000"）。
func parseStatusHeader(s string) int {
	n := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0
		}
		n = n*10 + int(c-'0')
	}
	return n
}

// ── 绑定到车道适配器（audio.go 分派层调用）─────────────────────────────────────

// serveDoubaoTTS 豆包 TTS 的一次尝试：成功则写响应并计量，失败返回失败事实。
func (a *AudioAdapter) serveDoubaoTTS(c *gin.Context, apiKey *service.APIKey, account *service.Account,
	meter *Meter, started time.Time, input, voice, format string, sampleRate int) (bool, *upstreamFailure) {

	key, _ := account.Credentials["api_key"].(string)
	if strings.TrimSpace(key) == "" {
		return false, &upstreamFailure{misconfigured: true}
	}
	audioBytes, err := a.doubao.synthesize(c.Request.Context(), key, doubaoTTSOptions{
		Text:       input,
		Voice:      voice,
		Format:     format,
		SampleRate: sampleRate,
	})
	if err != nil {
		return false, doubaoToUpstreamFailure(err)
	}
	c.Data(http.StatusOK, "audio/mpeg", audioBytes)

	if meter != nil {
		chars := utf8.RuneCountInString(input)
		meter.WriteUsage(c, apiKey, UsageInput{
			Lane:             LaneAudio,
			Model:            "audio:tts",
			UpstreamModel:    "doubao:" + voice,
			RequestID:        laneRequestID(c),
			Account:          account,
			Audio:            &service.AudioUsage{Mode: "tts", DurationOrUnits: float64(chars) / 1_000_000},
			Duration:         time.Since(started),
			UpstreamEndpoint: "/v3/tts/bidirection",
		})
	}
	return true, nil
}

// serveDoubaoASR 豆包 ASR 的一次尝试。
func (a *AudioAdapter) serveDoubaoASR(c *gin.Context, apiKey *service.APIKey, account *service.Account,
	meter *Meter, started time.Time, form *asrFormData, audioURL string) (bool, *upstreamFailure) {

	key, _ := account.Credentials["api_key"].(string)
	if strings.TrimSpace(key) == "" {
		return false, &upstreamFailure{misconfigured: true}
	}
	result, err := a.doubao.submitAndPoll(c.Request.Context(), key, audioURL, doubaoASROptions{
		Format: doubaoAudioFormat(form.fileName),
	})
	if err != nil {
		return false, doubaoToUpstreamFailure(err)
	}

	c.JSON(http.StatusOK, gin.H{"text": result.Text})
	if meter != nil {
		meter.WriteUsage(c, apiKey, UsageInput{
			Lane:             LaneAudio,
			Model:            "audio:stt",
			UpstreamModel:    "doubao:bigmodel",
			RequestID:        laneRequestID(c),
			Account:          account,
			Audio:            &service.AudioUsage{Mode: "stt", DurationOrUnits: result.DurationSecond / 3600},
			Duration:         time.Since(started),
			UpstreamEndpoint: "/v3/auc/bigmodel/submit",
		})
	}
	return true, nil
}

// doubaoToUpstreamFailure 把豆包错误映射成车道统一的失败事实。
func doubaoToUpstreamFailure(err error) *upstreamFailure {
	var de *doubaoUpstreamError
	if !asDoubaoError(err, &de) {
		return &upstreamFailure{status: 0}
	}
	return &upstreamFailure{
		status:      de.status,
		bodyExcerpt: de.message,
	}
}

func asDoubaoError(err error, target **doubaoUpstreamError) bool {
	if de, ok := err.(*doubaoUpstreamError); ok {
		*target = de
		return true
	}
	return false
}

// doubaoAudioFormat 由上传文件名推断上游要求的格式串。
func doubaoAudioFormat(fileName string) string {
	lower := strings.ToLower(fileName)
	if i := strings.LastIndex(lower, "."); i >= 0 && i+1 < len(lower) {
		switch f := lower[i+1:]; f {
		case "wav", "mp3", "ogg", "spx", "amr", "aac", "m4a":
			return f
		}
	}
	return "wav"
}

// 确保 logger 引用不因后续删减而未用（分派层会记录协议命中）。
var _ = logger.L
var _ = zap.String
var _ = hex.EncodeToString
var _ = context.Background
