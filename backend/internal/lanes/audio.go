package lanes

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/Wei-Shaw/sub2api/internal/service"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
)

// 硅基流动 audio 车道常量（tasks 3.2）。ASR/TTS 上游形状=OpenAI 兼容
// （transcriptions=multipart、speech=JSON+二进制响应），上游差异吞在适配器内。
const (
	siliconflowAudioTranscriptionsPath = "/v1/audio/transcriptions"
	siliconflowAudioSpeechPath         = "/v1/audio/speech"
	audioDefaultSTTModel               = "FunAudioLLM/SenseVoiceSmall"
	audioDefaultTTSModel               = "FunAudioLLM/CosyVoice2-0.5B"
	audioDefaultTTSVoice               = "FunAudioLLM/CosyVoice2-0.5B:alex"
	// audioMaxUploadBytes v1 内存缓冲上限（multipart 重建需要整体读入；
	// 后续如需超大文件可改流式 pipe 重建）。
	audioMaxUploadBytes = 100 << 20
	// audioASRTextBodyLimit 上游 verbose_json 响应上限（含 segments 时分段多）。
	audioASRResponseLimit = 32 << 20
)

// audioSupportedASRFormats 调用者可选的转写响应形状（v1）：
// json={"text":...}（默认）、text=纯文本、verbose_json=上游原样。
var audioSupportedASRFormats = []string{"json", "text", "verbose_json"}

// AudioAdapter 语音车道适配器：POST /v1/audio/transcriptions（ASR，multipart）
// 与 POST /v1/audio/speech（TTS，JSON→二进制）。计量：ASR 按音频时长（小时）、
// TTS 按合成字符数（百万字符）——复用 Group 既有音频三价字段。
type AudioAdapter struct {
	client *http.Client
}

func NewAudioAdapter() *AudioAdapter {
	return &AudioAdapter{client: &http.Client{Timeout: 120 * time.Second}}
}

func (a *AudioAdapter) Serve(c *gin.Context, apiKey *service.APIKey, account *service.Account, meter *Meter) {
	switch c.FullPath() {
	case "/v1/audio/transcriptions":
		a.serveTranscriptions(c, apiKey, account, meter)
	case "/v1/audio/speech":
		a.serveSpeech(c, apiKey, account, meter)
	default:
		notImplemented(c, LaneAudio)
	}
}

// laneUpstreamHTTPError 上游非 2xx 分级：4xx（400/422=内容/参数问题，多半是
// 调用者上传内容）→ 400；其余（401/403/429/5xx=上游侧故障）→ 502。
// 均不外泄上游响应文本（可能含账户信息）、不计费。
func laneUpstreamHTTPError(c *gin.Context, lane string, upstreamStatus int) {
	if upstreamStatus == http.StatusBadRequest || upstreamStatus == http.StatusUnprocessableEntity {
		laneBadRequest(c, lane, fmt.Sprintf("%s lane upstream rejected the request content (status %d)", lane, upstreamStatus))
		return
	}
	laneUpstreamError(c, lane, upstreamStatus)
}

func laneUpstreamBaseURL(account *service.Account) string {
	baseURL, _ := account.Credentials["base_url"].(string)
	if strings.TrimSpace(baseURL) == "" {
		baseURL = siliconflowDefaultBaseURL
	}
	return strings.TrimRight(baseURL, "/")
}

// ── ASR：/v1/audio/transcriptions ─────────────────────────────────────────────

// asrFormData 解析后的入站 multipart：file 保留原文件名+内容，其余字段原文。
type asrFormData struct {
	fileName       string
	fileBytes      []byte
	model          string
	responseFormat string // 调用者所求（默认 json）
	extraFields    map[string]string
}

func parseASRMultipart(c *gin.Context, body []byte) (*asrFormData, int, error) {
	mediaType, params, err := mime.ParseMediaType(c.GetHeader("Content-Type"))
	if err != nil || !strings.HasPrefix(mediaType, "multipart/") {
		return nil, http.StatusBadRequest, fmt.Errorf("Content-Type must be multipart/form-data")
	}
	boundary := params["boundary"]
	if boundary == "" {
		return nil, http.StatusBadRequest, fmt.Errorf("multipart boundary missing")
	}
	form := &asrFormData{responseFormat: "json", extraFields: map[string]string{}}
	reader := multipart.NewReader(bytes.NewReader(body), boundary)
	for {
		part, err := reader.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, http.StatusBadRequest, fmt.Errorf("malformed multipart body")
		}
		name := part.FormName()
		switch name {
		case "file":
			if form.fileBytes != nil {
				part.Close()
				return nil, http.StatusBadRequest, fmt.Errorf("field `file` must appear exactly once")
			}
			limited := io.LimitReader(part, audioMaxUploadBytes+1)
			data, err := io.ReadAll(limited)
			part.Close()
			if err != nil {
				return nil, http.StatusBadRequest, fmt.Errorf("failed to read uploaded file")
			}
			if len(data) > audioMaxUploadBytes {
				return nil, http.StatusRequestEntityTooLarge, fmt.Errorf("uploaded file exceeds %d bytes", audioMaxUploadBytes)
			}
			form.fileBytes = data
			form.fileName = part.FileName()
			if form.fileName == "" {
				form.fileName = "audio"
			}
		case "model":
			v, _ := io.ReadAll(io.LimitReader(part, 512))
			form.model = strings.TrimSpace(string(v))
			part.Close()
		case "response_format":
			v, _ := io.ReadAll(io.LimitReader(part, 64))
			form.responseFormat = strings.ToLower(strings.TrimSpace(string(v)))
			part.Close()
		default:
			v, _ := io.ReadAll(io.LimitReader(part, 64<<10))
			form.extraFields[name] = string(v)
			part.Close()
		}
	}
	if form.fileBytes == nil {
		return nil, http.StatusBadRequest, fmt.Errorf("field `file` is required (multipart/form-data)")
	}
	return form, 0, nil
}

func buildASRUpstreamBody(form *asrFormData) ([]byte, string, error) {
	var buf bytes.Buffer
	writer := multipart.NewWriter(&buf)
	filePart, err := writer.CreateFormFile("file", form.fileName)
	if err != nil {
		return nil, "", err
	}
	if _, err := filePart.Write(form.fileBytes); err != nil {
		return nil, "", err
	}
	model := form.model
	if model == "" {
		model = audioDefaultSTTModel
	}
	if err := writer.WriteField("model", model); err != nil {
		return nil, "", err
	}
	// 内部强制 verbose_json 以向上游取音频时长（计费依据）；对调用者的回形
	// 由 response_format 另行转换，见 writeASRResponse。
	if err := writer.WriteField("response_format", "verbose_json"); err != nil {
		return nil, "", err
	}
	for k, v := range form.extraFields {
		if k == "response_format" {
			continue
		}
		if err := writer.WriteField(k, v); err != nil {
			return nil, "", err
		}
	}
	if err := writer.Close(); err != nil {
		return nil, "", err
	}
	return buf.Bytes(), writer.FormDataContentType(), nil
}

// asrVerboseResponse 上游 verbose_json 形状（duration 秒=计费依据）。
type asrVerboseResponse struct {
	Text     string  `json:"text"`
	Duration float64 `json:"duration"`
	Language string  `json:"language,omitempty"`
}

// wavDurationSeconds 兜底时长解析（RIFF/WAVE）：byteRate @28、data chunk 尺寸。
// 非 wav 或解析失败返回 0（真 0 计费 + 告警，宁少勿虚计）。
func wavDurationSeconds(data []byte) float64 {
	if len(data) < 44 || string(data[0:4]) != "RIFF" || string(data[8:12]) != "WAVE" {
		return 0
	}
	byteRate := binary.LittleEndian.Uint32(data[28:32])
	if byteRate == 0 {
		return 0
	}
	// 找 data chunk（从 12 起按 chunk 遍历）。
	offset := 12
	for offset+8 <= len(data) {
		chunkID := string(data[offset : offset+4])
		chunkSize := int(binary.LittleEndian.Uint32(data[offset+4 : offset+8]))
		if chunkID == "data" {
			available := len(data) - offset - 8
			if chunkSize < available {
				available = chunkSize
			}
			return float64(available) / float64(byteRate)
		}
		offset += 8 + chunkSize + chunkSize%2
	}
	return 0
}

func writeASRResponse(c *gin.Context, form *asrFormData, raw []byte, verbose asrVerboseResponse) {
	switch form.responseFormat {
	case "text":
		c.Data(http.StatusOK, "text/plain; charset=utf-8", []byte(verbose.Text))
	case "verbose_json":
		c.Data(http.StatusOK, "application/json", raw)
	default: // json
		c.JSON(http.StatusOK, gin.H{"text": verbose.Text})
	}
}

func (a *AudioAdapter) serveTranscriptions(c *gin.Context, apiKey *service.APIKey, account *service.Account, meter *Meter) {
	started := time.Now()
	body, err := io.ReadAll(io.LimitReader(c.Request.Body, audioMaxUploadBytes+1))
	if err != nil {
		laneBadRequest(c, LaneAudio, "failed to read request body")
		return
	}
	if len(body) > audioMaxUploadBytes {
		c.JSON(http.StatusRequestEntityTooLarge, gin.H{
			"error": gin.H{
				"type":    "lane_payload_too_large",
				"message": fmt.Sprintf("audio upload exceeds %d bytes", audioMaxUploadBytes),
				"lane":    LaneAudio,
			},
		})
		return
	}
	form, status, err := parseASRMultipart(c, body)
	if err != nil {
		c.JSON(status, gin.H{"error": gin.H{"type": "invalid_request_error", "message": err.Error(), "lane": LaneAudio}})
		return
	}
	supported := false
	for _, f := range audioSupportedASRFormats {
		if f == form.responseFormat {
			supported = true
			break
		}
	}
	if !supported {
		laneBadRequest(c, LaneAudio, fmt.Sprintf("unsupported response_format %q; supported: %s", form.responseFormat, strings.Join(audioSupportedASRFormats, ", ")))
		return
	}
	apiKeyStr, _ := account.Credentials["api_key"].(string)
	if apiKeyStr == "" {
		laneUpstreamError(c, LaneAudio, 0)
		return
	}

	upstreamBody, contentType, err := buildASRUpstreamBody(form)
	if err != nil {
		laneBadRequest(c, LaneAudio, "failed to serialize upstream request")
		return
	}
	httpReq, err := http.NewRequestWithContext(c.Request.Context(), http.MethodPost, laneUpstreamBaseURL(account)+siliconflowAudioTranscriptionsPath, bytes.NewReader(upstreamBody))
	if err != nil {
		laneUpstreamError(c, LaneAudio, 0)
		return
	}
	httpReq.Header.Set("Authorization", "Bearer "+apiKeyStr)
	httpReq.Header.Set("Content-Type", contentType)

	resp, err := a.client.Do(httpReq)
	if err != nil {
		laneUpstreamError(c, LaneAudio, 0)
		return
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, audioASRResponseLimit))
	if err != nil {
		laneUpstreamError(c, LaneAudio, resp.StatusCode)
		return
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		laneUpstreamHTTPError(c, LaneAudio, resp.StatusCode)
		return
	}
	var verbose asrVerboseResponse
	if err := json.Unmarshal(respBody, &verbose); err != nil {
		laneUpstreamError(c, LaneAudio, resp.StatusCode)
		return
	}
	// 时长=计费依据：上游 verbose_json.duration 优先；缺则 wav 头兜底；再缺
	// 记 0 并告警（宁少勿虚计，观测位）。
	durationSeconds := verbose.Duration
	if durationSeconds <= 0 {
		if d := wavDurationSeconds(form.fileBytes); d > 0 {
			durationSeconds = d
		} else {
			logger.L().With(
				zap.String("component", "lanes.audio.stt"),
				zap.String("engine", "siliconflow"),
			).Warn("lanes.audio.stt_duration_unavailable_bill_zero")
		}
	}

	writeASRResponse(c, form, respBody, verbose)

	if meter != nil {
		model := form.model
		if model == "" {
			model = audioDefaultSTTModel
		}
		meter.WriteUsage(c, apiKey, UsageInput{
			Lane:             LaneAudio,
			Model:            "audio:stt",
			UpstreamModel:    "siliconflow:" + model,
			RequestID:        laneRequestID(c),
			Account:          account,
			Audio:            &service.AudioUsage{Mode: "stt", DurationOrUnits: durationSeconds / 3600},
			Duration:         time.Since(started),
			UpstreamEndpoint: siliconflowAudioTranscriptionsPath,
		})
	}
}

// ── TTS：/v1/audio/speech ─────────────────────────────────────────────────────

func (a *AudioAdapter) serveSpeech(c *gin.Context, apiKey *service.APIKey, account *service.Account, meter *Meter) {
	started := time.Now()
	body, err := io.ReadAll(io.LimitReader(c.Request.Body, 4<<20))
	if err != nil {
		laneBadRequest(c, LaneAudio, "failed to read request body")
		return
	}
	var req map[string]any
	if err := json.Unmarshal(body, &req); err != nil {
		laneBadRequest(c, LaneAudio, "request body must be a JSON object ({\"input\",\"voice\",\"model\"})")
		return
	}
	input, _ := req["input"].(string)
	if strings.TrimSpace(input) == "" {
		laneBadRequest(c, LaneAudio, "field `input` is required and must be a non-empty string")
		return
	}
	voice, _ := req["voice"].(string)
	if strings.TrimSpace(voice) == "" {
		// 上游 voice 名与模型绑定；v1 未传时用硅基 CosyVoice 默认音色。
		req["voice"] = audioDefaultTTSVoice
	}
	model, _ := req["model"].(string)
	if strings.TrimSpace(model) == "" {
		model = audioDefaultTTSModel
		req["model"] = model
	}
	if _, ok := req["response_format"]; !ok {
		req["response_format"] = "mp3"
	}
	apiKeyStr, _ := account.Credentials["api_key"].(string)
	if apiKeyStr == "" {
		laneUpstreamError(c, LaneAudio, 0)
		return
	}
	upstreamBody, err := json.Marshal(req)
	if err != nil {
		laneBadRequest(c, LaneAudio, "failed to serialize request")
		return
	}
	httpReq, err := http.NewRequestWithContext(c.Request.Context(), http.MethodPost, laneUpstreamBaseURL(account)+siliconflowAudioSpeechPath, bytes.NewReader(upstreamBody))
	if err != nil {
		laneUpstreamError(c, LaneAudio, 0)
		return
	}
	httpReq.Header.Set("Authorization", "Bearer "+apiKeyStr)
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := a.client.Do(httpReq)
	if err != nil {
		laneUpstreamError(c, LaneAudio, 0)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10)) // 排空不计费
		laneUpstreamHTTPError(c, LaneAudio, resp.StatusCode)
		return
	}
	contentType := resp.Header.Get("Content-Type")
	if contentType == "" {
		contentType = "audio/mpeg"
	}
	audioBytes, err := io.ReadAll(io.LimitReader(resp.Body, audioMaxUploadBytes))
	if err != nil {
		laneUpstreamError(c, LaneAudio, resp.StatusCode)
		return
	}
	c.Data(http.StatusOK, contentType, audioBytes)

	if meter != nil {
		chars := utf8.RuneCountInString(input)
		meter.WriteUsage(c, apiKey, UsageInput{
			Lane:             LaneAudio,
			Model:            "audio:tts",
			UpstreamModel:    "siliconflow:" + model,
			RequestID:        laneRequestID(c),
			Account:          account,
			Audio:            &service.AudioUsage{Mode: "tts", DurationOrUnits: float64(chars) / 1_000_000},
			Duration:         time.Since(started),
			UpstreamEndpoint: siliconflowAudioSpeechPath,
		})
	}
}
