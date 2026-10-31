package egress

import (
	"net/netip"
	"testing"
)

// reachesLocalNetwork reports whether addr is in a range a dial reaches the local host or network by.
func reachesLocalNetwork(addr netip.Addr) bool {
	return addr.IsLoopback() || addr.IsPrivate() || addr.IsLinkLocalUnicast() || addr.IsMulticast() ||
		addr.IsUnspecified() || addr.Zone() != ""
}

// FuzzPubliclyRoutable checks that no address in a range the proxy must never reach is admitted.
func FuzzPubliclyRoutable(f *testing.F) {
	for _, seed := range []string{
		"8.8.8.8", "127.0.0.1", "10.0.0.1", "169.254.169.254", "100.64.0.1", "0.0.0.0", "240.0.0.1",
		"::1", "fe80::1", "fc00::1", "::%0", "64:ff9b::7f00:1", "2002:7f00:1::", "2001:4860:4860::8888", "",
	} {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, raw string) {
		addr, err := netip.ParseAddr(raw)
		if err != nil || addr.Is4In6() {
			return // lookup unmaps an IPv4-mapped address before it asks, see publiclyRoutable
		}

		if !publiclyRoutable(addr) {
			return
		}

		if reachesLocalNetwork(addr) {
			t.Fatalf("publiclyRoutable admitted %q", raw)
		}
	})
}

// FuzzParseAPI checks that an API URL parseAPI accepts always names a scheme and a host.
func FuzzParseAPI(f *testing.F) {
	for _, seed := range []string{
		"", "https://api.github.com", "http://127.0.0.1:8080/v1", "api.github.com", "https://", "//host",
		"https://user:pw@host/%zz", "\x00",
	} {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, raw string) {
		parsed, err := parseAPI(raw)
		if err != nil {
			return
		}

		if raw == "" {
			if parsed != nil {
				t.Fatalf("parseAPI(%q) returned %v, want nil", raw, parsed)
			}

			return
		}

		if parsed.Scheme == "" || parsed.Host == "" {
			t.Fatalf("parseAPI(%q) accepted a URL without a scheme and host: %v", raw, parsed)
		}
	})
}
