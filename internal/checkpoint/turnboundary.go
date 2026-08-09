package checkpoint

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
)

// tailWindow bounds how much of a transcript LastRecordIsCompletedTurn
// reads. Checkpoint runs across every live session on daemon shutdown and
// transcripts can be multi-MB, so this reads only the tail rather than the
// whole file line-by-line.
const tailWindow = 256 * 1024

// rec is the minimal shape LastRecordIsCompletedTurn needs from a
// transcript line. Real records carry many more top-level keys (cwd,
// sessionId, …) and the assistant message object carries more fields
// (role, content, stop_sequence, stop_details, usage, …); only type,
// isSidechain, and message.stop_reason matter here.
type rec struct {
	Type        string `json:"type"`
	IsSidechain bool   `json:"isSidechain"`
	Message     struct {
		StopReason string `json:"stop_reason"`
	} `json:"message"`
}

// LastRecordIsCompletedTurn reports whether the session transcript at
// transcriptPath ends at a completed conversation turn — i.e. the last
// main-thread conversation record is an assistant record whose
// message.stop_reason is exactly "end_turn".
//
// The transcript is append-only JSONL (one JSON object per line). Only
// type ∈ {"user","assistant"} with isSidechain != true are main-thread
// conversation records; every other type (system, attachment, mode,
// last-prompt, ai-title, pr-link, file-history-*, queue-operation, …) is
// bookkeeping, and the file's literal last line is frequently one of those —
// so a naive "read the last line" is wrong. We scan BACKWARD for the last
// conversation record.
//
// It errs toward false: a missing/empty file, a window containing no
// conversation record, or an unreadable record all yield false. Only a
// genuine I/O failure on a file that exists returns a non-nil error (and
// false alongside it).
func LastRecordIsCompletedTurn(transcriptPath string) (bool, error) {
	f, err := os.Open(transcriptPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return false, err
	}
	size := info.Size()
	if size == 0 {
		return false, nil
	}

	readSize := size
	startedAtZero := true
	if size > tailWindow {
		readSize = tailWindow
		startedAtZero = false
	}

	buf := make([]byte, readSize)
	if _, err := f.Seek(size-readSize, io.SeekStart); err != nil {
		return false, err
	}
	if _, err := io.ReadFull(f, buf); err != nil {
		return false, err
	}

	if !startedAtZero {
		// The first line in the window is a partial record (we started
		// mid-file); discard everything up to and including its newline.
		if idx := bytes.IndexByte(buf, '\n'); idx != -1 {
			buf = buf[idx+1:]
		} else {
			// No newline at all in the window means the window couldn't
			// find even one complete line boundary — nothing usable.
			buf = nil
		}
	}

	lines := bytes.Split(buf, []byte("\n"))
	for i := len(lines) - 1; i >= 0; i-- {
		line := bytes.TrimSpace(lines[i])
		if len(line) == 0 {
			continue
		}
		var r rec
		if err := json.Unmarshal(line, &r); err != nil {
			continue
		}
		if r.IsSidechain {
			continue
		}
		if r.Type != "user" && r.Type != "assistant" {
			continue
		}
		return r.Type == "assistant" && r.Message.StopReason == "end_turn", nil
	}

	return false, nil
}
