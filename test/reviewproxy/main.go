// Command reviewproxy is an opt-in browser-test bridge. The real daemon side
// remains TLS and is verified against its pinned certificate; the local
// browser side is plain loopback HTTP for automation environments that refuse
// self-signed HTTPS interstitials. It is never built into corral.
package main

import (
	"crypto/tls"
	"crypto/x509"
	"flag"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
)

func main() {
	listen := flag.String("listen", "", "loopback listen address")
	upstream := flag.String("upstream", "", "TLS upstream URL")
	ca := flag.String("ca", "", "pinned upstream certificate")
	flag.Parse()
	if *listen == "" || *upstream == "" || *ca == "" {
		log.Fatal("-listen, -upstream and -ca are required")
	}
	target, err := url.Parse(*upstream)
	if err != nil {
		log.Fatal(err)
	}
	pem, err := os.ReadFile(*ca)
	if err != nil {
		log.Fatal(err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(pem) {
		log.Fatal("invalid CA certificate")
	}
	proxy := httputil.NewSingleHostReverseProxy(target)
	proxy.Transport = &http.Transport{TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: roots}}
	log.Fatal(http.ListenAndServe(*listen, proxy))
}
