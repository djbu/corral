package sessions

import "testing"

// TestSlugMatchesNotesExample is design doc §10.2's slug round-trip
// test: the exact example recorded in spike/resume/NOTES.md §1.
func TestSlugMatchesNotesExample(t *testing.T) {
	const (
		cwd  = "/Users/danielbecerra/code/research/corral/spike/resume/sandbox"
		want = "-Users-danielbecerra-code-research-corral-spike-resume-sandbox"
	)

	got := Slug(cwd)
	if got != want {
		t.Fatalf("Slug(%q) = %q, want %q", cwd, got, want)
	}

	// Round-trip: this specific example has no literal '-' in any path
	// segment, so Unslug(Slug(cwd)) must exactly recover cwd — see
	// Slug's doc comment on why that is not true in general.
	if back := Unslug(got); back != cwd {
		t.Fatalf("Unslug(Slug(%q)) = %q, want %q", cwd, back, cwd)
	}
}

// TestSlugRoundTripTableDriven exercises a few more cwds with no literal
// '-' in any segment, plus documents (rather than silently passing over)
// the known ambiguous case.
func TestSlugRoundTripTableDriven(t *testing.T) {
	cases := []struct {
		name string
		cwd  string
	}{
		{"repo root", "/Users/danielbecerra/code/research/corral"},
		{"single segment", "/tmp"},
		{"root", "/"},
		{"trailing content", "/home/ci/work/project/sub"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			slug := Slug(tc.cwd)
			if got := Unslug(slug); got != tc.cwd {
				t.Fatalf("Unslug(Slug(%q)) = %q, want %q", tc.cwd, got, tc.cwd)
			}
		})
	}
}

// TestSlugAmbiguousWithLiteralHyphen documents the known limitation
// explicitly (Slug's doc comment): a path segment containing a literal
// '-' collides with the path-separator encoding. This test asserts the
// documented behavior (the collision happens), not a false claim that
// Unslug resolves it.
func TestSlugAmbiguousWithLiteralHyphen(t *testing.T) {
	const (
		withHyphenSegment = "/a-b/c"
		withoutHyphen     = "/a/b/c"
	)
	if Slug(withHyphenSegment) != Slug(withoutHyphen) {
		t.Fatalf("expected %q and %q to slug identically (the documented ambiguity), got %q and %q",
			withHyphenSegment, withoutHyphen, Slug(withHyphenSegment), Slug(withoutHyphen))
	}
}

func TestProjectDirAndTranscriptPath(t *testing.T) {
	const (
		claudeHome = "/home/ci/.claude"
		cwd        = "/home/ci/work/project"
		sessionID  = "11111111-1111-1111-1111-111111111111"
	)

	wantDir := "/home/ci/.claude/projects/-home-ci-work-project"
	if got := ProjectDir(claudeHome, cwd); got != wantDir {
		t.Fatalf("ProjectDir = %q, want %q", got, wantDir)
	}

	wantPath := wantDir + "/" + sessionID + ".jsonl"
	if got := TranscriptPath(claudeHome, cwd, sessionID); got != wantPath {
		t.Fatalf("TranscriptPath = %q, want %q", got, wantPath)
	}
}
