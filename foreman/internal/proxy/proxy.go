// Package proxy holds the plumbing foreman's loopback proxies share: the address check that keeps
// a proxy on a loopback address, the client transport that ignores the host's proxy settings, and
// the serve loop that reports why a proxy stopped. One implementation keeps every proxy bound,
// dialed and closed the same way.
package proxy

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"time"
)

// Ceilings on a proxy's own connections.
const (
	// DialTimeout bounds reaching an upstream.
	DialTimeout = 10 * time.Second
	// HeaderTimeout bounds waiting for headers, and serves both directions: an upstream's response
	// headers (a transport's ResponseHeaderTimeout) and a client's request headers (a server's
	// ReadHeaderTimeout). The proxies reuse this one value for both. The
	// body may then stream for as long as the run lasts.
	HeaderTimeout = 60 * time.Second
	// IdleTimeout bounds a kept-alive connection between requests.
	IdleTimeout = 90 * time.Second
)

// DialFunc opens a connection to an upstream. A *net.Dialer's DialContext is one.
type DialFunc func(ctx context.Context, network, address string) (net.Conn, error)

// Address checks that addr names a literal loopback IP and port, the only address a proxy may
// listen on. name is the proxy's own name, so the error says which proxy refused.
func Address(name, addr string) error {
	listen, err := netip.ParseAddrPort(addr)
	if err != nil {
		return fmt.Errorf("%s proxy address %q: %w", name, addr, err)
	}

	if !listen.Addr().IsLoopback() {
		return fmt.Errorf("%s proxy address %q: listens on loopback only", name, addr)
	}

	return nil
}

// Transport is a proxy's client transport. It ignores the proxy environment variables, so traffic a
// proxy forwards never travels through another one. dial reaches an upstream; tlsTimeout bounds the
// TLS handshake.
func Transport(dial DialFunc, tlsTimeout time.Duration) *http.Transport {
	return &http.Transport{
		Proxy:                 nil,
		DialContext:           dial,
		TLSHandshakeTimeout:   tlsTimeout,
		ResponseHeaderTimeout: HeaderTimeout,
		IdleConnTimeout:       IdleTimeout,
	}
}

// Serve runs server on ln, returning while it serves. A stop the server did not cause is a fatal
// accept error: nothing answers the run's requests afterwards, so it is logged rather than
// discarded. ErrServerClosed is the one stop with nothing to report.
func Serve(logger *slog.Logger, name string, server *http.Server, ln net.Listener) {
	go func() {
		err := server.Serve(ln)
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error(name+" proxy: serve stopped", "error", err)
		}
	}()
}
