package lanes

import (
	"bytes"
	"context"
	"crypto/hmac"
	"errors"
	"crypto/sha256"
	"encoding/base64"
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
	"go.uber.org/zap"
)

// 腾讯云语音（云 API 3.0）上游适配器。
//
// 与豆包的差别：这里是**标准 REST**（POST + JSON），协议简单的多；真正的门槛
// 是 TC3-HMAC-SHA256 签名与错误面映射。
//
// 实测判定（2026-10-18）：用户提供的凭据 SecretId 有效、SecretKey 不完整
// （29 字符，标准 32），真实 key 与随机伪造 key 返回相同 AuthFailure.SignatureFailure。
// 因此本适配器的签名路径以「与官方算法一致」为目标，由金样测试锁定；
// 真机验收（4.x）须待凭据补齐。
const (
	tencentTTSEndpoint   = "tts.tencentcloudapi.com"
	tencentASREndpoint   = "asr.tencentcloudapi.com"
	tencentTTSAction     = "TextToVoice"
	tencentTTSVersion    = "2019-08-23"
	tencentASRAction     = "SentenceRecognition"
	tencentASRVersion    = "2019-06-14"
	tencentDefaultRegion = "ap-guangzhou"
	tencentDefaultVoice  = 101007 // 实测可用音色
	tencentServiceTTS    = "tts"
	tencentServiceASR    = "asr"
)

// tencentAdapter 腾讯云上游能力实现。
type tencentAdapter struct {
	httpClient *http.Client
	// 端点可注入（测试用）。
	ttsEndpoint string
	asrEndpoint string
	// now 供签名测试固定时间戳。
	now func() time.Time
}

func newTencentAdapter() *tencentAdapter {
	return &tencentAdapter{
		httpClient:  &http.Client{Timeout: 60 * time.Second},
		ttsEndpoint: tencentTTSEndpoint,
		asrEndpoint: tencentASREndpoint,
		now:         time.Now,
	}
}

// tencentCredentials 账号凭据形态。
type tencentCredentials struct {
	SecretID  string
	SecretKey string
	Token     string // 临时凭据需要；永久密钥为空
	Region    string
}

// tencentCredentialsFrom 从账号凭据解析腾讯云凭据。
func tencentCredentialsFrom(creds map[string]any) tencentCredentials {
	c := tencentCredentials{
		SecretID:  mapString(creds, "secret_id"),
		SecretKey: mapString(creds, "secret_key"),
		Token:     mapString(creds, "token"),
		Region:    mapString(creds, "region"),
	}
	if c.Region == "" {
		c.Region = tencentDefaultRegion
	}
	return c
}

// mapString 取 map 里的字符串值并去空白。
func mapString(m map[string]any, key string) string {
	if v, ok := m[key]; ok {
		if s, ok := v.(string); ok {
			return strings.TrimSpace(s)
		}
	}
	return ""
}

// tencentTC3 计算 TC3-HMAC-SHA256 签名（腾讯云云 API 3.0 标准流程）。
//
// 步骤（官方文档）：
//  1. canonicalRequest = METHOD\nURI\nQUERY\ncanonicalHeaders\nsignedHeaders\nhashedPayload
//  2. stringToSign = "TC3-HMAC-SHA256"\nTimestamp\nDate/Service/tc3_request\nhash(canonicalRequest)
//  3. SecretDate = HMAC("TC3"+SecretKey, Date)
//     SecretService = HMAC(SecretDate, Service)
//     SecretSigning = HMAC(SecretService, "tc3_request")
//     Signature = hex(HMAC(SecretSigning, stringToSign))
func tencentTC3(secretID, secretKey, service, host, payload string, ts time.Time) string {
	date := ts.UTC().Format("2006-01-02")
	timestamp := ts.Unix()
	contentType := "application/json; charset=utf-8"

	hashedPayload := sha256Hex(payload)
	canonicalRequest := strings.Join([]string{
		"POST",
		"/",
		"",
		"content-type:" + contentType + "\n" + "host:" + host + "\n",
		"content-type;host",
		hashedPayload,
	}, "\n")

	stringToSign := strings.Join([]string{
		"TC3-HMAC-SHA256",
		fmt.Sprintf("%d", timestamp),
		date + "/" + service + "/tc3_request",
		sha256Hex(canonicalRequest),
	}, "\n")

	secretDate := hmacSHA256([]byte("TC3"+secretKey), date)
	secretService := hmacSHA256(secretDate, service)
	secretSigning := hmacSHA256(secretService, "tc3_request")
	signature := hex.EncodeToString(hmacSHA256(secretSigning, stringToSign))

	return fmt.Sprintf(
		"TC3-HMAC-SHA256 Credential=%s/%s/%s/tc3_request, SignedHeaders=content-type;host, Signature=%s",
		secretID, date, service, signature,
	)
}

func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func hmacSHA256(key []byte, data string) []byte {
	h := hmac.New(sha256.New, key)
	_, _ = h.Write([]byte(data))
	return h.Sum(nil)
}

// tencentCall 发起一次云 API 调用并解出 Response 段。
func (a *tencentAdapter) tencentCall(ctx context.Context, cred tencentCredentials, endpoint, service, action, version string, payload any) (map[string]any, error) {
	if strings.TrimSpace(cred.SecretID) == "" || strings.TrimSpace(cred.SecretKey) == "" {
		return nil, errTencentMissingCredential
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("marshal payload: %w", err)
	}
	ts := a.now()
	auth := tencentTC3(cred.SecretID, cred.SecretKey, service, endpoint, string(body), ts)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://"+endpoint+"/", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Host = endpoint
	req.Header.Set("Authorization", auth)
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	req.Header.Set("X-TC-Action", action)
	req.Header.Set("X-TC-Version", version)
	req.Header.Set("X-TC-Region", cred.Region)
	req.Header.Set("X-TC-Timestamp", fmt.Sprintf("%d", ts.Unix()))
	if cred.Token != "" {
		req.Header.Set("X-TC-Token", cred.Token)
	}

	resp, err := a.httpClient.Do(req)
	if err != nil {
		return nil, &tencentUpstreamError{status: 0, err: err}
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return nil, &tencentUpstreamError{status: resp.StatusCode, err: err}
	}

	var envelope struct {
		Response struct {
			Error *struct {
				Code    string `json:"Code"`
				Message string `json:"Message"`
			} `json:"Error"`
			RequestID string `json:"RequestId"`
		} `json:"Response"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return nil, &tencentUpstreamError{status: resp.StatusCode, err: err}
	}
	if envelope.Response.Error != nil {
		return nil, &tencentUpstreamError{
			status:  resp.StatusCode,
			code:    envelope.Response.Error.Code,
			message: envelope.Response.Error.Message,
		}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, &tencentUpstreamError{status: resp.StatusCode}
	}

	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, &tencentUpstreamError{status: resp.StatusCode, err: err}
	}
	if r, ok := out["Response"].(map[string]any); ok {
		return r, nil
	}
	return nil, &tencentUpstreamError{status: resp.StatusCode, message: "malformed response envelope"}
}

// synthesize TTS：TextToVoice 返回 Base64 音频，解码后下发。
func (a *tencentAdapter) synthesize(ctx context.Context, cred tencentCredentials, text string, voiceType int, format string, sampleRate int) ([]byte, error) {
	if voiceType <= 0 {
		voiceType = tencentDefaultVoice
	}
	if format == "" {
		format = "mp3"
	}
	sampleRate = tencentDefaultSampleRate(sampleRate)
	params := map[string]any{
		"Text":       text,
		"SessionId":  newUUIDLike(),
		"VoiceType":  voiceType,
		"Codec":      format,
		"SampleRate": sampleRate,
		"ProjectId":  0,
	}
	resp, err := a.tencentCall(ctx, cred, a.ttsEndpoint, tencentServiceTTS, tencentTTSAction, tencentTTSVersion, params)
	if err != nil {
		// 音色不支持该采样率时降级重试一次（调用者显式指定的值可能与音色冲突；
		// 与其把请求打死，不如按上游能力回退到通用值）。
		var te *tencentUpstreamError
		if errors.As(err, &te) && strings.Contains(te.code, "InvalidParameterValue.SampleRate") && sampleRate != 16000 {
			logger.L().Warn("lanes.audio.tencent.tts_samplerate_fallback",
				zap.Int("requested", sampleRate), zap.Int("voice_type", voiceType))
			params["SampleRate"] = 16000
			params["SessionId"] = newUUIDLike()
			resp, err = a.tencentCall(ctx, cred, a.ttsEndpoint, tencentServiceTTS, tencentTTSAction, tencentTTSVersion, params)
		}
		if err != nil {
			return nil, err
		}
	}
	audioB64, _ := resp["Audio"].(string)
	if strings.TrimSpace(audioB64) == "" {
		return nil, &tencentUpstreamError{status: 0, message: "empty audio in response"}
	}
	// 上游返回 Base64，必须解码后下发（不能把 Base64 当二进制写）。
	audio, err := base64.StdEncoding.DecodeString(audioB64)
	if err != nil {
		return nil, &tencentUpstreamError{status: 0, err: fmt.Errorf("decode audio base64: %w", err)}
	}
	return audio, nil
}

// transcribe 一句话识别（同步）：≤60s、≤3MB。
func (a *tencentAdapter) transcribe(ctx context.Context, cred tencentCredentials, audioBytes []byte, voiceFormat string) (string, error) {
	if len(audioBytes) == 0 {
		return "", &tencentUpstreamError{status: 0, message: "no audio bytes"}
	}
	params := map[string]any{
		"EngSerViceType": "16k_zh",
		"SourceType":     1, // 1=本地数据（Base64）
		"VoiceFormat":    voiceFormat,
		"Data":           base64.StdEncoding.EncodeToString(audioBytes),
		"DataLen":        len(audioBytes),
	}
	resp, err := a.tencentCall(ctx, cred, a.asrEndpoint, tencentServiceASR, tencentASRAction, tencentASRVersion, params)
	if err != nil {
		return "", err
	}
	text, _ := resp["Result"].(string)
	return text, nil
}

// ── 错误与抽象 ────────────────────────────────────────────────────────────────

type tencentUpstreamError struct {
	status  int
	code    string
	message string
	err     error
}

func (e *tencentUpstreamError) Error() string {
	if e.err != nil {
		return fmt.Sprintf("tencent upstream error (status %d): %v", e.status, e.err)
	}
	return fmt.Sprintf("tencent upstream error (status %d, code %s): %s", e.status, e.code, e.message)
}

var errTencentMissingCredential = fmt.Errorf("tencent account missing secret_id/secret_key")

// tencentToUpstreamFailure 映射到车道分级判据。
func tencentToUpstreamFailure(err error) *upstreamFailure {
	if te, ok := err.(*tencentUpstreamError); ok {
		status := te.status
		// 云 API 的业务错误常以 HTTP 200 + Response.Error 返回：此时 te.status
		// 是 200，直接下发会被分级判据当成「上游正常却失败」的怪状态（曾写出
		// `status 200` 这种冷却原因，且真实错误码被吞、无法定位）。按业务码
		// 归类到可读的 HTTP 语义。
		if te.code != "" {
			switch {
			case strings.HasPrefix(te.code, "AuthFailure"):
				status = http.StatusForbidden
			case strings.HasPrefix(te.code, "InvalidParameter"), strings.HasPrefix(te.code, "InvalidParameterValue"):
				status = http.StatusBadRequest
			case strings.HasPrefix(te.code, "LimitExceeded"), strings.HasPrefix(te.code, "RequestLimitExceeded"):
				status = http.StatusTooManyRequests
			case strings.HasPrefix(te.code, "FailedOperation"), strings.HasPrefix(te.code, "InternalError"):
				status = http.StatusBadGateway
			case status == http.StatusOK:
				// 其他业务错误：归入上游侧故障（502），避免 200 这种无判别力状态。
				status = http.StatusBadGateway
			}
			// 上游错误码入 WARN：这是定位「腾讯 TTS 为何不通」的唯一线索，
			// 不能只留在分级判据的 bodyExcerpt 里。
			logger.L().Warn("lanes.audio.tencent.upstream_error",
				zap.String("code", te.code),
				zap.Int("http_status", te.status),
				zap.String("message", firstN(te.message, 200)),
			)
		}
		if status == 0 {
			status = http.StatusBadGateway
		}
		return &upstreamFailure{status: status, bodyExcerpt: te.code + " " + te.message}
	}
	return &upstreamFailure{status: http.StatusBadGateway}
}

// ── 绑定到车道适配器（audio.go 分派层调用）─────────────────────────────────────

// serveTencentTTS 腾讯云 TTS 的一次尝试。
func (a *AudioAdapter) serveTencentTTS(c *gin.Context, apiKey *service.APIKey, account *service.Account,
	meter *Meter, started time.Time, input, voice, format string, sampleRate int) (bool, *upstreamFailure) {

	creds := tencentCredentialsFrom(account.Credentials)
	if creds.SecretID == "" || creds.SecretKey == "" {
		return false, &upstreamFailure{misconfigured: true}
	}
	// 对外 voice 是字符串（OpenAI 形状），腾讯云要整数 VoiceType；
	// 非数字时回落默认音色（调用者不必知道上游音色编号）。
	voiceType := tencentDefaultVoice
	if n, ok := parsePositiveInt(voice); ok {
		voiceType = n
	}

	audio, err := a.tencent.synthesize(c.Request.Context(), creds, input, voiceType, format, sampleRate)
	if err != nil {
		return false, tencentToUpstreamFailure(err)
	}
	c.Data(http.StatusOK, "audio/mpeg", audio)

	if meter != nil {
		chars := utf8.RuneCountInString(input)
		meter.WriteUsage(c, apiKey, UsageInput{
			Lane:             LaneAudio,
			Model:            "audio:tts",
			UpstreamModel:    fmt.Sprintf("tencent:%d", voiceType),
			RequestID:        laneRequestID(c),
			Account:          account,
			Audio:            &service.AudioUsage{Mode: "tts", DurationOrUnits: float64(chars) / 1_000_000},
			Duration:         time.Since(started),
			UpstreamEndpoint: "/" + tencentTTSAction,
		})
	}
	return true, nil
}

// serveTencentASR 腾讯云一句话识别的一次尝试（同步）。
func (a *AudioAdapter) serveTencentASR(c *gin.Context, apiKey *service.APIKey, account *service.Account,
	meter *Meter, started time.Time, form *asrFormData) (bool, *upstreamFailure) {

	creds := tencentCredentialsFrom(account.Credentials)
	if creds.SecretID == "" || creds.SecretKey == "" {
		return false, &upstreamFailure{misconfigured: true}
	}
	voiceFormat := strings.TrimPrefix(audioFileExt(form.fileName), ".")
	text, err := a.tencent.transcribe(c.Request.Context(), creds, form.fileBytes, voiceFormat)
	if err != nil {
		return false, tencentToUpstreamFailure(err)
	}

	// 一句话识别不返回时长：按 wav 头估算，缺失则记 0（宁少勿虚计）。
	durationSeconds := wavDurationSeconds(form.fileBytes)
	if durationSeconds <= 0 {
		logger.L().With(
			zap.String("component", "lanes.audio.stt"),
			zap.String("engine", "tencent"),
		).Warn("lanes.audio.stt_duration_unavailable_bill_zero")
	}
	writeASRTextResponse(c, form, text)
	if meter != nil {
		meter.WriteUsage(c, apiKey, UsageInput{
			Lane:             LaneAudio,
			Model:            "audio:stt",
			UpstreamModel:    "tencent:sentence",
			RequestID:        laneRequestID(c),
			Account:          account,
			Audio:            &service.AudioUsage{Mode: "stt", DurationOrUnits: durationSeconds / 3600},
			Duration:         time.Since(started),
			UpstreamEndpoint: "/" + tencentASRAction,
		})
	}
	return true, nil
}

// writeASRTextResponse 按调用者所求形状写转写结果（json / text）。
func writeASRTextResponse(c *gin.Context, form *asrFormData, text string) {
	switch form.responseFormat {
	case "text":
		c.Data(http.StatusOK, "text/plain; charset=utf-8", []byte(text))
	case "verbose_json":
		c.JSON(http.StatusOK, gin.H{"text": text})
	default:
		c.JSON(http.StatusOK, gin.H{"text": text})
	}
}

// parsePositiveInt 解析正整数字符串。
func parsePositiveInt(s string) (int, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, false
	}
	n := 0
	for _, r := range s {
		if r < '0' || r > '9' {
			return 0, false
		}
		n = n*10 + int(r-'0')
	}
	if n <= 0 {
		return 0, false
	}
	return n, true
}

// tencentDefaultSampleRate 归一化采样率：<=0 表示调用者未指定，取腾讯云通用值
// 16000。腾讯云音色的采样率能力按音色不同（实测 VoiceType=101007 支持 16000、
// 不支持 24000），因此**不能**沿用豆包的 24000 默认。
func tencentDefaultSampleRate(rate int) int {
	if rate <= 0 {
		return 16000
	}
	return rate
}
