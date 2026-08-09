package daemon

import (
	"net"
	"reflect"
	"testing"
)

// TestHostsForListen_SpecificHost covers a specific host:port address: the
// SAN host is exactly that host, no interface enumeration involved.
func TestHostsForListen_SpecificHost(t *testing.T) {
	fakeAddrs := func() ([]net.Addr, error) {
		t.Fatal("interfaceAddrs must not be called for a specific host")
		return nil, nil
	}
	got := hostsForListen("192.168.1.5:8443", fakeAddrs)
	want := []string{"192.168.1.5"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("hostsForListen = %v, want %v", got, want)
	}
}

// TestHostsForListen_Wildcard covers a wildcard bind (":8443"): the
// loopback interface address is filtered out and only the non-loopback IP
// is returned. The fake interfaceAddrs is injected so this never depends on
// the real machine's interfaces.
func TestHostsForListen_Wildcard(t *testing.T) {
	_, loopbackNet, err := net.ParseCIDR("127.0.0.1/8")
	if err != nil {
		t.Fatalf("ParseCIDR loopback: %v", err)
	}
	_, lanNet, err := net.ParseCIDR("192.168.1.5/24")
	if err != nil {
		t.Fatalf("ParseCIDR lan: %v", err)
	}
	fakeAddrs := func() ([]net.Addr, error) {
		return []net.Addr{
			&net.IPNet{IP: net.ParseIP("127.0.0.1"), Mask: loopbackNet.Mask},
			&net.IPNet{IP: net.ParseIP("192.168.1.5"), Mask: lanNet.Mask},
		}, nil
	}
	got := hostsForListen(":8443", fakeAddrs)
	want := []string{"192.168.1.5"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("hostsForListen = %v, want %v (loopback filtered out)", got, want)
	}
}

// TestHostsForListen_Malformed covers a malformed address with no port
// separator: SplitHostPort fails, and hostsForListen returns nil rather
// than erroring — the caller (tls.Listen) will surface the real error.
func TestHostsForListen_Malformed(t *testing.T) {
	fakeAddrs := func() ([]net.Addr, error) {
		t.Fatal("interfaceAddrs must not be called for a malformed address")
		return nil, nil
	}
	got := hostsForListen("nonsense", fakeAddrs)
	if got != nil {
		t.Errorf("hostsForListen(%q) = %v, want nil", "nonsense", got)
	}
}
