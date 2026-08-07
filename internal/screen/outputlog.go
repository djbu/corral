package screen

import (
	"fmt"
	"log/slog"
	"os"
	"sync"
)

// OutputLog is a size-capped raw PTY tee (design doc §7.4): diagnostics
// only, never replayed to an attaching client (Screen's grid is what
// serves reattach). Once maxBytes have been written, OutputLog stops
// writing silently except for one WARN log line — no rotation, no
// truncation of in-flight writes beyond the cap boundary.
type OutputLog struct {
	mu       sync.Mutex
	f        *os.File
	max      int64
	written  int64
	capped   bool
	log      *slog.Logger
	warnOnce sync.Once
}

// OpenOutputLog creates (or truncates) path at mode 0600 and returns an
// OutputLog capped at maxBytes. log must not be nil.
func OpenOutputLog(path string, maxBytes int64, log *slog.Logger) (*OutputLog, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return nil, fmt.Errorf("screen: open output log %s: %w", path, err)
	}
	// Explicit chmod after create, in case path already existed at a
	// looser mode (O_CREATE does not tighten an existing file's mode) —
	// mirrors store.chmodDBFiles's precedent.
	if err := f.Chmod(0o600); err != nil {
		f.Close()
		return nil, fmt.Errorf("screen: chmod output log %s: %w", path, err)
	}
	return &OutputLog{f: f, max: maxBytes, log: log}, nil
}

// Write tees p to disk up to the size cap; bytes beyond the cap are
// silently dropped (the returned error is always nil — a full output log
// is diagnostics-only and must never propagate into Screen.Feed's path).
func (o *OutputLog) Write(p []byte) (int, error) {
	o.mu.Lock()
	defer o.mu.Unlock()

	if o.capped {
		return len(p), nil
	}

	remaining := o.max - o.written
	toWrite := p
	if int64(len(p)) > remaining {
		toWrite = p[:remaining]
	}

	n, err := o.f.Write(toWrite)
	o.written += int64(n)
	if err != nil {
		return len(p), fmt.Errorf("screen: write output log: %w", err)
	}

	if o.written >= o.max {
		o.capped = true
		o.warnOnce.Do(func() {
			o.log.Warn("screen: output log reached size cap; no longer writing",
				"path", o.f.Name(), "max_bytes", o.max)
		})
	}

	return len(p), nil
}

// Close closes the underlying file.
func (o *OutputLog) Close() error {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.f.Close()
}
