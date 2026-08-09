// Package version holds build-time version metadata for corral. Version and
// Commit are populated via -ldflags at build time (see Makefile); they default
// to "dev" and "none" for local `go build`/`go test` runs.
package version

import "strconv"

// Version is the corral release version, e.g. "0.1.0". Set via:
//
//	-ldflags "-X github.com/djbu/corral/internal/version.Version=..."
var Version = "dev"

// Commit is the git commit hash corral was built from. Set via:
//
//	-ldflags "-X github.com/djbu/corral/internal/version.Commit=..."
var Commit = "none"

// APIVersion is the daemon<->client attach-protocol and API version. It is
// bumped whenever a wire-incompatible change is made to internal/proto or the
// HTTP API surface. It is a Go const (not a build-time var) because it is a
// compatibility fact about the binary's code, not something the build knows
// better than the source.
const APIVersion = 1

// String returns the human-readable version string printed by
// `corral --version`, e.g. "corral 0.1.0 (none, api 1)".
func String() string {
	return "corral " + Version + " (" + Commit + ", api " + strconv.Itoa(APIVersion) + ")"
}
