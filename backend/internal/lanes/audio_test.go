package lanes

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/domain"
	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// audio 契约（tasks 3.2）：ASR multipart 重建+verbose_json 强制取时长+调用者
// 回形三态+wav 兜底；TTS 默认音色/模型+二进制透传+按字符计量；错误路径不计费。

type audioUpstreamCapture struct {
	mu           sync.Mutex
	path         string
	method       string
	authHeader   string
	contentType  string
	body         []byte
	jsonBody     map[string]any
	responseCode int
	responseBody []byte
	responseType string
}

func newAudioFakeUpstream(t *testing.T, capture *audioUpstreamCapture) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		capture.mu.Lock()
		capture.path = r.URL.Path
		capture.method = r.Method
		capture.authHeader = r.Header.Get("Authorization")
		capture.contentType = r.Header.Get("Content-Type")
		capture.body = body
		capture.jsonBody = nil
		if strings.Contains(capture.contentType, "application/json") {
			var parsed map[string]any
			if err := json.Unmarshal(body, &parsed); err == nil {
				capture.jsonBody = parsed
			}
		}
		code := capture.responseCode
		respBody := capture.responseBody
		respType := capture.responseType
		capture.mu.Unlock()

		if r.URL.Path == siliconflowAudioSpeechPath {
			if code == 0 {
				code = http.StatusOK
				respType = "audio/mpeg"
				respBody = []byte("ID3fake-mp3-bytes")
			}
			w.Header().Set("Content-Type", respType)
			w.WriteHeader(code)
			_, _ = w.Write(respBody)
			return
		}
		// ASR：默认 verbose_json 成功（duration=12 秒=计费依据）；调用方已给
		// responseBody 时不覆盖（否则自定响应被默认件吞掉）。
		if code == 0 {
			code = http.StatusOK
			if len(respBody) == 0 {
				respType = "application/json"
				respBody = []byte(`{"text":"你好，壹座","duration":12,"language":"zh"}`)
			}
		}
		w.Header().Set("Content-Type", respType)
		w.WriteHeader(code)
		_, _ = w.Write(respBody)
	}))
	t.Cleanup(server.Close)
	return server
}

func newAudioTestApp(upstream *httptest.Server, recorded chan *service.OpenAIRecordUsageInput) *Lanes {
	lister := &fakeLaneAccountLister{accounts: []service.Account{
		{ID: 41, Platform: domain.PlatformOpenAINext, Status: service.StatusActive, Priority: 1,
			Credentials: map[string]any{
				"api_key":  "sk-audio-test",
				"base_url": upstream.URL,
			}},
	}}
	meter := &Meter{recordUsage: func(_ context.Context, input *service.OpenAIRecordUsageInput) error {
		recorded <- input
		return nil
	}}
	return &Lanes{
		Meter: meter,
		Pool:  NewPool(lister, nil),
		adapters: map[string]LaneAdapter{
			LaneAudio: NewAudioAdapter(),
		},
	}
}

func serveAudio(t *testing.T, app *Lanes, path string, body []byte, contentType string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, path, bytesReader(body))
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	router := gin.New()
	group := router.Group("/v1")
	group.Use(func(c *gin.Context) {
		c.Set(string(middleware.ContextKeyAPIKey), &service.APIKey{
			ID: 100, User: &service.User{ID: 7}, Group: &service.Group{ID: 42, Platform: service.PlatformOpenAI},
		})
		c.Next()
	})
	RegisterGatewayLanes(group, app)
	router.ServeHTTP(w, req)
	return w
}

func bytesReader(b []byte) io.Reader {
	if b == nil {
		return nil
	}
	return strings.NewReader(string(b))
}

// buildASRMultipart 构造入站 multipart（file 字段+可选字段）。
func buildASRMultipart(t *testing.T, fileName string, fileBytes []byte, fields map[string]string) ([]byte, string) {
	t.Helper()
	var buf strings.Builder
	writer := multipart.NewWriter(&buf)
	part, err := writer.CreateFormFile("file", fileName)
	require.NoError(t, err)
	_, err = part.Write(fileBytes)
	require.NoError(t, err)
	for k, v := range fields {
		require.NoError(t, writer.WriteField(k, v))
	}
	require.NoError(t, writer.Close())
	return []byte(buf.String()), writer.FormDataContentType()
}

// minimalWAV 构造 1 秒 16kHz 单声道 16bit WAV（byteRate=32000）。
func minimalWAV(seconds int) []byte {
	const byteRate = 32000
	dataLen := seconds * byteRate
	out := make([]byte, 44+dataLen)
	copy(out[0:4], "RIFF")
	binary.LittleEndian.PutUint32(out[4:8], uint32(36+dataLen))
	copy(out[8:12], "WAVE")
	copy(out[12:16], "fmt ")
	binary.LittleEndian.PutUint32(out[16:20], 16)
	binary.LittleEndian.PutUint16(out[20:22], 1)
	binary.LittleEndian.PutUint16(out[22:24], 1)
	binary.LittleEndian.PutUint32(out[24:28], 16000)
	binary.LittleEndian.PutUint32(out[28:32], byteRate)
	binary.LittleEndian.PutUint16(out[32:34], 2)
	binary.LittleEndian.PutUint16(out[34:36], 16)
	copy(out[36:40], "data")
	binary.LittleEndian.PutUint32(out[40:44], uint32(dataLen))
	return out
}

func TestAudioLaneASRHappyPath(t *testing.T) {
	capture := &audioUpstreamCapture{}
	upstream := newAudioFakeUpstream(t, capture)
	recorded := make(chan *service.OpenAIRecordUsageInput, 1)
	app := newAudioTestApp(upstream, recorded)

	body, contentType := buildASRMultipart(t, "clip.wav", minimalWAV(1), map[string]string{"model": "FunAudioLLM/SenseVoiceSmall"})
	w := serveAudio(t, app, "/v1/audio/transcriptions", body, contentType)

	require.Equal(t, http.StatusOK, w.Code)
	// 默认回形=json（OpenAI 形状）。
	require.JSONEq(t, `{"text":"你好，壹座"}`, w.Body.String())

	// 上游收到 multipart 重建件：file+model+强制 verbose_json（取时长计费）。
	capture.mu.Lock()
	require.Equal(t, siliconflowAudioTranscriptionsPath, capture.path)
	require.Equal(t, "Bearer sk-audio-test", capture.authHeader)
	require.Contains(t, capture.contentType, "multipart/form-data")
	require.Contains(t, string(capture.body), `name="model"`)
	require.Contains(t, string(capture.body), "FunAudioLLM/SenseVoiceSmall")
	require.Contains(t, string(capture.body), `name="response_format"`)
	require.Contains(t, string(capture.body), "verbose_json")
	require.Contains(t, string(capture.body), `filename="clip.wav"`)
	capture.mu.Unlock()

	// 计量：mode=stt，时长 12 秒 → 12/3600 小时（Group STTPerHour 价目位面）。
	select {
	case input := <-recorded:
		require.NotNil(t, input.Result.AudioUsage)
		require.Equal(t, "stt", input.Result.AudioUsage.Mode)
		require.InDelta(t, 12.0/3600, input.Result.AudioUsage.DurationOrUnits, 1e-12)
		require.Equal(t, "audio:stt", input.Result.Model)
		require.Equal(t, "siliconflow:FunAudioLLM/SenseVoiceSmall", input.Result.UpstreamModel)
		require.Equal(t, int64(41), input.Account.ID)
		require.Equal(t, "/v1/audio/transcriptions", input.InboundEndpoint)
	case <-time.After(2 * time.Second):
		t.Fatal("ASR usage was not recorded")
	}
}

func TestAudioLaneASRResponseFormats(t *testing.T) {
	upstream := newAudioFakeUpstream(t, &audioUpstreamCapture{})
	recorded := make(chan *service.OpenAIRecordUsageInput, 3)
	app := newAudioTestApp(upstream, recorded)

	for _, tc := range []struct {
		format string
		want   string
	}{
		{"text", "你好，壹座"}, // 纯文本
		{"verbose_json", `{"text":"你好，壹座","duration":12,"language":"zh"}`}, // 上游原样
		{"json", `{"text":"你好，壹座"}`},                                       // 默认
	} {
		t.Run(tc.format, func(t *testing.T) {
			fields := map[string]string{}
			if tc.format != "json" {
				fields["response_format"] = tc.format
			}
			body, contentType := buildASRMultipart(t, "a.wav", minimalWAV(1), fields)
			w := serveAudio(t, app, "/v1/audio/transcriptions", body, contentType)
			require.Equal(t, http.StatusOK, w.Code)
			if tc.format == "verbose_json" {
				require.JSONEq(t, tc.want, w.Body.String())
			} else {
				require.Equal(t, tc.want, w.Body.String())
			}
		})
	}

	t.Run("unsupported response_format rejected", func(t *testing.T) {
		body, contentType := buildASRMultipart(t, "a.wav", minimalWAV(1), map[string]string{"response_format": "srt"})
		w := serveAudio(t, app, "/v1/audio/transcriptions", body, contentType)
		require.Equal(t, http.StatusBadRequest, w.Code)
		require.Contains(t, w.Body.String(), "unsupported response_format")
	})
}

func TestAudioLaneASRValidationAndDurationFallback(t *testing.T) {
	t.Run("missing file and non-multipart rejected", func(t *testing.T) {
		upstream := newAudioFakeUpstream(t, &audioUpstreamCapture{})
		recorded := make(chan *service.OpenAIRecordUsageInput, 1)
		app := newAudioTestApp(upstream, recorded)

		// 非 multipart
		w := serveAudio(t, app, "/v1/audio/transcriptions", []byte("{}"), "application/json")
		require.Equal(t, http.StatusBadRequest, w.Code)
		require.Contains(t, w.Body.String(), "multipart/form-data")

		// multipart 但无 file 字段
		var buf strings.Builder
		writer := multipart.NewWriter(&buf)
		require.NoError(t, writer.WriteField("model", "x"))
		require.NoError(t, writer.Close())
		w = serveAudio(t, app, "/v1/audio/transcriptions", []byte(buf.String()), writer.FormDataContentType())
		require.Equal(t, http.StatusBadRequest, w.Code)
		require.Contains(t, w.Body.String(), "`file` is required")

		select {
		case <-recorded:
			t.Fatal("invalid ASR requests must not be billed")
		default:
		}
	})

	t.Run("missing upstream duration falls back to wav header", func(t *testing.T) {
		capture := &audioUpstreamCapture{responseBody: []byte(`{"text":"无时长字段","duration":0}`)}
		upstream := newAudioFakeUpstream(t, capture)
		recorded := make(chan *service.OpenAIRecordUsageInput, 1)
		app := newAudioTestApp(upstream, recorded)

		// 2 秒 wav（byteRate 32000 → data chunk 64000 字节）。
		body, contentType := buildASRMultipart(t, "b.wav", minimalWAV(2), nil)
		w := serveAudio(t, app, "/v1/audio/transcriptions", body, contentType)
		require.Equal(t, http.StatusOK, w.Code)

		select {
		case input := <-recorded:
			require.Equal(t, "stt", input.Result.AudioUsage.Mode)
			require.InDelta(t, 2.0/3600, input.Result.AudioUsage.DurationOrUnits, 1e-9)
		case <-time.After(2 * time.Second):
			t.Fatal("ASR usage was not recorded")
		}
	})
}

func TestAudioLaneTTS(t *testing.T) {
	capture := &audioUpstreamCapture{}
	upstream := newAudioFakeUpstream(t, capture)
	recorded := make(chan *service.OpenAIRecordUsageInput, 1)
	app := newAudioTestApp(upstream, recorded)

	w := serveAudio(t, app, "/v1/audio/speech", []byte(`{"input":"你好，壹座平台","voice":"FunAudioLLM/CosyVoice2-0.5B:alex"}`), "application/json")
	require.Equal(t, http.StatusOK, w.Code)
	require.Equal(t, "audio/mpeg", w.Header().Get("Content-Type"))
	require.Equal(t, "ID3fake-mp3-bytes", w.Body.String()) // 二进制透传

	capture.mu.Lock()
	require.Equal(t, siliconflowAudioSpeechPath, capture.path)
	require.Equal(t, "Bearer sk-audio-test", capture.authHeader)
	require.Equal(t, "你好，壹座平台", capture.jsonBody["input"])
	require.Equal(t, audioDefaultTTSModel, capture.jsonBody["model"]) // 默认模型填充
	require.Equal(t, "mp3", capture.jsonBody["response_format"])      // 默认格式填充
	require.Equal(t, "FunAudioLLM/CosyVoice2-0.5B:alex", capture.jsonBody["voice"])
	capture.mu.Unlock()

	// 计量：mode=tts，字符数按 utf-8 rune（非字节！）计 → "你好，壹座平台"=7 rune / 1e6。
	select {
	case input := <-recorded:
		require.NotNil(t, input.Result.AudioUsage)
		require.Equal(t, "tts", input.Result.AudioUsage.Mode)
		require.InDelta(t, 7.0/1_000_000, input.Result.AudioUsage.DurationOrUnits, 1e-12)
		require.Equal(t, "audio:tts", input.Result.Model)
		require.Equal(t, "/v1/audio/speech", input.InboundEndpoint)
	case <-time.After(2 * time.Second):
		t.Fatal("TTS usage was not recorded")
	}
}

func TestAudioLaneTTSValidation(t *testing.T) {
	upstream := newAudioFakeUpstream(t, &audioUpstreamCapture{})
	recorded := make(chan *service.OpenAIRecordUsageInput, 1)
	app := newAudioTestApp(upstream, recorded)

	for _, tc := range []struct {
		name string
		body string
		want string
	}{
		{"missing input", `{"voice":"v"}`, "`input`"},
		{"blank input", `{"input":"   "}`, "`input`"},
		{"non-json", `not-json`, "JSON object"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := serveAudio(t, app, "/v1/audio/speech", []byte(tc.body), "application/json")
			require.Equal(t, http.StatusBadRequest, w.Code)
			require.Contains(t, w.Body.String(), tc.want)
		})
	}
	select {
	case <-recorded:
		t.Fatal("invalid TTS requests must not be billed")
	default:
	}
}

func TestAudioLaneUpstreamFailureNotBilled(t *testing.T) {
	capture := &audioUpstreamCapture{responseCode: http.StatusUnauthorized, responseBody: []byte(`{"error":"Invalid API key: sk-abc"}`)}
	upstream := newAudioFakeUpstream(t, capture)
	recorded := make(chan *service.OpenAIRecordUsageInput, 1)
	app := newAudioTestApp(upstream, recorded)

	// 上游 401 → 502（上游侧故障），不外泄、不计费。
	w := serveAudio(t, app, "/v1/audio/speech", []byte(`{"input":"x"}`), "application/json")
	require.Equal(t, http.StatusBadGateway, w.Code)
	require.NotContains(t, w.Body.String(), "Invalid API key")
	require.NotContains(t, w.Body.String(), "sk-abc")

	// 上游 400（内容/参数问题）→ 400 给调用者。
	capture.mu.Lock()
	capture.responseCode = http.StatusBadRequest
	capture.mu.Unlock()
	w = serveAudio(t, app, "/v1/audio/speech", []byte(`{"input":"x"}`), "application/json")
	require.Equal(t, http.StatusBadRequest, w.Code)
	require.Contains(t, w.Body.String(), "upstream rejected")

	select {
	case <-recorded:
		t.Fatal("failed upstream calls must not be billed")
	case <-time.After(200 * time.Millisecond):
	}
}

// 轮转（ciyuan-lane-pool-failover 3.2）：TTS 首号 402 → 换二号线成功并计量归因。
func TestAudioTTSFailoverToSecondAccount(t *testing.T) {
	gin.SetMode(gin.TestMode)
	var mu sync.Mutex
	served := []string{}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		mu.Lock()
		served = append(served, auth)
		mu.Unlock()
		if strings.HasSuffix(auth, "sk-a") {
			w.WriteHeader(http.StatusPaymentRequired)
			_, _ = w.Write([]byte(`{"error":{"message":"Insufficient balance"}}`))
			return
		}
		w.Header().Set("Content-Type", "audio/mpeg")
		_, _ = w.Write([]byte("fake-mp3"))
	}))
	t.Cleanup(upstream.Close)

	writer := newFakeCooldownWriter()
	lister := &fakeLaneAccountLister{accounts: []service.Account{
		{ID: 1, Platform: domain.PlatformOpenAINext, Status: service.StatusActive, Priority: 1,
			Credentials: map[string]any{"api_key": "sk-a", "base_url": upstream.URL}},
		{ID: 2, Platform: domain.PlatformOpenAINext, Status: service.StatusActive, Priority: 2,
			Credentials: map[string]any{"api_key": "sk-b", "base_url": upstream.URL}},
	}}
	recorded := make(chan *service.OpenAIRecordUsageInput, 1)
	app := &Lanes{
		Meter: &Meter{recordUsage: func(_ context.Context, input *service.OpenAIRecordUsageInput) error {
			recorded <- input
			return nil
		}},
		Pool: NewPool(lister, writer),
		adapters: map[string]LaneAdapter{
			LaneAudio: NewAudioAdapter(),
		},
	}

	w := serveAudio(t, app, "/v1/audio/speech", []byte(`{"input":"你好"}`), "application/json")
	require.Equal(t, http.StatusOK, w.Code)
	mu.Lock()
	require.Len(t, served, 2)
	mu.Unlock()
	require.Len(t, writer.writes[1], 1, "首号落额度类冷却")
	select {
	case input := <-recorded:
		require.Equal(t, int64(2), input.Account.ID, "计量归因二号线")
	case <-time.After(2 * time.Second):
		t.Fatal("usage was not recorded")
	}
}

// 全池尽（两号都 500）→ 502 不计费，两号都落瞬态冷却。
func TestAudioTTSAllExhausted(t *testing.T) {
	gin.SetMode(gin.TestMode)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(upstream.Close)

	writer := newFakeCooldownWriter()
	lister := &fakeLaneAccountLister{accounts: []service.Account{
		{ID: 1, Platform: domain.PlatformOpenAINext, Status: service.StatusActive, Priority: 1,
			Credentials: map[string]any{"api_key": "sk-a", "base_url": upstream.URL}},
		{ID: 2, Platform: domain.PlatformOpenAINext, Status: service.StatusActive, Priority: 2,
			Credentials: map[string]any{"api_key": "sk-b", "base_url": upstream.URL}},
	}}
	recorded := make(chan *service.OpenAIRecordUsageInput, 1)
	app := &Lanes{
		Meter: &Meter{recordUsage: func(_ context.Context, input *service.OpenAIRecordUsageInput) error {
			recorded <- input
			return nil
		}},
		Pool: NewPool(lister, writer),
		adapters: map[string]LaneAdapter{
			LaneAudio: NewAudioAdapter(),
		},
	}

	w := serveAudio(t, app, "/v1/audio/speech", []byte(`{"input":"你好"}`), "application/json")
	require.Equal(t, http.StatusBadGateway, w.Code)
	require.Contains(t, w.Body.String(), "lane_upstream_error")
	require.Len(t, writer.writes[1], 1)
	require.Len(t, writer.writes[2], 1)
	select {
	case <-recorded:
		t.Fatal("exhausted pool must not be billed")
	case <-time.After(200 * time.Millisecond):
	}
}
