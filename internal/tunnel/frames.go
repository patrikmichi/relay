// Package tunnel is the connector agent: it holds one outbound WebSocket to the
// broker, unseals the requests the gateway sends and runs the ones its local
// allowlist permits against HTTP and MCP targets inside the customer's network.
package tunnel

import (
	"encoding/binary"
	"errors"
	"fmt"
)

// Protocol constants of ctun.v1.
const (
	ProtoVersion = 1
	// MaxPayload is the largest frame payload.
	MaxPayload = 256 << 10
	headerLen  = 12
	// Subprotocol is the WebSocket subprotocol the agent asks for.
	Subprotocol = "ctun.v1"
)

// FrameType is the second byte of a frame.
type FrameType uint8

// Frame types.
const (
	FrameHello    FrameType = 1
	FrameHelloAck FrameType = 2
	FrameOpen     FrameType = 3
	FrameData     FrameType = 4
	FrameEnd      FrameType = 5
	FrameCancel   FrameType = 6
	FrameError    FrameType = 7
	FrameStatus   FrameType = 8
	FrameDrain    FrameType = 9
	FrameWindow   FrameType = 10
)

// FlagHeader marks the DATA frame that opens a response: its payload is the
// encapsulated response key followed by the sealed response header.
const FlagHeader byte = 0x01

var frameNames = map[FrameType]string{
	FrameHello: "HELLO", FrameHelloAck: "HELLO_ACK", FrameOpen: "OPEN", FrameData: "DATA", FrameEnd: "END",
	FrameCancel: "CANCEL", FrameError: "ERROR", FrameStatus: "STATUS", FrameDrain: "DRAIN", FrameWindow: "WINDOW",
}

func (t FrameType) String() string {
	if n, ok := frameNames[t]; ok {
		return n
	}
	return fmt.Sprintf("type(%d)", uint8(t))
}

// Frame is one binary WebSocket message.
type Frame struct {
	Type     FrameType
	Flags    byte
	StreamID uint32
	Payload  []byte
}

// Frame errors.
var (
	ErrFrameTooLarge = errors.New("frame payload is larger than 256 KiB")
	ErrFrameVersion  = errors.New("unsupported frame version")
	ErrFrameShort    = errors.New("frame is shorter than its header says")
	ErrFrameType     = errors.New("unknown frame type")
)

// Marshal encodes the frame: ver, type, flags, reserved, stream id, length,
// payload.
func (f Frame) Marshal() ([]byte, error) {
	if len(f.Payload) > MaxPayload {
		return nil, ErrFrameTooLarge
	}
	out := make([]byte, headerLen, headerLen+len(f.Payload))
	out[0], out[1], out[2] = ProtoVersion, byte(f.Type), f.Flags
	binary.BigEndian.PutUint32(out[4:], f.StreamID)
	binary.BigEndian.PutUint32(out[8:], uint32(len(f.Payload)))
	return append(out, f.Payload...), nil
}

// ParseFrame decodes one message. A version other than 1, an unknown type, a
// reserved byte that is not zero, an oversized length or trailing bytes are
// refused.
func ParseFrame(b []byte) (Frame, error) {
	if len(b) < headerLen {
		return Frame{}, ErrFrameShort
	}
	if b[0] != ProtoVersion {
		return Frame{}, ErrFrameVersion
	}
	if _, ok := frameNames[FrameType(b[1])]; !ok {
		return Frame{}, ErrFrameType
	}
	if b[3] != 0 {
		return Frame{}, errors.New("reserved frame byte is not zero")
	}
	n := binary.BigEndian.Uint32(b[8:])
	if n > MaxPayload {
		return Frame{}, ErrFrameTooLarge
	}
	if int(n) != len(b)-headerLen {
		return Frame{}, ErrFrameShort
	}
	return Frame{Type: FrameType(b[1]), Flags: b[2], StreamID: binary.BigEndian.Uint32(b[4:]), Payload: append([]byte(nil), b[headerLen:]...)}, nil
}
