package dashboard

import (
	"regexp"
	"strconv"
	"testing"

	"github.com/djbu/corral/internal/version"
)

// apiVersionRe matches app.js's `const API_VERSION = <int>;` declaration.
var apiVersionRe = regexp.MustCompile(`const API_VERSION = (\d+)`)

// TestAppJS_APIVersionMatchesVersionPackage guards against exactly the
// silent-breakage scenario app.js's own doc comment on API_VERSION warns
// about: if version.APIVersion is ever bumped (a wire-incompatible HTTP
// API change) without updating the constant embedded in this JS file,
// every /v1/* request the dashboard makes would start failing the
// version-handshake with no compile-time signal — this test is that
// signal.
func TestAppJS_APIVersionMatchesVersionPackage(t *testing.T) {
	b, err := assets.ReadFile("app.js")
	if err != nil {
		t.Fatalf("ReadFile(app.js): %v", err)
	}

	m := apiVersionRe.FindSubmatch(b)
	if m == nil {
		t.Fatalf("app.js: could not find `const API_VERSION = <int>` declaration")
	}

	got, err := strconv.Atoi(string(m[1]))
	if err != nil {
		t.Fatalf("parsing API_VERSION value %q: %v", m[1], err)
	}

	if got != version.APIVersion {
		t.Fatalf("app.js API_VERSION = %d, want %d (version.APIVersion) — update app.js's API_VERSION constant", got, version.APIVersion)
	}
}
