package main

import (
	"encoding/binary"
	"fmt"
	"io"
	"sync"
)

// Wire protocol, client -> daemon direction only. The daemon -> client
// direction is unframed: it's just raw PTY output bytes (replay buffer,
// then live stream), because the client never needs to distinguish
// message types in that direction for this spike.
//
// Frame: [1 byte type][4 byte big-endian length][payload]
type frameType byte

const (
	frameAttach frameType = 'A' // payload: uint16 rows, uint16 cols (initial size on attach)
	frameData   frameType = 'D' // payload: raw bytes to write to the PTY master (keystrokes)
	frameResize frameType = 'R' // payload: uint16 rows, uint16 cols (SIGWINCH-driven resize)
)

const maxFramePayload = 1 << 20 // 1MiB sanity cap

func writeFrame(w io.Writer, t frameType, payload []byte) error {
	hdr := make([]byte, 5)
	hdr[0] = byte(t)
	binary.BigEndian.PutUint32(hdr[1:], uint32(len(payload)))
	if _, err := w.Write(hdr); err != nil {
		return err
	}
	if len(payload) == 0 {
		return nil
	}
	_, err := w.Write(payload)
	return err
}

func readFrame(r io.Reader) (frameType, []byte, error) {
	hdr := make([]byte, 5)
	if _, err := io.ReadFull(r, hdr); err != nil {
		return 0, nil, err
	}
	t := frameType(hdr[0])
	n := binary.BigEndian.Uint32(hdr[1:])
	if n > maxFramePayload {
		return 0, nil, fmt.Errorf("frame payload too large: %d", n)
	}
	payload := make([]byte, n)
	if n > 0 {
		if _, err := io.ReadFull(r, payload); err != nil {
			return 0, nil, err
		}
	}
	return t, payload, nil
}

func encodeSize(rows, cols uint16) []byte {
	b := make([]byte, 4)
	binary.BigEndian.PutUint16(b[0:2], rows)
	binary.BigEndian.PutUint16(b[2:4], cols)
	return b
}

func decodeSize(b []byte) (rows, cols uint16, err error) {
	if len(b) < 4 {
		return 0, 0, fmt.Errorf("size payload too short: %d bytes", len(b))
	}
	return binary.BigEndian.Uint16(b[0:2]), binary.BigEndian.Uint16(b[2:4]), nil
}

// scrollback is a fixed-capacity ring buffer of the most recent N bytes of
// PTY output, plus the "current attached writer" bookkeeping. Attach()
// atomically snapshots the buffer and swaps in the new live writer under
// one lock so the PTY reader goroutine can never interleave a write
// between the snapshot and the subscription (no dropped or duplicated
// bytes across a reattach).
type scrollback struct {
	mu   sync.Mutex
	buf  []byte
	cap  int
	live io.Writer // nil if nobody is attached
}

func newScrollback(capBytes int) *scrollback {
	return &scrollback{cap: capBytes}
}

// Feed is called by the single PTY-reader goroutine for every chunk read
// from the PTY master. It appends to the ring buffer and, if a client is
// currently attached, mirrors the bytes to it.
func (s *scrollback) Feed(p []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.buf = append(s.buf, p...)
	if over := len(s.buf) - s.cap; over > 0 {
		s.buf = s.buf[over:]
	}
	if s.live != nil {
		if _, err := s.live.Write(p); err != nil {
			s.live = nil
		}
	}
}

// Attach snapshots the current buffer and registers w as the live writer
// for subsequent Feed calls. Returns the snapshot to replay to the new
// client before it starts receiving live data.
func (s *scrollback) Attach(w io.Writer) []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	snap := make([]byte, len(s.buf))
	copy(snap, s.buf)
	s.live = w
	return snap
}

// Detach clears the live writer iff it is still w (avoids a race where a
// newer attach already replaced it).
func (s *scrollback) Detach(w io.Writer) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.live == w {
		s.live = nil
	}
}
