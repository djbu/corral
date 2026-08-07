package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// ParseBytes parses a byte-size string like "8MiB", "512KiB", "1GiB", or a
// bare integer (bytes, no suffix) into a byte count. Suffixes are binary
// (1 KiB = 1024 bytes) and case-insensitive.
func ParseBytes(s string) (int64, error) {
	trimmed := strings.TrimSpace(s)
	if trimmed == "" {
		return 0, fmt.Errorf("config: empty byte size")
	}

	i := 0
	for i < len(trimmed) && (trimmed[i] == '.' || (trimmed[i] >= '0' && trimmed[i] <= '9')) {
		i++
	}
	numPart, suffix := trimmed[:i], strings.TrimSpace(trimmed[i:])
	if numPart == "" {
		return 0, fmt.Errorf("config: invalid byte size %q", s)
	}
	val, err := strconv.ParseFloat(numPart, 64)
	if err != nil {
		return 0, fmt.Errorf("config: invalid byte size %q: %w", s, err)
	}

	var mult float64
	switch strings.ToLower(suffix) {
	case "", "b":
		mult = 1
	case "kib":
		mult = 1 << 10
	case "mib":
		mult = 1 << 20
	case "gib":
		mult = 1 << 30
	default:
		return 0, fmt.Errorf("config: unknown byte size suffix %q in %q", suffix, s)
	}
	return int64(val * mult), nil
}

// expandHome expands a leading "~" or "~/..." to the user's home directory.
// Paths not starting with "~" are returned unchanged; "~user"-style
// expansion is intentionally not supported (§8.4 only requires "~" itself).
func expandHome(path string) string {
	if path == "~" {
		if home, err := os.UserHomeDir(); err == nil {
			return home
		}
		return path
	}
	if strings.HasPrefix(path, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, path[2:])
		}
	}
	return path
}
