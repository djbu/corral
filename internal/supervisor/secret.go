package supervisor

import (
	"crypto/rand"
	"encoding/hex"
)

// newSessionSecret returns a 256-bit hex secret for hook authentication
// (Amendment: CORRAL_SESSION_SECRET). Generated fresh per spawn, held only
// in memory (LiveSession.Secret) — never persisted, never logged.
func newSessionSecret() (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}
