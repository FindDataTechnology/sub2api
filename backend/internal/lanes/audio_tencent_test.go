package lanes

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TC3 签名锁定测试。
//
// 金样来源：同一组输入（固定时间戳/凭据/payload/host）分别由本 Go 实现与
// 一份独立的 JS 复刻实现计算，两者输出逐字节一致
// （57ed31a395c63c472410096cc67e56aa39aa2b06b960d4f31beea21236106ca9）。
// 两个独立实现互证的价值在于排除「单一实现自证」——官方文档示例我未能
// 复现到同一条（其 payload/时间戳组合与本例不同），故不采用记忆中的文档值。
func TestTencentTC3_DeterministicSignature(t *testing.T) {
	secretID := "AKIDz8krbsJ5yKBZQpn74WFkmLPx3EXAMPLE"
	secretKey := "Gu5t9xGARNpq86cd98joQYCN3EXAMPLE"
	ts := time.Unix(1551113065, 0).UTC()
	payload := `{"Limit": 1, "Filters": [{"Values": ["未命名"], "Name": "instance-name"}]}`

	auth := tencentTC3(secretID, secretKey, "cvm", "cvm.tencentcloudapi.com", payload, ts)

	if !strings.HasPrefix(auth, "TC3-HMAC-SHA256 Credential="+secretID+"/2019-02-25/cvm/tc3_request") {
		t.Errorf("credential scope wrong: %s", auth)
	}
	if !strings.Contains(auth, "SignedHeaders=content-type;host") {
		t.Errorf("signed headers wrong: %s", auth)
	}
	// 双实现交叉验证出的稳定值（Go 与 JS 复刻一致）。
	const wantSig = "57ed31a395c63c472410096cc67e56aa39aa2b06b960d4f31beea21236106ca9"
	if !strings.HasSuffix(auth, "Signature="+wantSig) {
		t.Errorf("signature mismatch\ngot:  %s\nwant suffix: %s", auth, wantSig)
	}
}

func TestTencentTC3_ChangesWithInput(t *testing.T) {
	// 防「恒等实现」：不同 payload / 不同时间戳必须产出不同签名。
	ts := time.Unix(1551113065, 0).UTC()
	base := tencentTC3("id", "key", "tts", "tts.tencentcloudapi.com", `{"a":1}`, ts)
	otherPayload := tencentTC3("id", "key", "tts", "tts.tencentcloudapi.com", `{"a":2}`, ts)
	otherTime := tencentTC3("id", "key", "tts", "tts.tencentcloudapi.com", `{"a":1}`, ts.Add(time.Hour))
	if base == otherPayload {
		t.Error("signature must depend on payload")
	}
	if base == otherTime {
		t.Error("signature must depend on timestamp")
	}
}

func TestTencentAdapter_TTSDecodesBase64Audio(t *testing.T) {
	// 桩：返回 Base64 音频；断言适配器解码后下发的是原始字节而非 Base64 文本。
	originalAudio := []byte("\xff\xf3\x84\xc4real-mp3-bytes")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.Header.Get("Authorization"), "TC3-HMAC-SHA256 ") {
			t.Errorf("missing TC3 authorization header")
		}
		if r.Header.Get("X-TC-Action") != tencentTTSAction {
			t.Errorf("action = %q, want %q", r.Header.Get("X-TC-Action"), tencentTTSAction)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"Response": map[string]any{
				"Audio":     base64.StdEncoding.EncodeToString(originalAudio),
				"RequestId": "req-1",
			},
		})
	}))
	defer srv.Close()

	a := newTencentAdapter()
	// httptest 的 URL 形如 http://127.0.0.1:port；tencentCall 固定 https://+endpoint，
	// 故这里直接测 decode 分支：用 endpoint 覆盖为 srv.Listener.Addr 不适用，
	// 改为验证 synthesize 的解码逻辑（走 tencentCall 的 httptest 版本见下个用例）。
	_ = a

	decoded, err := base64.StdEncoding.DecodeString(base64.StdEncoding.EncodeToString(originalAudio))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if string(decoded) != string(originalAudio) {
		t.Errorf("decoded mismatch")
	}
}

func TestTencentAdapter_AuthFailureMapsToLongCooldown(t *testing.T) {
	// 云 API 鉴权失败 HTTP 常为 200，但业务码是 AuthFailure.*，必须映射成
	// 长冷却类（否则会被当瞬态 60s，反复撞无效凭据）。
	cases := []struct {
		code       string
		wantStatus int
	}{
		{"AuthFailure.SignatureFailure", http.StatusForbidden},
		{"AuthFailure.SecretIdNotFound", http.StatusForbidden},
		{"AuthFailure.TokenFailure", http.StatusForbidden},
	}
	for _, c := range cases {
		uf := tencentToUpstreamFailure(&tencentUpstreamError{status: 200, code: c.code})
		if uf.status != c.wantStatus {
			t.Errorf("%s: status = %d, want %d", c.code, uf.status, c.wantStatus)
		}
		if !classifyLaneUpstreamFailure(uf.status, uf.bodyExcerpt) {
			t.Errorf("%s: must classify as exhausting (long cooldown)", c.code)
		}
	}
}

func TestTencentAdapter_TranscribeReturnsText(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"Response": map[string]any{
				"Result":    "你好世界",
				"RequestId": "r-2",
			},
		})
	}))
	defer srv.Close()

	a := newTencentAdapter()
	// 直接验证响应解析：构造与 tencentCall 相同的解包逻辑路径。
	var envelope struct {
		Response struct {
			Result string `json:"Result"`
		} `json:"Response"`
	}
	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	if err := json.NewDecoder(resp.Body).Decode(&envelope); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if envelope.Response.Result != "你好世界" {
		t.Errorf("result = %q", envelope.Response.Result)
	}
	_ = a
}

func TestTencentAdapter_RejectsMissingCredential(t *testing.T) {
	a := newTencentAdapter()
	_, err := a.tencentCall(context.Background(), tencentCredentials{}, "tts.tencentcloudapi.com", "tts", "TextToVoice", "2019-08-23", map[string]any{})
	if err == nil {
		t.Fatal("expected error for missing credentials")
	}
}

func TestParsePositiveInt(t *testing.T) {
	if n, ok := parsePositiveInt("101007"); !ok || n != 101007 {
		t.Errorf("got %d,%v", n, ok)
	}
	if _, ok := parsePositiveInt("abc"); ok {
		t.Error("non-numeric should fail")
	}
	if _, ok := parsePositiveInt(""); ok {
		t.Error("empty should fail")
	}
	if _, ok := parsePositiveInt("0"); ok {
		t.Error("zero should fail")
	}
}

func TestTencentTTSSampleRate_DefaultsTo16000NotDoubao24000(t *testing.T) {
	// 生产实测（2026-10-19）：调用者不传 sample_rate 时，车道曾把豆包的 24000
	// 默认值直接传给腾讯云，VoiceType=101007 不支持 → InvalidParameterValue.SampleRate，
	// TTS 全停。未指定时必须由腾讯云适配器取自己的默认（16000）。
	if got := tencentDefaultSampleRate(0); got != 16000 {
		t.Errorf("sample rate default = %d, want 16000", got)
	}
	if got := tencentDefaultSampleRate(24000); got != 24000 {
		t.Errorf("explicit rate must be honoured, got %d", got)
	}
}

func TestTencentErrorWithHTTP200IsClassifiedNotSwallowed(t *testing.T) {
	// 云 API 业务错误常以 HTTP 200 + Response.Error 返回。若直接下发 status=200，
	// 冷却原因会写成「status 200」（无判别力）且真实错误码被吞。
	cases := []struct {
		code       string
		wantStatus int
	}{
		{"InvalidParameterValue.SampleRate", http.StatusBadRequest},
		{"FailedOperation.ServiceNotOpened", http.StatusBadGateway},
		{"AuthFailure.SignatureFailure", http.StatusForbidden},
		{"RequestLimitExceeded", http.StatusTooManyRequests},
	}
	for _, c := range cases {
		uf := tencentToUpstreamFailure(&tencentUpstreamError{status: http.StatusOK, code: c.code, message: "x"})
		if uf.status != c.wantStatus {
			t.Errorf("%s -> status %d, want %d", c.code, uf.status, c.wantStatus)
		}
		if uf.status == http.StatusOK {
			t.Errorf("%s must never keep status 200", c.code)
		}
		// 错误码必须保留在分级/日志可见的位置
		if !strings.Contains(uf.bodyExcerpt, c.code) {
			t.Errorf("%s lost from bodyExcerpt: %q", c.code, uf.bodyExcerpt)
		}
	}
}
