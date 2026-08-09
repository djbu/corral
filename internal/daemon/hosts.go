package daemon

import "net"

// hostsFor returns the extra SAN hosts to embed in the self-signed cert for
// a given listen address. LoadOrGenerate always adds loopback defaults;
// this supplies the NON-loopback names a remote client needs. For a
// specific host (e.g. "192.168.1.5:8443" or "myhost:8443") it's just that
// host. For a wildcard/unspecified bind (":8443", "0.0.0.0:8443",
// "[::]:8443") there is no host in the address, so it enumerates this
// machine's non-loopback interface IPs — that's what lets the phone reach
// 192.168.1.x with a matching cert. Never resolves DNS; offline and
// deterministic given the interface set.
func hostsFor(listen string) []string { return hostsForListen(listen, net.InterfaceAddrs) }

// hostsForListen is hostsFor's testable body: interfaceAddrs is injected so
// tests can assert against a fake interface set rather than the real
// machine's, which would make the test non-deterministic and
// environment-dependent.
func hostsForListen(listen string, interfaceAddrs func() ([]net.Addr, error)) []string {
	host, _, err := net.SplitHostPort(listen)
	if err != nil {
		return nil // malformed; loopback defaults still apply, tls.Listen will surface the real error
	}
	switch host {
	case "", "0.0.0.0", "::":
		// wildcard bind — enumerate non-loopback interface IPs
		addrs, err := interfaceAddrs()
		if err != nil {
			return nil
		}
		var out []string
		for _, a := range addrs {
			ipNet, ok := a.(*net.IPNet)
			if !ok || ipNet.IP.IsLoopback() || ipNet.IP.IsLinkLocalUnicast() || ipNet.IP.IsLinkLocalMulticast() {
				continue
			}
			out = append(out, ipNet.IP.String())
		}
		return out
	default:
		return []string{host}
	}
}
