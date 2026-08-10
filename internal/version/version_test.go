package version

import "testing"

func TestStringReleaseMetadata(t *testing.T) {
	originalVersion, originalCommit, originalMetadata := Version, Commit, ReleaseMetadata
	t.Cleanup(func() {
		Version, Commit, ReleaseMetadata = originalVersion, originalCommit, originalMetadata
	})

	Version = "0.7.0-rc.1"
	Commit = "0123456789abcdef0123456789abcdef01234567"
	ReleaseMetadata = "v1|0.7.0-rc.1|0123456789abcdef0123456789abcdef01234567|1"

	want := "corral 0.7.0-rc.1 (0123456789abcdef0123456789abcdef01234567, api 1)"
	if got := String(); got != want {
		t.Fatalf("String() = %q, want %q", got, want)
	}
}

func TestStringRejectsInconsistentReleaseMetadata(t *testing.T) {
	originalVersion, originalCommit, originalMetadata := Version, Commit, ReleaseMetadata
	t.Cleanup(func() {
		Version, Commit, ReleaseMetadata = originalVersion, originalCommit, originalMetadata
	})

	Version = "dev"
	Commit = "none"
	ReleaseMetadata = "v1|9.9.9|bad|99"

	if got, want := String(), "corral dev (none, api 1)"; got != want {
		t.Fatalf("String() = %q, want fallback %q", got, want)
	}
}
