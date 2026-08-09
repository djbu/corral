package apitoken

import (
	"strings"
	"testing"
)

// TestMint_DistinctPlaintexts mints many tokens and asserts none collide
// and every plaintext carries the crl_ prefix (design doc m5.md §14).
func TestMint_DistinctPlaintexts(t *testing.T) {
	const n = 1000
	seen := make(map[string]bool, n)

	for i := 0; i < n; i++ {
		pt, _, err := Mint()
		if err != nil {
			t.Fatalf("Mint: %v", err)
		}
		if !strings.HasPrefix(pt, tokenPrefix) {
			t.Fatalf("Mint plaintext %q missing %q prefix", pt, tokenPrefix)
		}
		// Pins the design doc's literal crl_<43-char-base64> contract
		// (m5.md §3.1: 32 random bytes, RawURLEncoding has no padding) so a
		// future change to the encoding or byte count fails loudly here
		// rather than silently shrinking the token's entropy.
		if wantLen := len(tokenPrefix) + 43; len(pt) != wantLen {
			t.Fatalf("Mint plaintext %q has length %d, want %d (crl_ + 43-char base64 of 32 bytes)", pt, len(pt), wantLen)
		}
		if seen[pt] {
			t.Fatalf("Mint produced duplicate plaintext %q after %d mints", pt, i)
		}
		seen[pt] = true
	}
}

// TestVerify_FreshMintRoundTrips checks the direct success path: a
// freshly minted (plaintext, hash) pair verifies true.
func TestVerify_FreshMintRoundTrips(t *testing.T) {
	pt, hash, err := Mint()
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	if !Verify(pt, hash) {
		t.Fatalf("Verify(%q, %q) = false, want true", pt, hash)
	}
}

// TestVerify_TamperedPlaintextFails checks that changing even one byte of
// the plaintext fails verification against the original hash.
func TestVerify_TamperedPlaintextFails(t *testing.T) {
	pt, hash, err := Mint()
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	tampered := pt[:len(pt)-1] + "x"
	if tampered == pt {
		t.Fatalf("tampering produced an identical string, fixture bug")
	}
	if Verify(tampered, hash) {
		t.Fatalf("Verify(tampered, hash) = true, want false")
	}
}

// TestVerify_EmptyInputsFail checks that an empty plaintext or an empty
// hash never verifies, matching the "no accidental match" expectation for
// missing/malformed bearer headers upstream (middleware_auth, step 34+).
func TestVerify_EmptyInputsFail(t *testing.T) {
	_, hash, err := Mint()
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	if Verify("", hash) {
		t.Fatalf(`Verify("", hash) = true, want false`)
	}
	pt, _, err := Mint()
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	if Verify(pt, "") {
		t.Fatalf(`Verify(pt, "") = true, want false`)
	}
}

// FuzzVerify asserts the one invariant that matters for arbitrary
// attacker-controlled input reaching Verify via a bearer header: it never
// panics, regardless of what garbage is supplied for either the
// plaintext or the stored hash.
func FuzzVerify(f *testing.F) {
	pt, hash, err := Mint()
	if err != nil {
		f.Fatalf("Mint: %v", err)
	}
	f.Add(pt, hash)
	f.Add("", "")
	f.Add("crl_not-a-real-token", "not-hex-either")

	f.Fuzz(func(t *testing.T, plaintext, hash string) {
		_ = Verify(plaintext, hash)
	})
}
