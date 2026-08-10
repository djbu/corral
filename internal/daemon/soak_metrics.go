package daemon

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"runtime/pprof"
	"time"
)

// soakMetricsEnv is intentionally undocumented runtime instrumentation for
// the repository's nightly reliability exercise. It is not a general daemon
// diagnostic facility: enabling it only permits appending aggregate counters
// to a fixed, owner-only file within state_dir.
const soakMetricsEnv = "CORRAL_SOAK_METRICS"
const soakProfilesEnv = "CORRAL_SOAK_PROFILES"

type soakSample struct {
	At         time.Time `json:"at"`
	Goroutines int       `json:"goroutines"`
	WALBytes   int64     `json:"wal_bytes"`
}

func (d *Daemon) startSoakMetrics() {
	if os.Getenv(soakMetricsEnv) != "1" {
		return
	}
	path := filepath.Join(d.cfg.StateDir, "soak-daemon.jsonl")
	write := func() {
		if err := appendSoakSample(path, filepath.Join(d.cfg.StateDir, "corral.db-wal")); err != nil {
			d.log.Warn("soak metrics sample", "err", err)
		}
		if os.Getenv(soakProfilesEnv) == "1" {
			if err := writeSoakProfiles(d.cfg.StateDir); err != nil {
				d.log.Warn("soak profiles", "err", err)
			}
		}
	}
	write()
	go func() {
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				write()
			case <-d.done:
				write()
				return
			}
		}
	}()
}

func writeSoakProfiles(stateDir string) error {
	goroutines, err := os.OpenFile(filepath.Join(stateDir, "soak-goroutine.txt"), os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("open goroutine profile: %w", err)
	}
	defer goroutines.Close()
	if err := pprof.Lookup("goroutine").WriteTo(goroutines, 2); err != nil {
		return fmt.Errorf("write goroutine profile: %w", err)
	}
	heap, err := os.OpenFile(filepath.Join(stateDir, "soak-heap.pprof"), os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("open heap profile: %w", err)
	}
	defer heap.Close()
	if err := pprof.WriteHeapProfile(heap); err != nil {
		return fmt.Errorf("write heap profile: %w", err)
	}
	return nil
}

func appendSoakSample(path, walPath string) error {
	walBytes := int64(0)
	if info, err := os.Stat(walPath); err == nil {
		walBytes = info.Size()
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("stat WAL: %w", err)
	}
	line, err := json.Marshal(soakSample{
		At:         time.Now().UTC(),
		Goroutines: runtime.NumGoroutine(),
		WALBytes:   walBytes,
	})
	if err != nil {
		return fmt.Errorf("encode sample: %w", err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("open metrics file: %w", err)
	}
	defer f.Close()
	if _, err := f.Write(append(line, '\n')); err != nil {
		return fmt.Errorf("append sample: %w", err)
	}
	return nil
}
