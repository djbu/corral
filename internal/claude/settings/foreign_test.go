package settings

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// TestForeignHookEvents covers the user-scope foreign-hook read: sorted event
// names from a real settings.json, and every best-effort failure mode
// yielding nil (empty home, missing file, malformed JSON, no hooks key).
func TestForeignHookEvents(t *testing.T) {
	t.Run("empty claudeHome yields nil", func(t *testing.T) {
		if got := ForeignHookEvents(""); got != nil {
			t.Fatalf("got %v, want nil", got)
		}
	})

	t.Run("missing file yields nil", func(t *testing.T) {
		if got := ForeignHookEvents(t.TempDir()); got != nil {
			t.Fatalf("got %v, want nil", got)
		}
	})

	t.Run("malformed json yields nil", func(t *testing.T) {
		dir := t.TempDir()
		writeSettings(t, dir, "{not json")
		if got := ForeignHookEvents(dir); got != nil {
			t.Fatalf("got %v, want nil", got)
		}
	})

	t.Run("no hooks key yields nil", func(t *testing.T) {
		dir := t.TempDir()
		writeSettings(t, dir, `{"model":"opus"}`)
		if got := ForeignHookEvents(dir); got != nil {
			t.Fatalf("got %v, want nil", got)
		}
	})

	t.Run("hooks present returns sorted event names", func(t *testing.T) {
		dir := t.TempDir()
		writeSettings(t, dir, `{"hooks":{"UserPromptSubmit":[],"SessionStart":[],"Stop":[]}}`)
		got := ForeignHookEvents(dir)
		want := []string{"SessionStart", "Stop", "UserPromptSubmit"}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("got %v, want %v (sorted)", got, want)
		}
	})
}

func writeSettings(t *testing.T, dir, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "settings.json"), []byte(content), 0o600); err != nil {
		t.Fatalf("writing settings.json: %v", err)
	}
}
