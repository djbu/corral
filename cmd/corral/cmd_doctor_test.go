package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/danielbecerra/corral/internal/claude/automode"
)

// TestWriteDoctorReport covers the four report branches: foreign hooks
// none/listed × Auto Mode active/unknown, and that the unknown path surfaces
// the raw probe output for diagnosis while the active path emits the verbatim
// A.6 sentence.
func TestWriteDoctorReport(t *testing.T) {
	t.Run("no foreign hooks, auto mode active", func(t *testing.T) {
		var buf bytes.Buffer
		writeDoctorReport(&buf, nil, automode.Result{
			Status: automode.StatusActive, AllowCount: 17, SoftDenyCount: 65, HardDenyCount: 1,
		})
		out := buf.String()
		mustContain(t, out, "Foreign hooks")
		mustContain(t, out, "  none")
		mustContain(t, out, automode.ActiveMessage)
		mustContain(t, out, "allow=17 soft_deny=65 hard_deny=1")
		mustContain(t, out, "project/local setting_sources not inspected")
	})

	t.Run("listed foreign hooks, auto mode unknown with raw", func(t *testing.T) {
		var buf bytes.Buffer
		writeDoctorReport(&buf, []string{"SessionStart", "Stop"}, automode.Result{
			Status: automode.StatusUnknown, Raw: "claude: unknown command",
		})
		out := buf.String()
		mustContain(t, out, "  - SessionStart")
		mustContain(t, out, "  - Stop")
		mustContain(t, out, "corral does not manage them")
		mustContain(t, out, "could not determine")
		mustContain(t, out, "raw: claude: unknown command")
		if strings.Contains(out, automode.ActiveMessage) {
			t.Error("unknown Auto Mode must not emit the active message (false 'unsupervised' claim)")
		}
	})
}

func mustContain(t *testing.T, haystack, needle string) {
	t.Helper()
	if !strings.Contains(haystack, needle) {
		t.Errorf("output missing %q\n---\n%s", needle, haystack)
	}
}
