package egress

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"slices"

	"github.com/nateinaction/factory/foreman/internal/proxy"
)

// resolver looks up every address a host names. *net.Resolver is one.
type resolver interface {
	LookupNetIP(ctx context.Context, network, host string) ([]netip.Addr, error)
}

// gateway reaches a destination. It is a value so a test can drive the resolution and the dial
// without depending on a host's network.
type gateway struct {
	resolve resolver
	dial    proxy.DialFunc
}

// refusedDestinationError is a destination the proxy will not reach, named by the address that refused
// it.
type refusedDestinationError struct {
	host string
	addr netip.Addr
}

// Error names the host and the address that made it unworthy of a connection.
func (e refusedDestinationError) Error() string {
	return fmt.Sprintf("egress proxy: %q is not publicly routable: it resolves to %s", e.host, e.addr)
}

// maxResolvedAddresses caps how many addresses one untrusted DNS answer may name for a dial. A
// long answer only makes the proxy check and try more destinations, so the first few are enough.
const maxResolvedAddresses = 8

// nonPublicPrefixes are ranges a routability test alone admits but the proxy must not reach:
// RFC 6598's shared space, where a tailnet lives, 0.0.0.0/8, which a host reads as itself, class E,
// and the NAT64 and 6to4 translation ranges.
var nonPublicPrefixes = []netip.Prefix{ //nolint:gochecknoglobals // a read-only set
	netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("0.0.0.0/8"),
	// 240.0.0.0/4 is reserved class E space, which Go counts as global unicast.
	netip.MustParsePrefix("240.0.0.0/4"),
	// NAT64 and 6to4 embed an IPv4 address the check never sees, so they can reach a private one.
	netip.MustParsePrefix("64:ff9b::/96"),
	netip.MustParsePrefix("2002::/16"),
}

// publiclyRoutable reports whether addr is one the proxy may dial: a valid, unzoned global unicast
// address outside every loopback, private, link-local, shared and multicast range. A zone is refused
// because Go's IsGlobalUnicast compares against the unspecified address with the zone included, so
// "::%0", which a dial reaches as the local host, would pass. It does not unmap an
// IPv4-mapped IPv6 address: the ranges above are IPv4 prefixes, which do not hold a mapped address,
// so the caller unmaps first (see lookup).
func publiclyRoutable(addr netip.Addr) bool {
	if !addr.IsValid() || addr.Zone() != "" || !addr.IsGlobalUnicast() || addr.IsPrivate() {
		return false
	}

	return !slices.ContainsFunc(nonPublicPrefixes, func(prefix netip.Prefix) bool {
		return prefix.Contains(addr)
	})
}

// dialContext resolves address's host, refuses the destination when any of its addresses is not
// publicly routable, and dials an address it resolved. Dialing the checked address is what keeps a
// second DNS answer from pointing the connection somewhere the check never saw.
func (g gateway) dialContext(ctx context.Context, network, address string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, fmt.Errorf("egress proxy: destination %q: %w", address, err)
	}

	addrs, err := g.lookup(ctx, host)
	if err != nil {
		return nil, err
	}

	var dialErr error

	for _, addr := range addrs {
		conn, err := g.dial(ctx, network, net.JoinHostPort(addr.String(), port))
		if err == nil {
			return conn, nil
		}

		dialErr = err
	}

	return nil, fmt.Errorf("egress proxy: dial %q: %w", host, dialErr)
}

// lookup resolves host to the addresses that may be dialed, or refuses it at the first address
// that may not. An address literal comes back from the resolver as itself. A name with any refused
// address is refused whole: a name that answers both publicly and privately is not to be trusted.
func (g gateway) lookup(ctx context.Context, host string) ([]netip.Addr, error) {
	addrs, err := g.resolve.LookupNetIP(ctx, "ip", host)
	if err != nil {
		return nil, fmt.Errorf("egress proxy: resolve %q: %w", host, err)
	}

	if len(addrs) == 0 {
		return nil, fmt.Errorf("egress proxy: resolve %q: no addresses", host)
	}

	checked := make([]netip.Addr, 0, min(len(addrs), maxResolvedAddresses))

	// The DNS answer is untrusted input: check and keep at most maxResolvedAddresses of it. The unmap
	// is what lets publiclyRoutable's IPv4 ranges hold an IPv4-mapped answer.
	for _, addr := range addrs[:min(len(addrs), maxResolvedAddresses)] {
		addr = addr.Unmap()
		if !publiclyRoutable(addr) {
			return nil, refusedDestinationError{host: host, addr: addr}
		}

		checked = append(checked, addr)
	}

	return checked, nil
}

// refusal finds a destination the gateway refused, reporting whether err is one.
func refusal(err error) (refusedDestinationError, bool) {
	if refused, ok := errors.AsType[refusedDestinationError](err); ok {
		return refused, true
	}

	return refusedDestinationError{}, false
}
