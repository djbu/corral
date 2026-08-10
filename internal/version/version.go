// Package version holds build-time version metadata for corral. Version and
// Commit are populated via -ldflags at build time (see Makefile); they default
// to "dev" and "none" for local `go build`/`go test` runs.
package version

import (
	"strconv"
	"strings"
)

// Version is the corral release version, e.g. "0.1.0". Set via:
//
//	-ldflags "-X github.com/djbu/corral/internal/version.Version=..."
var Version = "dev"

// Commit is the git commit hash corral was built from. Set via:
//
//	-ldflags "-X github.com/djbu/corral/internal/version.Commit=..."
var Commit = "none"

// ReleaseMetadata is a compact, machine-verifiable copy of the release
// identity embedded in every distributed binary. GoReleaser sets it to
// "v1|<version>|<full-commit>|<api>". String only trusts it when all fields
// agree with the independently injected values and the compiled API constant.
var ReleaseMetadata = ""

// APIVersion is the daemon<->client attach-protocol and API version. It is
// bumped whenever a wire-incompatible change is made to internal/proto or the
// HTTP API surface. It is a Go const (not a build-time var) because it is a
// compatibility fact about the binary's code, not something the build knows
// better than the source.
const APIVersion = 1

// String returns the human-readable version string printed by
// `corral --version`, e.g. "corral 0.1.0 (none, api 1)".
func String() string {
	parts := strings.Split(ReleaseMetadata, "|")
	if len(parts) == 4 && parts[0] == "v1" && parts[1] == Version &&
		parts[2] == Commit && parts[3] == strconv.Itoa(APIVersion) {
		return "corral " + parts[1] + " (" + parts[2] + ", api " + parts[3] + ")"
	}
	return "corral " + Version + " (" + Commit + ", api " + strconv.Itoa(APIVersion) + ")"
}
