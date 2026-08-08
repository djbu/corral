package proto

import (
	"bytes"
	"errors"
	"io"
	"testing"
)

func TestRoundTrip(t *testing.T) {
	cases := []Frame{
		{Type: TypeHello, Payload: mustJSON(t, Hello{Rows: 24, Cols: 80, TakeOver: true, ClientVersion: "0.1.0"})},
		{Type: TypeInput, Payload: []byte("hello\n")},
		{Type: TypeOutput, Payload: []byte{}},
	}
	for _, want := range cases {
		var buf bytes.Buffer
		if err := WriteFrame(&buf, want); err != nil {
			t.Fatalf("WriteFrame: %v", err)
		}
		got, err := ReadFrame(&buf)
		if err != nil {
			t.Fatalf("ReadFrame: %v", err)
		}
		if got.Type != want.Type {
			t.Fatalf("Type = %#x, want %#x", got.Type, want.Type)
		}
		if !bytes.Equal(got.Payload, want.Payload) {
			t.Fatalf("Payload = %q, want %q", got.Payload, want.Payload)
		}
	}
}

// TestHelloReadyRoundTrip verifies design doc §5.2's opening exchange at
// the JSON-field level (TestRoundTrip above only checks the wire bytes
// survive intact, not that every field decodes back to what was sent) —
// Hello client->daemon, then Ready daemon->client, exactly as
// Registry.Attach and cmd_attach.go exchange them over a real
// connection.
func TestHelloReadyRoundTrip(t *testing.T) {
	wantHello := Hello{Rows: 24, Cols: 80, TakeOver: true, ClientVersion: "0.1.0"}
	var buf bytes.Buffer
	if err := WriteFrame(&buf, Frame{Type: TypeHello, Payload: mustJSON(t, wantHello)}); err != nil {
		t.Fatalf("WriteFrame(Hello): %v", err)
	}
	helloFrame, err := ReadFrame(&buf)
	if err != nil {
		t.Fatalf("ReadFrame(Hello): %v", err)
	}
	if helloFrame.Type != TypeHello {
		t.Fatalf("Type = %#x, want TypeHello (%#x)", helloFrame.Type, TypeHello)
	}
	var gotHello Hello
	if err := DecodeJSON(helloFrame.Payload, &gotHello); err != nil {
		t.Fatalf("DecodeJSON(Hello): %v", err)
	}
	if gotHello != wantHello {
		t.Fatalf("Hello = %#v, want %#v", gotHello, wantHello)
	}

	wantReady := Ready{SessionID: "sess-1", Name: "work-1", Rows: 24, Cols: 80, AltScreen: true, RepaintBytes: 4096}
	buf.Reset()
	if err := WriteFrame(&buf, Frame{Type: TypeReady, Payload: mustJSON(t, wantReady)}); err != nil {
		t.Fatalf("WriteFrame(Ready): %v", err)
	}
	readyFrame, err := ReadFrame(&buf)
	if err != nil {
		t.Fatalf("ReadFrame(Ready): %v", err)
	}
	if readyFrame.Type != TypeReady {
		t.Fatalf("Type = %#x, want TypeReady (%#x)", readyFrame.Type, TypeReady)
	}
	var gotReady Ready
	if err := DecodeJSON(readyFrame.Payload, &gotReady); err != nil {
		t.Fatalf("DecodeJSON(Ready): %v", err)
	}
	if gotReady != wantReady {
		t.Fatalf("Ready = %#v, want %#v", gotReady, wantReady)
	}
}

func TestShortHeader(t *testing.T) {
	buf := bytes.NewReader([]byte{0x01, 0x00, 0x00}) // 3 of 5 header bytes
	if _, err := ReadFrame(buf); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("ReadFrame on short header: err = %v, want io.ErrUnexpectedEOF", err)
	}
}

func TestOversizePayloadRejected(t *testing.T) {
	var buf bytes.Buffer
	hdr := []byte{byte(TypeInput), 0, 0, 0, 0}
	// Encode a length one byte over MaxPayload directly, bypassing
	// WriteFrame's own guard, to exercise ReadFrame's independent check.
	big := uint32(MaxPayload + 1)
	hdr[1] = byte(big >> 24)
	hdr[2] = byte(big >> 16)
	hdr[3] = byte(big >> 8)
	hdr[4] = byte(big)
	buf.Write(hdr)
	if _, err := ReadFrame(&buf); !errors.Is(err, ErrPayloadTooLarge) {
		t.Fatalf("ReadFrame on oversize length: err = %v, want ErrPayloadTooLarge", err)
	}

	if err := WriteFrame(io.Discard, Frame{Type: TypeInput, Payload: make([]byte, MaxPayload+1)}); !errors.Is(err, ErrPayloadTooLarge) {
		t.Fatalf("WriteFrame on oversize payload: err = %v, want ErrPayloadTooLarge", err)
	}
}

func TestTruncatedPayload(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteFrame(&buf, Frame{Type: TypeInput, Payload: []byte("0123456789")}); err != nil {
		t.Fatalf("WriteFrame: %v", err)
	}
	// Drop the last 3 payload bytes, leaving a header that promises more
	// than the reader will actually get.
	truncated := buf.Bytes()[:buf.Len()-3]
	if _, err := ReadFrame(bytes.NewReader(truncated)); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("ReadFrame on truncated payload: err = %v, want io.ErrUnexpectedEOF", err)
	}
}

func TestUnknownTypeToleratedAndSkipped(t *testing.T) {
	const unknownType Type = 0x7f
	if IsKnown(unknownType) {
		t.Fatalf("IsKnown(%#x) = true, want false", unknownType)
	}

	var buf bytes.Buffer
	if err := WriteFrame(&buf, Frame{Type: unknownType, Payload: []byte("future extension")}); err != nil {
		t.Fatalf("WriteFrame: %v", err)
	}
	// A known frame right after it, to prove the reader stayed in sync
	// (skip-and-continue, not "unknown byte corrupts framing").
	if err := WriteFrame(&buf, Frame{Type: TypePing, Payload: mustJSON(t, PingPong{N: 7})}); err != nil {
		t.Fatalf("WriteFrame: %v", err)
	}

	got1, err := ReadFrame(&buf)
	if err != nil {
		t.Fatalf("ReadFrame (unknown): %v", err)
	}
	if got1.Type != unknownType || string(got1.Payload) != "future extension" {
		t.Fatalf("got %#v, want unknown frame preserved intact", got1)
	}

	got2, err := ReadFrame(&buf)
	if err != nil {
		t.Fatalf("ReadFrame (known, after unknown): %v", err)
	}
	if got2.Type != TypePing {
		t.Fatalf("Type after skipping unknown = %#x, want TypePing", got2.Type)
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := EncodeJSON(v)
	if err != nil {
		t.Fatalf("EncodeJSON: %v", err)
	}
	return b
}
