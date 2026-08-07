package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestParseBytes(t *testing.T) {
	cases := []struct {
		in      string
		want    int64
		wantErr bool
	}{
		{"8MiB", 8 * 1 << 20, false},
		{"512KiB", 512 * 1 << 10, false},
		{"1GiB", 1 << 30, false},
		{"100", 100, false},
		{"100B", 100, false},
		{"0", 0, false},
		{"1.5MiB", int64(1.5 * (1 << 20)), false},
		{"", 0, true},
		{"MiB", 0, true},
		{"8XiB", 0, true},
	}
	for _, tc := range cases {
		got, err := ParseBytes(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Errorf("ParseBytes(%q) = %d, want error", tc.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("ParseBytes(%q) unexpected error: %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("ParseBytes(%q) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

func TestExpandHome(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skipf("no home directory available: %v", err)
	}

	cases := []struct {
		in   string
		want string
	}{
		{"~", home},
		{"~/.corral/config.toml", filepath.Join(home, ".corral", "config.toml")},
		{"/absolute/path", "/absolute/path"},
		{"relative/path", "relative/path"},
	}
	for _, tc := range cases {
		if got := expandHome(tc.in); got != tc.want {
			t.Errorf("expandHome(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
