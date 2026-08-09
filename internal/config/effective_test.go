package config

import (
	"path/filepath"
	"testing"
)

// TestEffective_ClientTokenNeverSurfaced is the dedicated rule-2 test
// (design doc m5.md §7): client.host and client.cacert are ordinary
// resolved config and belong in Effective's output exactly like any other
// key, but client.token is a secret and must be absent from BOTH the
// Values and Sources maps — Effective is the shared rendering path behind
// `corral config` and the daemon's GET /v1/config, so a leak here would be
// a leak on the wire.
func TestEffective_ClientTokenNeverSurfaced(t *testing.T) {
	home := withHome(t)
	writeFile(t, filepath.Join(home, ".corral", "config.toml"), ""+
		"[client]\n"+
		"host = \"corral.example.com:8443\"\n"+
		"token = \"crl_super_secret\"\n"+
		"cacert = \"~/certs/ca.pem\"\n")

	dir := t.TempDir()
	eff, err := Effective(dir)
	if err != nil {
		t.Fatalf("Effective: %v", err)
	}

	if got := eff.Values["client.host"]; got != "corral.example.com:8443" {
		t.Errorf(`Values["client.host"] = %q, want %q`, got, "corral.example.com:8443")
	}
	wantCACert := filepath.Join(home, "certs", "ca.pem")
	if got := eff.Values["client.cacert"]; got != wantCACert {
		t.Errorf(`Values["client.cacert"] = %q, want %q`, got, wantCACert)
	}

	if _, ok := eff.Values["client.token"]; ok {
		t.Errorf(`Values["client.token"] present = %q, want absent`, eff.Values["client.token"])
	}
	if _, ok := eff.Sources["client.token"]; ok {
		t.Errorf(`Sources["client.token"] present = %q, want absent`, eff.Sources["client.token"])
	}

	// Sanity check that the secret value itself never shows up anywhere in
	// either map (belt-and-suspenders beyond the key-absence checks above).
	for k, v := range eff.Values {
		if v == "crl_super_secret" {
			t.Errorf("Values[%q] = %q: client.token's secret leaked into Values", k, v)
		}
	}
}
