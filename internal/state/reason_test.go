package state

import "testing"

func TestPermissionRequestSeq(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want int64
	}{
		{"permission", `{"kind":"permission","hook_seq":41}`, 41},
		{"question is not permission evidence", `{"kind":"question","hook_seq":41}`, 0},
		{"missing seq", `{"kind":"permission"}`, 0},
		{"invalid", `{`, 0},
		{"empty", ``, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := PermissionRequestSeq(tt.raw); got != tt.want {
				t.Fatalf("PermissionRequestSeq(%q) = %d, want %d", tt.raw, got, tt.want)
			}
		})
	}
}
