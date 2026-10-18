package lanes

import (
	"context"
	"os"
	"strings"
	"testing"
)

// 真机验收：用真实豆包服务跑一遍合成。需显式开启，避免 CI 依赖外部服务与付费额度。
//
//	DOUBAO_LIVE_TEST=1 DOUBAO_API_KEY=... go test ./internal/lanes/ -run TestDoubaoLIVE -v
func TestDoubaoLIVE_Synthesize(t *testing.T) {
	if os.Getenv("DOUBAO_LIVE_TEST") != "1" {
		t.Skip("set DOUBAO_LIVE_TEST=1 to run against the real openspeech service")
	}
	key := strings.TrimSpace(os.Getenv("DOUBAO_API_KEY"))
	if key == "" {
		t.Skip("DOUBAO_API_KEY not set")
	}

	a := newDoubaoAdapter()
	audio, err := a.synthesize(context.Background(), key, doubaoTTSOptions{
		Text:  "你好，这是词元语音车道的真机验收。",
		Voice: doubaoDefaultTTSVoice,
	})
	if err != nil {
		t.Fatalf("synthesize against real service: %v", err)
	}
	if len(audio) < 1000 {
		t.Fatalf("audio too short: %d bytes", len(audio))
	}
	// 真机返回必须以 ID3 标签起始（mp3）。
	if string(audio[:3]) != "ID3" {
		t.Errorf("audio does not start with ID3 tag; first bytes = %q", string(audio[:8]))
	}
	t.Logf("LIVE OK: %d bytes of audio, starts with ID3", len(audio))
}

// TestDoubaoLIVE_ASR 录音文件识别真机验收。
//
// 上游只接受公网可达 URL，故本测试需要外部托管：把一段音频放到
// DOUBAO_ASR_AUDIO_URL 指向的地址后运行。未设置则跳过（不伪造公网可达性）。
func TestDoubaoLIVE_ASR(t *testing.T) {
	if os.Getenv("DOUBAO_LIVE_TEST") != "1" {
		t.Skip("set DOUBAO_LIVE_TEST=1")
	}
	key := strings.TrimSpace(os.Getenv("DOUBAO_API_KEY"))
	audioURL := strings.TrimSpace(os.Getenv("DOUBAO_ASR_AUDIO_URL"))
	if key == "" || audioURL == "" {
		t.Skip("DOUBAO_API_KEY and DOUBAO_ASR_AUDIO_URL required")
	}

	a := newDoubaoAdapter()
	res, err := a.submitAndPoll(context.Background(), key, audioURL, doubaoASROptions{Format: "wav"})
	if err != nil {
		t.Fatalf("submitAndPoll: %v", err)
	}
	if strings.TrimSpace(res.Text) == "" {
		t.Fatal("empty transcription from real service")
	}
	t.Logf("LIVE ASR OK: %d chars, duration=%.3fs, text=%q",
		len([]rune(res.Text)), res.DurationSecond, firstN(res.Text, 60))
}
