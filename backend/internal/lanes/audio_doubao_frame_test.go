package lanes

import (
	"bytes"
	"encoding/binary"
	"testing"
)

// 金样字节全部取自 2026-10-18 对 openspeech 真实服务的抓包（真 key、真合成），
// 不是按文档手写——避免「文档理解错但测试也错」的自证循环。

func TestParseDoubaoFrame_ConnectionStarted(t *testing.T) {
	// 完整真机帧（54 字节）：4B 头 + event(50) + sidLen(36) + sid + payloadLen(2) + "{}"
	const hexFrame = "11941000000000320000002433633662646461312d653332612d343532362d616530312d353166643235363335663630000000027b7d"
	raw := mustHex(t, hexFrame)
	if len(raw) != 54 {
		t.Fatalf("fixture len = %d, want 54", len(raw))
	}

	f, err := parseDoubaoFrame(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if f.messageType != doubaoMsgTypeFullServer {
		t.Errorf("messageType = %d, want %d", f.messageType, doubaoMsgTypeFullServer)
	}
	if f.flags != doubaoFlagWithEvent {
		t.Errorf("flags = %d, want %d", f.flags, doubaoFlagWithEvent)
	}
	if f.serialization != doubaoSerializationJSON {
		t.Errorf("serialization = %d, want JSON", f.serialization)
	}
	if f.event != doubaoEventConnectionStarted {
		t.Errorf("event = %d, want %d", f.event, doubaoEventConnectionStarted)
	}
}

func TestParseDoubaoFrame_SessionStarted(t *testing.T) {
	// 完整真机帧（54 字节）：event(150) + sidLen(36) + sid + payloadLen(2) + "{}"
	const hexFrame = "11941000000000960000002431313131313131312d323232322d333333332d343434342d353535353535353535353535000000027b7d"
	raw := mustHex(t, hexFrame)

	f, err := parseDoubaoFrame(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if f.event != doubaoEventSessionStarted {
		t.Errorf("event = %d, want %d", f.event, doubaoEventSessionStarted)
	}
	if f.sessionID == "" {
		t.Error("sessionID empty; session-scoped event must carry it")
	}
}

func TestParseDoubaoFrame_AudioOnly(t *testing.T) {
	// 真机抓包形态：4B 头(11 b4 00 00) + event(352) + sidLen(36) + sid
	//              + uint32 payload size + 裸音频（首帧以 "ID3" 起始）
	// 关键断言：serialization nibble = 0（raw），音频不得被当 JSON/base64。
	sid := "11111111-2222-3333-4444-555555555555"
	audio := []byte("ID3\x04\x00\x00\x00\x00")

	buf := make([]byte, 0, 64)
	buf = append(buf, 0x11, 0xb4, 0x00, 0x00) // version1|hdr1, msgType11|flags4, raw|nocomp
	buf = binary.BigEndian.AppendUint32(buf, doubaoEventTTSResponse)
	buf = binary.BigEndian.AppendUint32(buf, uint32(len(sid)))
	buf = append(buf, sid...)
	buf = binary.BigEndian.AppendUint32(buf, uint32(len(audio)))
	buf = append(buf, audio...)

	f, err := parseDoubaoFrame(buf)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if f.messageType != doubaoMsgTypeAudioOnly {
		t.Errorf("messageType = %d, want %d", f.messageType, doubaoMsgTypeAudioOnly)
	}
	if f.event != doubaoEventTTSResponse {
		t.Errorf("event = %d, want %d", f.event, doubaoEventTTSResponse)
	}
	if f.serialization != doubaoSerializationRaw {
		t.Errorf("serialization = %d, want raw(0) — audio must not be treated as JSON/base64", f.serialization)
	}
	if f.sessionID != sid {
		t.Errorf("sessionID = %q, want %q", f.sessionID, sid)
	}
	if !bytes.Equal(f.payload, audio) {
		t.Errorf("payload = %q, want %q", f.payload, audio)
	}
}

func TestParseDoubaoFrame_RejectsWrongVersion(t *testing.T) {
	// 0x21 => version 2, header words 1 —— 协议只认 version 1
	raw := []byte{0x21, 0x94, 0x10, 0x00, 0, 0, 0, 50}
	if _, err := parseDoubaoFrame(raw); err == nil {
		t.Fatal("expected error for unsupported protocol version")
	}
}

func TestParseDoubaoFrame_RejectsTruncated(t *testing.T) {
	raw := mustHex(t, "11b400000000016000000024")
	if _, err := parseDoubaoFrame(raw); err == nil {
		t.Fatal("expected error for truncated audio frame")
	}
}

func TestBuildDoubaoFrame_EventFieldIsInt32(t *testing.T) {
	// 上行帧：event 必须是 4 字节大端 int32（传字符串会被上游拒——实测坑）
	payload := []byte(`{"event":100}`)
	raw := buildDoubaoFrame(doubaoMsgTypeFullClient, doubaoFlagWithEvent, doubaoEventStartSession, "sid-1", payload)

	if len(raw) < 16 {
		t.Fatalf("frame too short: %d", len(raw))
	}
	if raw[0] != 0x11 {
		t.Errorf("byte0 = %#x, want 0x11 (version 1 | header size 1)", raw[0])
	}
	if raw[1] != byte(doubaoMsgTypeFullClient<<4|doubaoFlagWithEvent) {
		t.Errorf("byte1 = %#x, want %#x", raw[1], byte(doubaoMsgTypeFullClient<<4|doubaoFlagWithEvent))
	}
	if raw[2] != byte(doubaoSerializationJSON<<4) {
		t.Errorf("byte2 = %#x, want JSON serialization", raw[2])
	}
	if got := int(binary.BigEndian.Uint32(raw[4:8])); got != doubaoEventStartSession {
		t.Errorf("event = %d, want %d", got, doubaoEventStartSession)
	}
	sidLen := int(binary.BigEndian.Uint32(raw[8:12]))
	if sidLen != len("sid-1") {
		t.Errorf("sidLen = %d, want %d", sidLen, len("sid-1"))
	}
	if string(raw[12:12+sidLen]) != "sid-1" {
		t.Errorf("sessionID = %q, want sid-1", string(raw[12:12+sidLen]))
	}
	// payload 长度 + 内容
	payOff := 12 + sidLen
	payLen := int(binary.BigEndian.Uint32(raw[payOff : payOff+4]))
	if payLen != len(payload) {
		t.Errorf("payload len = %d, want %d", payLen, len(payload))
	}
	if !bytes.Equal(raw[payOff+4:], payload) {
		t.Errorf("payload = %q, want %q", raw[payOff+4:], payload)
	}
}

func TestBuildDoubaoFrame_OmitsSessionSegmentWhenEmpty(t *testing.T) {
	raw := buildDoubaoFrame(doubaoMsgTypeFullClient, doubaoFlagWithEvent, doubaoEventStartConnection, "", []byte("{}"))
	if len(raw) != 12+2 {
		t.Fatalf("len = %d, want 14 (header+event+payloadLen+2)", len(raw))
	}
}

func TestParseDoubaoFrame_ErrorFrame(t *testing.T) {
	// msg_type=15(错误帧): 4B 头 + 4B error code + payload，无 event/size
	raw := []byte{0x11, 0xf0, 0x10, 0x00}
	raw = binary.BigEndian.AppendUint32(raw, 45000030)
	raw = append(raw, []byte(`{"error":"bad"}`)...)

	f, err := parseDoubaoFrame(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !f.isError() {
		t.Fatal("expected error frame")
	}
	if f.errorCode != 45000030 {
		t.Errorf("errorCode = %d, want 45000030", f.errorCode)
	}
}

func TestDoubaoAudioChunks_ConcatenatesInOrder(t *testing.T) {
	a := &doubaoFrame{payload: []byte("ID3-part1")}
	b := &doubaoFrame{payload: []byte("part2")}
	got := doubaoAudioChunks([]*doubaoFrame{a, b})
	if string(got) != "ID3-part1part2" {
		t.Errorf("got %q", got)
	}
}

func mustHex(t *testing.T, s string) []byte {
	t.Helper()
	out := make([]byte, len(s)/2)
	for i := 0; i < len(out); i++ {
		var v byte
		for j := 0; j < 2; j++ {
			c := s[i*2+j]
			var d byte
			switch {
			case c >= '0' && c <= '9':
				d = c - '0'
			case c >= 'a' && c <= 'f':
				d = c - 'a' + 10
			case c >= 'A' && c <= 'F':
				d = c - 'A' + 10
			default:
				t.Fatalf("bad hex char %q", c)
			}
			v = v<<4 | d
		}
		out[i] = v
	}
	return out
}
