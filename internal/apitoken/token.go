// Package apitoken mints and verifies corral's API bearer tokens (design
// doc m5.md §3.1). It is pure crypto with no store or http dependency: it
// knows nothing about persistence or transport, only how to generate a
// high-entropy secret, render it, and compare it to a stored hash in
// constant time.
//
// A token is a 256-bit random value, crypto/rand, rendered as URL-safe
// base64 with a short human prefix for greppability in a config file:
// crl_<43-char-base64>. The prefix is cosmetic (helps a user recognize a
// corral token in ~/.corral/config.toml); it is part of the plaintext and
// part of what is hashed.
//
// Hash choice: SHA-256, not bcrypt/argon2. Decided and recorded (m5.md
// §3.1): password hashes are deliberately slow to defend low-entropy human
// secrets against offline brute force. An API token is a 256-bit
// uniformly-random value — there is no dictionary and no feasible brute
// force regardless of hash speed, so a slow KDF buys nothing and adds a
// per-request CPU cost on the hot path of every authenticated request. A
// single SHA-256 is the correct primitive for high-entropy bearer tokens
// (this is what GitHub/Stripe-style token systems do).
//
// The plaintext is never persisted, never logged, never returned by any
// read endpoint. It is generated once, at mint time, and it is the
// caller's job (corral token create) to print it exactly once; only the
// hash is stored.
package apitoken

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"fmt"
)

// tokenPrefix is prepended to every minted token's plaintext. It is
// cosmetic (greppability in a config file), not a security boundary — it
// is part of the plaintext and part of what gets hashed.
const tokenPrefix = "crl_"

// Mint generates a new 256-bit random token, returning its plaintext
// (crl_-prefixed, shown to the operator exactly once) and its hash (the
// only form that should ever be persisted).
func Mint() (plaintext string, hash string, err error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", "", fmt.Errorf("apitoken: reading random bytes: %w", err)
	}
	plaintext = tokenPrefix + base64.RawURLEncoding.EncodeToString(buf)
	return plaintext, hashToken(plaintext), nil
}

// Verify reports whether plaintext hashes to hash, using a constant-time
// comparison so a timing side channel can't leak how many hash bytes
// matched. It never panics regardless of what garbage is passed for
// either argument (see FuzzVerify).
func Verify(plaintext, hash string) bool {
	return subtle.ConstantTimeCompare([]byte(hashToken(plaintext)), []byte(hash)) == 1
}

// HashForLookup hashes plaintext the same way Mint does, for callers (the
// store) that need to look a token up by its hash rather than mint a new
// one. Exported so store/tokens.go has a single hash definition to import
// rather than duplicating the sha256+hex logic.
func HashForLookup(plaintext string) string {
	return hashToken(plaintext)
}

// hashToken returns the hex-encoded SHA-256 digest of plaintext.
func hashToken(plaintext string) string {
	sum := sha256.Sum256([]byte(plaintext))
	return hex.EncodeToString(sum[:])
}
