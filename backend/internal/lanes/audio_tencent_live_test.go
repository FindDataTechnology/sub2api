package lanes

import (
	"context"
	"os"
	"strings"
	"testing"
	"unicode/utf8"
)

// 真机验收（腾讯云）。需显式开启：
//
//	TENCENT_LIVE_TEST=1 TENCENT_SECRET_ID=... TENCENT_SECRET_KEY=... \
//	  go test ./internal/lanes/ -run TestTencentLIVE -v
func TestTencentLIVE_TextToVoice(t *testing.T) {
	if os.Getenv("TENCENT_LIVE_TEST") != "1" {
		t.Skip("set TENCENT_LIVE_TEST=1 to run against the real Tencent Cloud API")
	}
	creds := tencentCredentials{
		SecretID:  strings.TrimSpace(os.Getenv("TENCENT_SECRET_ID")),
		SecretKey: strings.TrimSpace(os.Getenv("TENCENT_SECRET_KEY")),
		Region:    "ap-guangzhou",
	}
	if creds.SecretID == "" || creds.SecretKey == "" {
		t.Skip("TENCENT_SECRET_ID/TENCENT_SECRET_KEY not set")
	}

	a := newTencentAdapter()
	const text = "你好，这是词元语音车道的真机验收。"
	audio, err := a.synthesize(context.Background(), creds, text, tencentDefaultVoice, "mp3", 16000)
	if err != nil {
		t.Fatalf("TextToVoice against real API: %v", err)
	}
	if len(audio) < 1000 {
		t.Fatalf("audio too short: %d bytes", len(audio))
	}
	t.Logf("LIVE OK: %d bytes, %d chars billed", len(audio), utf8.RuneCountInString(text))
}

// TestTencentLIVE_SentenceRecognition 一句话识别真机验收（用之字形 TTS 产出的
// 音频回灌，形成「合成→识别」闭环，无需外部音频源）。
func TestTencentLIVE_SentenceRecognition(t *testing.T) {
	if os.Getenv("TENCENT_LIVE_TEST") != "1" {
		t.Skip("set TENCENT_LIVE_TEST=1")
	}
	creds := tencentCredentials{
		SecretID:  strings.TrimSpace(os.Getenv("TENCENT_SECRET_ID")),
		SecretKey: strings.TrimSpace(os.Getenv("TENCENT_SECRET_KEY")),
		Region:    "ap-guangzhou",
	}
	if creds.SecretID == "" || creds.SecretKey == "" {
		t.Skip("credentials not set")
	}
	a := newTencentAdapter()
	ctx := context.Background()

	// 用 wav 合成（一句话识别对 wav 支持最好），再回灌识别。
	const spoken = "腾讯云语音识别测试成功"
	audio, err := a.synthesize(ctx, creds, spoken, tencentDefaultVoice, "wav", 16000)
	if err != nil {
		t.Fatalf("synthesize: %v", err)
	}
	if len(audio) < 1000 {
		t.Fatalf("synthesized audio too short: %d", len(audio))
	}
	t.Logf("synthesized %d bytes wav", len(audio))

	text, err := a.transcribe(ctx, creds, audio, "wav")
	if err != nil {
		t.Fatalf("transcribe: %v", err)
	}
	if strings.TrimSpace(text) == "" {
		t.Fatal("empty transcription")
	}
	t.Logf("LIVE ASR OK: %q", text)
}
