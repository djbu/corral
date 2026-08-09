package tlsbootstrap

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/danielbecerra/corral/internal/clock/clocktest"
)

// fakeNow anchors every test's clock so validity assertions are exact and
// reproducible.
var fakeNow = time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC)

func newFakeClock() *clocktest.FakeClock {
	return clocktest.NewFake(fakeNow)
}

// parseLeaf loads path as a PEM certificate and returns its parsed
// x509.Certificate, failing the test on any error.
func parseLeaf(t *testing.T, path string) *x509.Certificate {
	t.Helper()
	certPEM, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading cert file: %v", err)
	}
	block, _ := pem.Decode(certPEM)
	if block == nil {
		t.Fatalf("no PEM block found in %s", path)
	}
	leaf, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatalf("parsing leaf certificate: %v", err)
	}
	return leaf
}

// TestGenerateOnAbsent covers case 1: an empty temp dir produces a usable
// certificate and both files land on disk.
func TestGenerateOnAbsent(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "tls")
	clk := newFakeClock()

	cert, err := LoadOrGenerate(clk, dir, nil)
	if err != nil {
		t.Fatalf("LoadOrGenerate: %v", err)
	}
	if cert == nil || len(cert.Certificate) == 0 {
		t.Fatalf("LoadOrGenerate returned an unusable certificate: %+v", cert)
	}
	if _, err := x509.ParseCertificate(cert.Certificate[0]); err != nil {
		t.Fatalf("re-parsing generated leaf: %v", err)
	}

	if _, err := os.Stat(filepath.Join(dir, certFileName)); err != nil {
		t.Fatalf("cert.pem missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, keyFileName)); err != nil {
		t.Fatalf("key.pem missing: %v", err)
	}
}

// TestFilePermissions covers case 2: cert.pem and key.pem are 0600, the
// tls dir itself is 0700.
func TestFilePermissions(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "tls")
	clk := newFakeClock()

	if _, err := LoadOrGenerate(clk, dir, nil); err != nil {
		t.Fatalf("LoadOrGenerate: %v", err)
	}

	dirInfo, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("stat dir: %v", err)
	}
	if perm := dirInfo.Mode().Perm(); perm != 0o700 {
		t.Errorf("tls dir mode = %o, want 0700", perm)
	}

	for _, name := range []string{certFileName, keyFileName} {
		info, err := os.Stat(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("stat %s: %v", name, err)
		}
		if perm := info.Mode().Perm(); perm != 0o600 {
			t.Errorf("%s mode = %o, want 0600", name, perm)
		}
	}
}

// TestDeterministicValidity covers case 3: NotBefore/NotAfter come from
// the injected clock, not time.Now().
func TestDeterministicValidity(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "tls")
	clk := newFakeClock()

	if _, err := LoadOrGenerate(clk, dir, nil); err != nil {
		t.Fatalf("LoadOrGenerate: %v", err)
	}

	leaf := parseLeaf(t, filepath.Join(dir, certFileName))
	if !leaf.NotBefore.Equal(fakeNow) {
		t.Errorf("NotBefore = %v, want %v", leaf.NotBefore, fakeNow)
	}
	wantNotAfter := fakeNow.AddDate(10, 0, 0)
	if !leaf.NotAfter.Equal(wantNotAfter) {
		t.Errorf("NotAfter = %v, want %v", leaf.NotAfter, wantNotAfter)
	}
}

// TestSANs covers case 4: fixed defaults plus caller-supplied hosts land
// in the right DNS/IP bucket, with no duplicate localhost.
func TestSANs(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "tls")
	clk := newFakeClock()
	hosts := []string{"home.example", "192.168.1.10", "localhost"}

	if _, err := LoadOrGenerate(clk, dir, hosts); err != nil {
		t.Fatalf("LoadOrGenerate: %v", err)
	}

	leaf := parseLeaf(t, filepath.Join(dir, certFileName))

	wantDNS := map[string]bool{"localhost": true, "home.example": true}
	if len(leaf.DNSNames) != len(wantDNS) {
		t.Errorf("DNSNames = %v, want exactly %v (no duplicate localhost)", leaf.DNSNames, wantDNS)
	}
	for _, name := range leaf.DNSNames {
		if !wantDNS[name] {
			t.Errorf("unexpected DNS SAN %q", name)
		}
		delete(wantDNS, name)
	}
	if len(wantDNS) != 0 {
		t.Errorf("missing DNS SANs: %v", wantDNS)
	}

	wantIPs := map[string]bool{"127.0.0.1": true, "::1": true, "192.168.1.10": true}
	if len(leaf.IPAddresses) != len(wantIPs) {
		t.Errorf("IPAddresses = %v, want exactly %v", leaf.IPAddresses, wantIPs)
	}
	for _, ip := range leaf.IPAddresses {
		if !wantIPs[ip.String()] {
			t.Errorf("unexpected IP SAN %q", ip.String())
		}
		delete(wantIPs, ip.String())
	}
	if len(wantIPs) != 0 {
		t.Errorf("missing IP SANs: %v", wantIPs)
	}
}

// TestKeyUsage covers case 5: KeyUsage/ExtKeyUsage/IsCA match §5 exactly.
func TestKeyUsage(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "tls")
	clk := newFakeClock()

	if _, err := LoadOrGenerate(clk, dir, nil); err != nil {
		t.Fatalf("LoadOrGenerate: %v", err)
	}

	leaf := parseLeaf(t, filepath.Join(dir, certFileName))

	if leaf.KeyUsage&x509.KeyUsageDigitalSignature == 0 {
		t.Errorf("KeyUsage missing DigitalSignature: %v", leaf.KeyUsage)
	}
	if leaf.KeyUsage&x509.KeyUsageKeyEncipherment == 0 {
		t.Errorf("KeyUsage missing KeyEncipherment: %v", leaf.KeyUsage)
	}

	foundServerAuth := false
	for _, eku := range leaf.ExtKeyUsage {
		if eku == x509.ExtKeyUsageServerAuth {
			foundServerAuth = true
		}
	}
	if !foundServerAuth {
		t.Errorf("ExtKeyUsage missing ExtKeyUsageServerAuth: %v", leaf.ExtKeyUsage)
	}

	if leaf.IsCA {
		t.Errorf("IsCA = true, want false")
	}
}

// TestIdempotentLoad covers case 6: a second call on the same dir loads
// rather than regenerates, so cert.pem is byte-identical — the property
// the client's pinned-CA trust (§7) depends on.
func TestIdempotentLoad(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "tls")
	clk := newFakeClock()

	if _, err := LoadOrGenerate(clk, dir, nil); err != nil {
		t.Fatalf("first LoadOrGenerate: %v", err)
	}
	first, err := os.ReadFile(filepath.Join(dir, certFileName))
	if err != nil {
		t.Fatalf("reading cert after first call: %v", err)
	}

	if _, err := LoadOrGenerate(clk, dir, nil); err != nil {
		t.Fatalf("second LoadOrGenerate: %v", err)
	}
	second, err := os.ReadFile(filepath.Join(dir, certFileName))
	if err != nil {
		t.Fatalf("reading cert after second call: %v", err)
	}

	if !bytes.Equal(first, second) {
		t.Errorf("cert.pem changed across calls; LoadOrGenerate regenerated instead of loading")
	}
}

// TestRoundTripsAsServerCert covers case 7: the generated certificate
// actually works as a TLS server cert, and pinning its leaf as a client CA
// (the §7 mechanism) validates the connection.
func TestRoundTripsAsServerCert(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "tls")
	clk := newFakeClock()

	cert, err := LoadOrGenerate(clk, dir, nil)
	if err != nil {
		t.Fatalf("LoadOrGenerate: %v", err)
	}

	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		t.Fatalf("parsing leaf: %v", err)
	}

	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{*cert}}
	srv.StartTLS()
	defer srv.Close()

	pool := x509.NewCertPool()
	pool.AddCert(leaf)
	client := &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{
				RootCAs:    pool,
				ServerName: "localhost",
				// Pin verification's notion of "now" to the same fake
				// clock the cert's validity window was generated from.
				// Without this, the handshake's certificate-expiry check
				// falls back to the real wall clock and this test would
				// only pass while fakeNow's 10-year window happens to
				// contain the actual current date — exactly the kind of
				// wall-clock leak this package exists to avoid.
				Time: func() time.Time { return fakeNow },
			},
		},
	}

	resp, err := client.Get(srv.URL)
	if err != nil {
		t.Fatalf("GET %s: %v", srv.URL, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusOK)
	}
}

// TestLoadPair covers the daemon.tls_cert/tls_key operator-override path:
// loading the exact same two files LoadOrGenerate just wrote succeeds, and
// a nonexistent path is a plain error rather than a silent regeneration
// (LoadPair never writes anything, unlike LoadOrGenerate).
func TestLoadPair(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "tls")
	clk := newFakeClock()

	if _, err := LoadOrGenerate(clk, dir, nil); err != nil {
		t.Fatalf("LoadOrGenerate: %v", err)
	}
	certPath := filepath.Join(dir, certFileName)
	keyPath := filepath.Join(dir, keyFileName)

	cert, err := LoadPair(certPath, keyPath)
	if err != nil {
		t.Fatalf("LoadPair: %v", err)
	}
	if cert == nil || len(cert.Certificate) == 0 {
		t.Fatalf("LoadPair returned an unusable certificate: %+v", cert)
	}

	if _, err := LoadPair(filepath.Join(dir, "nonexistent-cert.pem"), keyPath); err == nil {
		t.Fatalf("LoadPair with a nonexistent cert path: want error, got nil")
	}
}

// TestHalfPairRegenerates covers case 8: only cert.pem present (and
// garbage) with key.pem absent is treated as "absent" and both files are
// regenerated fresh rather than erroring out on the unusable half-pair.
func TestHalfPairRegenerates(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, certFileName), []byte("not a real cert"), 0o600); err != nil {
		t.Fatalf("seeding garbage cert.pem: %v", err)
	}
	clk := newFakeClock()

	cert, err := LoadOrGenerate(clk, dir, nil)
	if err != nil {
		t.Fatalf("LoadOrGenerate with half pair: %v", err)
	}
	if len(cert.Certificate) == 0 {
		t.Fatalf("LoadOrGenerate returned an unusable certificate")
	}

	leaf := parseLeaf(t, filepath.Join(dir, certFileName))
	if !leaf.NotBefore.Equal(fakeNow) {
		t.Errorf("regenerated cert NotBefore = %v, want %v (garbage cert.pem should have been overwritten)", leaf.NotBefore, fakeNow)
	}
	if _, err := os.Stat(filepath.Join(dir, keyFileName)); err != nil {
		t.Fatalf("key.pem missing after regeneration: %v", err)
	}
}
