// Package tlsbootstrap generates or loads the self-signed TLS certificate
// corral's opt-in TCP listener uses (design doc m5.md §5). TLS is mandatory
// on that listener — there is no plaintext-TCP code path to misconfigure
// into existence — and this package is the sole source of the
// *tls.Certificate it presents.
//
// On first use it mints a fresh ECDSA P-256 leaf, self-signed, valid for
// ten years, and writes it to <dir>/cert.pem and <dir>/key.pem (mode 0600,
// parent dir mode 0700). On every subsequent call it loads that same pair
// unchanged. This stability matters: the corral CLI client pins the
// daemon's own cert as its trusted CA (§7) rather than relying on a system
// trust store, so the cert handed back on daemon restart N must be
// byte-identical to the one handed back on restart 1, or every client's
// pin breaks.
//
// Cert validity (NotBefore/NotAfter) is derived from an injected
// clock.Clock rather than time.Now(), matching the daemon-wide
// no-wall-clock-in-persisted-logic discipline: it lets tests assert exact
// validity windows without sleeping or racing the real clock.
package tlsbootstrap

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"

	"github.com/danielbecerra/corral/internal/clock"
)

// certFileName and keyFileName are the fixed basenames written under dir.
const (
	certFileName = "cert.pem"
	keyFileName  = "key.pem"
)

// certValidityYears is the self-signed leaf's lifetime. Ten years is long
// enough that an operator running corral as a home server never has to
// think about renewal; there is no CA relationship to revoke or rotate
// against, so a long validity carries none of the usual "long-lived cert"
// risk.
const certValidityYears = 10

// LoadOrGenerate returns the TLS certificate the daemon's TCP listener
// should present, generating and persisting a fresh self-signed one under
// dir if <dir>/cert.pem and <dir>/key.pem don't both already exist.
//
// clk supplies the clock used for the generated cert's validity window
// (NotBefore = clk.Now(), NotAfter = ten years later) — production code
// must never call time.Now() directly, so callers pass clock.Real() and
// tests pass a clocktest.FakeClock for deterministic assertions.
//
// dir is created (mode 0700) if absent. hosts are additional Subject
// Alternative Names to embed in a freshly generated cert — each is
// classified as an IP address (net.ParseIP succeeds) or a DNS name
// otherwise, and merged with the fixed defaults (localhost, 127.0.0.1,
// ::1), de-duplicating so a caller re-passing "localhost" is harmless.
// hosts is ignored when an existing pair is loaded, since SANs are baked
// into the persisted cert.
//
// If exactly one of the two files exists, the pair is treated as absent
// and both are regenerated: a half-written pair (e.g. from a prior
// interrupted write) is unusable as-is, and there is no way to recover a
// missing key from a lone cert file, so regenerating both is the only
// option that doesn't require the operator to intervene.
//
// If both files exist but are corrupt (e.g. truncated or not valid
// PEM/DER), LoadOrGenerate returns an error from tls.LoadX509KeyPair
// rather than silently regenerating: "both present" is the load path by
// contract, and silently discarding a cert an operator may have
// deliberately placed there (see the tls_cert/tls_key override case in
// §5) would be surprising. Recovering from that state is an operator
// action (delete the pair, or fix the override paths), not this
// function's job.
func LoadOrGenerate(clk clock.Clock, dir string, hosts []string) (*tls.Certificate, error) {
	certPath := filepath.Join(dir, certFileName)
	keyPath := filepath.Join(dir, keyFileName)

	_, certErr := os.Stat(certPath)
	_, keyErr := os.Stat(keyPath)
	if certErr == nil && keyErr == nil {
		cert, err := tls.LoadX509KeyPair(certPath, keyPath)
		if err != nil {
			return nil, fmt.Errorf("tlsbootstrap: loading existing cert/key pair: %w", err)
		}
		return &cert, nil
	}

	if err := generate(clk, dir, certPath, keyPath, hosts); err != nil {
		return nil, err
	}

	cert, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		return nil, fmt.Errorf("tlsbootstrap: loading newly generated cert/key pair: %w", err)
	}
	return &cert, nil
}

// generate mints a fresh self-signed ECDSA P-256 leaf per §5's parameters
// and writes it to certPath/keyPath (creating dir as needed).
func generate(clk clock.Clock, dir, certPath, keyPath string, hosts []string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("tlsbootstrap: creating tls dir: %w", err)
	}

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return fmt.Errorf("tlsbootstrap: generating ECDSA key: %w", err)
	}

	notBefore := clk.Now()
	template := &x509.Certificate{
		// A fixed serial number is deliberate, not an oversight: serial
		// uniqueness matters for a CA issuing many certs to distinguish
		// them from one another, but this is a single self-signed leaf
		// regenerated in place (never two certs coexisting under the same
		// trust relationship), so there is nothing for a fixed serial to
		// collide with. Fixing it also keeps generated output
		// deterministic, which is convenient for tests.
		SerialNumber: big.NewInt(1),
		Subject: pkix.Name{
			CommonName: "corral",
		},
		NotBefore:             notBefore,
		NotAfter:              notBefore.AddDate(certValidityYears, 0, 0),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  false,
	}
	populateSANs(template, hosts)

	certDER, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		return fmt.Errorf("tlsbootstrap: creating self-signed certificate: %w", err)
	}

	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return fmt.Errorf("tlsbootstrap: marshaling EC private key: %w", err)
	}

	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})

	if err := os.WriteFile(certPath, certPEM, 0o600); err != nil {
		return fmt.Errorf("tlsbootstrap: writing cert file: %w", err)
	}
	// The key file is written 0600 explicitly (never world/group
	// readable) — the same mode as the cert, but called out because this
	// is the file whose exposure actually matters.
	if err := os.WriteFile(keyPath, keyPEM, 0o600); err != nil {
		return fmt.Errorf("tlsbootstrap: writing key file: %w", err)
	}

	return nil
}

// populateSANs fills template's DNSNames/IPAddresses with the fixed
// defaults (localhost, 127.0.0.1, ::1) plus every entry in hosts,
// classifying each host as an IP (net.ParseIP succeeds) or a DNS name
// otherwise, and de-duplicating both lists. It never resolves a hostname —
// SANs are exactly what the caller and the fixed defaults specify, keeping
// certificate generation pure and offline.
func populateSANs(template *x509.Certificate, hosts []string) {
	dnsSeen := make(map[string]bool)
	ipSeen := make(map[string]bool)

	addDNS := func(name string) {
		if !dnsSeen[name] {
			dnsSeen[name] = true
			template.DNSNames = append(template.DNSNames, name)
		}
	}
	addIP := func(ip net.IP) {
		key := ip.String()
		if !ipSeen[key] {
			ipSeen[key] = true
			template.IPAddresses = append(template.IPAddresses, ip)
		}
	}

	addDNS("localhost")
	addIP(net.ParseIP("127.0.0.1"))
	addIP(net.ParseIP("::1"))

	for _, h := range hosts {
		if ip := net.ParseIP(h); ip != nil {
			addIP(ip)
		} else {
			addDNS(h)
		}
	}
}
