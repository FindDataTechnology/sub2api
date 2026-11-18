package lanes

import (
	"encoding/binary"
	"errors"
	"fmt"
)

// 豆包语音（openspeech）V3 二进制帧编解码。
//
// 协议形态（2026-10-18 真 key 实测确证，与官方文档一致）：
//
//	[0] 高 4 位 = protocol version（恒 1）；低 4 位 = header size（恒 1，即 4 字节）
//	[1] 高 4 位 = message type；低 4 位 = message type specific flags
//	[2] 高 4 位 = serialization（0=raw 二进制 / 1=JSON）；低 4 位 = compression
//	[3] reserved
//	[4..7]  event 编号（int32 BE）——仅「带 event 的帧」有此段
//	session 类上行/下行帧额外携带：uint32(len)+session_id，之后才是 payload
//	音频响应帧（Audio-only）额外携带 session_id 但**不带**外层 payload size
//
// 实测踩过的坑（勿回退）：
//   - event 字段必须是 int32；传字符串服务端直接拒
//   - 音频帧 serialization=0（裸二进制 mp3），不是 base64
//   - Audio-only 帧的 payload 前置**确有** uint32 payload size 字段，与其余帧一致；
//     该值等于剩余字节数（6/6 帧实测精确匹配）。曾误判为「ID3 长度前缀」，实为
//     同一 size 字段——按 size 切片后首帧自然以 "ID3" 起始。
const (
	doubaoProtocolVersion = 1
	doubaoHeaderSizeWords = 1 // header 长度以 4 字节为单位，1 => 4 字节

	doubaoMsgTypeFullClient = 1  // 完整客户端请求
	doubaoMsgTypeFullServer = 9  // 完整服务端响应
	doubaoMsgTypeAudioOnly  = 11 // 纯音频响应
	doubaoMsgTypeError      = 15 // 错误帧

	doubaoFlagWithEvent = 4 // message type specific flags：携带 event

	doubaoSerializationRaw  = 0
	doubaoSerializationJSON = 1

	// 事件编号（官方 Event 定义）
	doubaoEventStartConnection      = 1
	doubaoEventFinishConnection     = 2
	doubaoEventConnectionStarted    = 50
	doubaoEventConnectionFailed     = 51
	doubaoEventConnectionFinished   = 52
	doubaoEventStartSession         = 100
	doubaoEventCancelSession        = 101
	doubaoEventFinishSession        = 102
	doubaoEventSessionStarted       = 150
	doubaoEventSessionCanceled      = 151
	doubaoEventSessionFinished      = 152
	doubaoEventSessionFailed        = 153
	doubaoEventTaskRequest          = 200
	doubaoEventTTSSentenceStart     = 350
	doubaoEventTTSSentenceEnd       = 351
	doubaoEventTTSResponse          = 352
)

// doubaoFrame 解出的一个上行/下行帧。
type doubaoFrame struct {
	messageType   int
	flags         int
	serialization int
	event         int
	sessionID     string
	payload       []byte
	errorCode     int
}

// isError 该帧是否为错误帧（msg_type=15）。
func (f *doubaoFrame) isError() bool {
	return f.messageType == doubaoMsgTypeError
}

// errMessage 错误帧的文本（payload 为 JSON 或纯文本）。
func (f *doubaoFrame) errMessage() string {
	if len(f.payload) == 0 {
		return "upstream error frame without payload"
	}
	return string(f.payload)
}

// buildDoubaoFrame 构造带 event 的上行帧。
// sessionID 非空时插入 uint32(len)+session_id 段（session 类事件要求）。
func buildDoubaoFrame(messageType, flags, event int, sessionID string, payload []byte) []byte {
	header := []byte{
		byte(doubaoProtocolVersion<<4 | doubaoHeaderSizeWords),
		byte(messageType<<4 | flags),
		byte(doubaoSerializationJSON << 4), // 上下行文本/JJSON 均为 JSON，无压缩
		0,
	}
	buf := make([]byte, 0, len(header)+4+len(sessionID)+4+len(payload)+8)
	buf = append(buf, header...)
	buf = binary.BigEndian.AppendUint32(buf, uint32(event))
	if sessionID != "" {
		buf = binary.BigEndian.AppendUint32(buf, uint32(len(sessionID)))
		buf = append(buf, sessionID...)
	}
	buf = binary.BigEndian.AppendUint32(buf, uint32(len(payload)))
	buf = append(buf, payload...)
	return buf
}

// parseDoubaoFrame 解析一个下行帧。
//
// 布局差异（实测）：
//   - 错误帧（msg_type=15）：header 后直接是 error code（4B），再是 payload，
//     不带 event、不带 payload size。
//   - Audio-only（msg_type=11）：header + event + uint32(len)+session_id + 裸音频，
//     不带外层 payload size。
//   - 其余带 event 的帧（如 full-server）：header + event +
//     [uint32(len)+session_id] + uint32(len)+payload。
func parseDoubaoFrame(b []byte) (*doubaoFrame, error) {
	if len(b) < 4 {
		return nil, errors.New("frame too short")
	}
	version := int(b[0] >> 4)
	headerWords := int(b[0] & 0x0f)
	if version != doubaoProtocolVersion {
		return nil, fmt.Errorf("unsupported protocol version %d", version)
	}
	headerLen := headerWords * 4
	if headerLen < 4 || len(b) < headerLen {
		return nil, errors.New("invalid header size")
	}

	f := &doubaoFrame{
		messageType:   int(b[1] >> 4),
		flags:         int(b[1] & 0x0f),
		serialization: int(b[2] >> 4),
	}
	off := headerLen

	if f.messageType == doubaoMsgTypeError {
		if len(b) < off+4 {
			return nil, errors.New("error frame without code")
		}
		f.errorCode = int(binary.BigEndian.Uint32(b[off:]))
		f.payload = b[off+4:]
		return f, nil
	}

	if f.flags&doubaoFlagWithEvent != 0 {
		if len(b) < off+4 {
			return nil, errors.New("frame without event")
		}
		f.event = int(binary.BigEndian.Uint32(b[off:]))
		off += 4
	}

	// Audio-only：event 后是 session_id 段，再是 uint32 payload size + 裸音频。
	if f.messageType == doubaoMsgTypeAudioOnly {
		if len(b) < off+4 {
			return nil, errors.New("audio frame without session id length")
		}
		sidLen := int(binary.BigEndian.Uint32(b[off:]))
		off += 4
		if sidLen < 0 || len(b) < off+sidLen {
			return nil, errors.New("audio frame session id overruns body")
		}
		f.sessionID = string(b[off : off+sidLen])
		off += sidLen
		if len(b) < off+4 {
			return nil, errors.New("audio frame without payload length")
		}
		payLen := int(binary.BigEndian.Uint32(b[off:]))
		off += 4
		if payLen < 0 || len(b) < off+payLen {
			return nil, errors.New("audio payload length overruns body")
		}
		f.payload = b[off : off+payLen]
		return f, nil
	}

	// session 类下行帧（SessionStarted/SessionFinished 等）带 session_id 段。
	// 判据：有剩余字节且首个 uint32 解释为 sidLen 后仍有 payload 长度字段。
	// 官方定义里 SessionStarted/Finished/Failed/Canceled 都带 sid 段。
	if isDoubaoSessionScopedEvent(f.event) {
		if len(b) < off+4 {
			return nil, errors.New("session frame without session id length")
		}
		sidLen := int(binary.BigEndian.Uint32(b[off:]))
		off += 4
		if sidLen < 0 || len(b) < off+sidLen {
			return nil, errors.New("session frame session id overruns body")
		}
		f.sessionID = string(b[off : off+sidLen])
		off += sidLen
	}

	if len(b) < off+4 {
		return nil, errors.New("frame without payload length")
	}
	payLen := int(binary.BigEndian.Uint32(b[off:]))
	off += 4
	if payLen < 0 || len(b) < off+payLen {
		return nil, errors.New("payload length overruns body")
	}
	f.payload = b[off : off+payLen]
	return f, nil
}

// isDoubaoSessionScopedEvent 该下行事件是否携带 session_id 段。
// 依据官方字节表：SessionStarted / SessionFinished / SessionFailed /
// SessionCanceled 均为「full-server response + event + len(sid)+sid + len(payload)」。
func isDoubaoSessionScopedEvent(event int) bool {
	switch event {
	case doubaoEventSessionStarted, doubaoEventSessionFinished,
		doubaoEventSessionFailed, doubaoEventSessionCanceled:
		return true
	}
	return false
}

// doubaoAudioChunks 把若干 Audio-only 帧的 payload 依序拼接。
//
// 实测：首帧 payload 以 "ID3" 起始（ID3v2.4 标签），后续帧为裸 MPEG 帧；
// 直接按序 concat 即得标准 mp3（file(1) 判读通过）。
func doubaoAudioChunks(frames []*doubaoFrame) []byte {
	total := 0
	for _, f := range frames {
		total += len(f.payload)
	}
	out := make([]byte, 0, total)
	for _, f := range frames {
		out = append(out, f.payload...)
	}
	return out
}
