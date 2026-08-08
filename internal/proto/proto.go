// Package proto implements corral's attach-protocol wire framing (design
// doc §5.2): every frame is [1 byte type][4 byte big-endian length][payload],
// payload capped at 1 MiB. Control frames (Hello, Resize, Goodbye, Ping,
// Pong, Ready, Exit, Error, Detached) carry a JSON payload; the two data
// frames (Input, Output) carry raw bytes. Type bytes this package does not
// recognize still round-trip through ReadFrame/WriteFrame unchanged — §5.2's
// forward-compatibility rule ("unknown type tolerated and skipped") is
// applied by the caller's read loop (skip and continue), not swallowed
// inside the codec, so a caller that wants to observe an unknown frame
// still can.
package proto

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// Type is one frame's type byte (design doc §5.2's table).
type Type byte

// Client->daemon frame types.
const (
	TypeHello        Type = 0x01
	TypeInput        Type = 0x02
	TypeResize       Type = 0x03
	TypeGoodbyeClose Type = 0x04 // client->daemon: {"reason":"detach"}
	TypePing         Type = 0x05
	TypePong         Type = 0x06
)

// Daemon->client frame types.
const (
	TypeReady    Type = 0x81
	TypeOutput   Type = 0x82
	TypeExit     Type = 0x83
	TypeError    Type = 0x84
	TypeGoodbye  Type = 0x85 // daemon->client: {"reason":"detach_ack"}
	TypeDetached Type = 0x86
)

// MaxPayload is §5.2's fixed 1 MiB payload cap.
const MaxPayload = 1 << 20

// headerLen is the fixed [1 byte type][4 byte BE length] header size.
const headerLen = 5

// ErrPayloadTooLarge is returned by WriteFrame/ReadFrame when a payload
// exceeds MaxPayload.
var ErrPayloadTooLarge = errors.New("proto: payload exceeds 1 MiB limit")

// Frame is one decoded wire frame.
type Frame struct {
	Type    Type
	Payload []byte
}

// KnownTypes is every type byte this version of the protocol recognizes.
var knownTypes = map[Type]bool{
	TypeHello: true, TypeInput: true, TypeResize: true, TypeGoodbyeClose: true,
	TypePing: true, TypePong: true,
	TypeReady: true, TypeOutput: true, TypeExit: true, TypeError: true,
	TypeGoodbye: true, TypeDetached: true,
}

// IsKnown reports whether t is a type byte this package's frame-type
// constants cover. A caller's read loop uses this to implement §5.2's
// forward-compatibility rule: an unknown type is tolerated (the frame is
// still fully read off the wire, so framing stays in sync) and skipped
// (never dispatched) rather than treated as a protocol error.
func IsKnown(t Type) bool {
	return knownTypes[t]
}

// WriteFrame writes f to w as one wire frame.
func WriteFrame(w io.Writer, f Frame) error {
	if len(f.Payload) > MaxPayload {
		return ErrPayloadTooLarge
	}
	hdr := make([]byte, headerLen)
	hdr[0] = byte(f.Type)
	binary.BigEndian.PutUint32(hdr[1:], uint32(len(f.Payload)))
	if _, err := w.Write(hdr); err != nil {
		return fmt.Errorf("proto: writing frame header: %w", err)
	}
	if len(f.Payload) > 0 {
		if _, err := w.Write(f.Payload); err != nil {
			return fmt.Errorf("proto: writing frame payload: %w", err)
		}
	}
	return nil
}

// ReadFrame reads one wire frame from r. A short header or a truncated
// payload surfaces io.ErrUnexpectedEOF (or io.EOF if r closed exactly at a
// frame boundary, before any header bytes) via io.ReadFull's own contract.
// A length field over MaxPayload returns ErrPayloadTooLarge without
// attempting to read the (oversized, likely garbage) payload — the caller
// must treat this as fatal for the connection, since framing cannot be
// trusted to resynchronize.
func ReadFrame(r io.Reader) (Frame, error) {
	hdr := make([]byte, headerLen)
	if _, err := io.ReadFull(r, hdr); err != nil {
		return Frame{}, err
	}
	typ := Type(hdr[0])
	length := binary.BigEndian.Uint32(hdr[1:])
	if length > MaxPayload {
		return Frame{}, ErrPayloadTooLarge
	}
	payload := make([]byte, length)
	if length > 0 {
		if _, err := io.ReadFull(r, payload); err != nil {
			return Frame{}, fmt.Errorf("proto: reading frame payload: %w", err)
		}
	}
	return Frame{Type: typ, Payload: payload}, nil
}

// EncodeJSON marshals v for use as a control frame's Payload.
func EncodeJSON(v any) ([]byte, error) {
	return json.Marshal(v)
}

// DecodeJSON unmarshals a control frame's Payload into v.
func DecodeJSON(payload []byte, v any) error {
	return json.Unmarshal(payload, v)
}

// Hello is the client->daemon 0x01 payload.
type Hello struct {
	Rows          int    `json:"rows"`
	Cols          int    `json:"cols"`
	TakeOver      bool   `json:"take_over"`
	ClientVersion string `json:"client_version"`
}

// Resize is the client->daemon 0x03 payload.
type Resize struct {
	Rows int `json:"rows"`
	Cols int `json:"cols"`
}

// GoodbyeClose is the client->daemon 0x04 payload ({"reason":"detach"}).
type GoodbyeClose struct {
	Reason string `json:"reason"`
}

// PingPong is the shared 0x05/0x06 payload shape.
type PingPong struct {
	N int `json:"n"`
}

// Ready is the daemon->client 0x81 payload.
type Ready struct {
	SessionID    string `json:"session_id"`
	Name         string `json:"name"`
	Rows         int    `json:"rows"`
	Cols         int    `json:"cols"`
	AltScreen    bool   `json:"alt_screen"`
	RepaintBytes int    `json:"repaint_bytes"`
}

// Exit is the daemon->client 0x83 payload.
type Exit struct {
	ExitCode *int   `json:"exit_code"`
	Signal   string `json:"signal"`
	Reason   string `json:"reason"`
}

// ErrorPayload is the daemon->client 0x84 payload.
type ErrorPayload struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// Goodbye is the daemon->client 0x85 payload ({"reason":"detach_ack"}).
type Goodbye struct {
	Reason string `json:"reason"`
}

// Detached is the daemon->client 0x86 payload ({"reason":"taken_over"}).
type Detached struct {
	Reason string `json:"reason"`
}
