package lanes

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gorilla/websocket"
)

// 这些测试用本地 WebSocket 桩复现真实事件序列（金样取自 2026-10-18 真机抓包），
// 目的是锁住「帧编解码 + 事件驱动」这条最容易写错的主干，而非替代真机验收。

// stubDoubaoTTS 启动一个按豆包 V3 语义应答的 WS 桩。
func stubDoubaoTTS(t *testing.T, audioChunks [][]byte, failWith *diagFrame) *httptest.Server {
	t.Helper()
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()

		// 断言鉴权头齐备（真实上游缺一即拒）。
		if r.Header.Get("X-Api-Key") == "" || r.Header.Get("X-Api-Resource-Id") != doubaoResourceTTS {
			return
		}
		sessionID := "stub-session"

		for {
			mt, raw, err := conn.ReadMessage()
			if err != nil {
				return
			}
			if mt != websocket.BinaryMessage {
				continue
			}
			// 桩侧按「上行帧」布局读取：4B 头 + 4B event（+ 可选 sid 段 + 4B len + payload）。
			if len(raw) < 8 {
				return
			}
			upEvent := int(binary.BigEndian.Uint32(raw[4:8]))
			if failWith != nil && upEvent == failWith.onEvent {
				_ = conn.WriteMessage(websocket.BinaryMessage, buildDoubaoErrorFrame(failWith.code, failWith.message))
				return
			}
			switch upEvent {
			case doubaoEventStartConnection:
				_ = conn.WriteMessage(websocket.BinaryMessage,
					buildDoubaoFrame(doubaoMsgTypeFullServer, doubaoFlagWithEvent, doubaoEventConnectionStarted, sessionID, []byte("{}")))
			case doubaoEventStartSession:
				// 校验 event 是 int 而非字符串（真实上游会拒字符串）。
				// 上行帧布局：4B头 + 4B event + 4B sidLen + sid + 4B len + payload
				sidLen := int(binary.BigEndian.Uint32(raw[8:12]))
				payOff := 12 + sidLen
				if len(raw) < payOff+4 {
					return
				}
				payLen := int(binary.BigEndian.Uint32(raw[payOff : payOff+4]))
				payload := raw[payOff+4 : payOff+4+payLen]
				var body map[string]any
				_ = json.Unmarshal(payload, &body)
				if _, ok := body["event"].(float64); !ok {
					return
				}
				_ = conn.WriteMessage(websocket.BinaryMessage,
					buildDoubaoFrame(doubaoMsgTypeFullServer, doubaoFlagWithEvent, doubaoEventSessionStarted, sessionID, []byte("{}")))
			case doubaoEventTaskRequest:
				_ = conn.WriteMessage(websocket.BinaryMessage,
					buildDoubaoFrame(doubaoMsgTypeFullServer, doubaoFlagWithEvent, doubaoEventTTSSentenceStart, sessionID, []byte("{}")))
				for _, chunk := range audioChunks {
					_ = conn.WriteMessage(websocket.BinaryMessage, buildDoubaoAudioFrame(sessionID, chunk))
				}
				_ = conn.WriteMessage(websocket.BinaryMessage,
					buildDoubaoFrame(doubaoMsgTypeFullServer, doubaoFlagWithEvent, doubaoEventTTSSentenceEnd, sessionID, []byte("{}")))
			case doubaoEventFinishSession:
				_ = conn.WriteMessage(websocket.BinaryMessage,
					buildDoubaoFrame(doubaoMsgTypeFullServer, doubaoFlagWithEvent, doubaoEventSessionFinished, sessionID, []byte(`{"status_code":20000000}`)))
				return
			}
		}
	}))
}

type diagFrame struct {
	onEvent int
	code    int
	message string
}

// buildDoubaoAudioFrame 构造 Audio-only 响应帧（serialization=raw）。
func buildDoubaoAudioFrame(sessionID string, audio []byte) []byte {
	buf := []byte{0x11, 0xb4, 0x00, 0x00}
	buf = binary.BigEndian.AppendUint32(buf, doubaoEventTTSResponse)
	buf = binary.BigEndian.AppendUint32(buf, uint32(len(sessionID)))
	buf = append(buf, sessionID...)
	buf = binary.BigEndian.AppendUint32(buf, uint32(len(audio)))
	return append(buf, audio...)
}

// buildDoubaoErrorFrame 构造错误帧（msg_type=15）。
func buildDoubaoErrorFrame(code int, message string) []byte {
	buf := []byte{0x11, 0xf0, 0x10, 0x00}
	buf = binary.BigEndian.AppendUint32(buf, uint32(code))
	return append(buf, []byte(message)...)
}

// newTestDoubaoAdapter 指向桩服务的适配器。
func newTestDoubaoAdapter(serverURL string) *doubaoAdapter {
	a := newDoubaoAdapter()
	a.dialer = &websocket.Dialer{HandshakeTimeout: 5 * time.Second}
	a.wsURL = serverURL
	return a
}

// accountWithCredentials 构造仅含凭据的账号（协议分派测试用）。
func accountWithCredentials(creds map[string]any) service.Account {
	if creds == nil {
		creds = map[string]any{}
	}
	return service.Account{Credentials: creds}
}

func TestDoubaoAdapter_SynthesizeEventSequence(t *testing.T) {
	chunks := [][]byte{
		[]byte("ID3\x04\x00\x00\x00\x00"),
		[]byte("\xff\xf3\x84\xc4-audio-2"),
		[]byte("\xff\xf3\x84\xc4-audio-3"),
	}
	srv := stubDoubaoTTS(t, chunks, nil)
	defer srv.Close()
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http")

	a := newTestDoubaoAdapter(wsURL)
	audio, err := a.synthesize(context.Background(), "test-key", doubaoTTSOptions{
		Text:  "你好，这是测试文本",
		Voice: "zh_female_shuangkuaisisi_uranus_bigtts",
	})
	if err != nil {
		t.Fatalf("synthesize: %v", err)
	}

	want := make([]byte, 0)
	for _, c := range chunks {
		want = append(want, c...)
	}
	if string(audio) != string(want) {
		t.Errorf("audio = %q, want %q", audio, want)
	}
	if !strings.HasPrefix(string(audio), "ID3") {
		t.Errorf("audio must start with ID3 tag, got %q", string(audio[:3]))
	}
}

func TestDoubaoAdapter_SynthesizeReportsUpstreamErrorFrame(t *testing.T) {
	srv := stubDoubaoTTS(t, nil, &diagFrame{
		onEvent: doubaoEventTaskRequest,
		code:    45000030,
		message: `resource not granted`,
	})
	defer srv.Close()
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http")

	a := newTestDoubaoAdapter(wsURL)
	_, err := a.synthesize(context.Background(), "test-key", doubaoTTSOptions{Text: "x"})
	if err == nil {
		t.Fatal("expected error from upstream error frame")
	}
	var de *doubaoUpstreamError
	if !asDoubaoError(err, &de) {
		t.Fatalf("expected doubaoUpstreamError, got %T", err)
	}
	if de.code != 45000030 {
		t.Errorf("code = %d, want 45000030", de.code)
	}
}

func TestDoubaoAdapter_SynthesizeRejectsMissingCredential(t *testing.T) {
	a := newTestDoubaoAdapter("ws://127.0.0.1:1")
	if _, err := a.synthesize(context.Background(), "", doubaoTTSOptions{Text: "x"}); err == nil {
		t.Fatal("expected error for empty api key")
	}
}

func TestDoubaoASR_SubmitAndPollCompletes(t *testing.T) {
	var submits, queries int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Api-Key") == "" || r.Header.Get("X-Api-Resource-Id") != doubaoResourceASR {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if strings.Contains(r.URL.Path, "submit") {
			submits++
			w.Header().Set("X-Api-Status-Code", "20000000")
			w.Header().Set("X-Api-Message", "OK")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"task_id":"t1"}`))
			return
		}
		queries++
		w.Header().Set("X-Api-Status-Code", "20000000")
		w.Header().Set("X-Api-Message", "OK")
		_, _ = w.Write([]byte(`{"audio_info":{"duration":33621},"result":{"text":"hello world"}}`))
	}))
	defer srv.Close()

	a := newDoubaoAdapter()
	a.submitURL = srv.URL + "/submit"
	a.queryURL = srv.URL + "/query"

	res, err := a.submitAndPoll(context.Background(), "k", "https://example.com/a.wav", doubaoASROptions{Format: "wav"})
	if err != nil {
		t.Fatalf("submitAndPoll: %v", err)
	}
	if res.Text != "hello world" {
		t.Errorf("text = %q, want hello world", res.Text)
	}
	if res.DurationSecond != 33.621 {
		t.Errorf("duration = %v, want 33.621", res.DurationSecond)
	}
	if submits != 1 {
		t.Errorf("submits = %d, want 1", submits)
	}
}

func TestDoubaoASR_SubmitFailurePropagates(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Api-Status-Code", "45000030")
		w.Header().Set("X-Api-Message", "requested resource not granted")
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()

	a := newDoubaoAdapter()
	a.submitURL = srv.URL + "/submit"
	a.queryURL = srv.URL + "/query"

	_, err := a.submitAndPoll(context.Background(), "k", "https://example.com/a.wav", doubaoASROptions{Format: "wav"})
	if err == nil {
		t.Fatal("expected submit failure")
	}
	var de *doubaoUpstreamError
	if !asDoubaoError(err, &de) {
		t.Fatalf("expected doubaoUpstreamError, got %T", err)
	}
	if de.code != 45000030 {
		t.Errorf("code = %d, want 45000030", de.code)
	}
	// 该错误必须映射到额度/鉴权类长冷却（45000030 属资源未授予）。
	uf := doubaoToUpstreamFailure(err)
	if uf.status != http.StatusForbidden {
		t.Errorf("mapped status = %d, want 403 (long cooldown)", uf.status)
	}
}

func TestAudioProtocolOf_DefaultsToOpenAI(t *testing.T) {
	if got := audioProtocolOf(nil); got != audioProtocolOpenAI {
		t.Errorf("nil account = %q, want openai", got)
	}
	empty := accountWithCredentials(nil)
	if got := audioProtocolOf(&empty); got != audioProtocolOpenAI {
		t.Errorf("empty creds = %q, want openai", got)
	}
	doubao := accountWithCredentials(map[string]any{"audio_protocol": "DOUBAO"})
	if got := audioProtocolOf(&doubao); got != audioProtocolDoubao {
		t.Errorf("explicit doubao (case-insensitive) = %q, want doubao", got)
	}
	bogus := accountWithCredentials(map[string]any{"audio_protocol": "bogus"})
	if got := audioProtocolOf(&bogus); got != audioProtocolOpenAI {
		t.Errorf("unknown protocol = %q, want fallback openai", got)
	}
}
