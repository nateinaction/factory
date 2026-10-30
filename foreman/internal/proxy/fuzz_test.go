package proxy

import (
	"net/netip"
	"testing"
)

// FuzzAddress checks that an address Address accepts is a literal loopback IP and port.
func FuzzAddress(f *testing.F) {
	for _, seed := range []string{
		"127.0.0.1:8080", "[::1]:80", "0.0.0.0:80", "localhost:80", "127.0.0.1", "[::ffff:127.0.0.1]:1",
		"127.0.0.1:99999", "10.0.0.1:80", "", "[fe80::1%eth0]:1",
	} {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, addr string) {
		if Address("fuzz", addr) != nil {
			return
		}

		listen, err := netip.ParseAddrPort(addr)
		if err != nil || !listen.Addr().IsLoopback() {
			t.Fatalf("Address accepted %q, which is not a literal loopback address and port", addr)
		}
	})
}
