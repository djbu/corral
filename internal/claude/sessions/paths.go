// Package sessions implements the on-disk layout claude itself uses under
// ~/.claude/projects (verify-only in M1 — see design doc §1's comment on
// claude/sessions/paths.go, and spike/resume/NOTES.md §1, which this
// package's slug rule is transcribed from verbatim). Nothing here writes
// to that layout in M1; it exists so corral can compute where claude will
// put a transcript, for the fakeclaude self-test (step 7) and for a human
// operator inspecting ~/.claude/projects after the fact.
package sessions

import (
	"path/filepath"
	"strings"
)

// Slug converts an absolute cwd into claude's project-directory name: the
// literal path with every '/' replaced by '-' (spike/resume/NOTES.md §1 —
// "No hashing, no truncation seen — it's a literal, reversible slug").
// cwd is used as given; callers are responsible for passing an absolute,
// cleaned path (as claude itself would see its own cwd).
//
// Known limitation, documented rather than silently mishandled (NOTES.md
// §1's own caveat): a path segment that itself contains a literal '-' is
// ambiguous after slugging — "/a-b/c" and "/a/b/c" both slug to
// "-a-b-c" — because '-' is used as both the path separator's
// replacement and a character path segments may legitimately contain.
// Unslug (below) cannot and does not attempt to resolve that ambiguity;
// it exists for the common case verified in NOTES.md's own examples,
// where no segment contains a literal '-'.
func Slug(cwd string) string {
	return strings.ReplaceAll(cwd, "/", "-")
}

// Unslug reverses Slug: every '-' becomes '/'. Only exact for a slug
// produced from a cwd with no literal '-' in any path segment — see
// Slug's doc comment. It exists so a slug found on disk (e.g. while
// listing ~/.claude/projects) can be mapped back to a candidate cwd for
// display or lookup, not as a lossless inverse for arbitrary input.
func Unslug(slug string) string {
	return strings.ReplaceAll(slug, "-", "/")
}

// ProjectDir returns the directory claude uses for cwd's transcripts:
// <claudeHome>/projects/<Slug(cwd)>.
func ProjectDir(claudeHome, cwd string) string {
	return filepath.Join(claudeHome, "projects", Slug(cwd))
}

// TranscriptPath returns the exact path of a session's transcript file:
// <claudeHome>/projects/<Slug(cwd)>/<sessionID>.jsonl (spike/resume/
// NOTES.md §1: "one file per session, named <session_id>.jsonl").
func TranscriptPath(claudeHome, cwd, sessionID string) string {
	return filepath.Join(ProjectDir(claudeHome, cwd), sessionID+".jsonl")
}
