// Package openrouter is the loopback forwarding proxy that holds a run's OpenRouter key. It adds
// Authorization to every request, forwards only to OpenRouter's API, and reads the usage every
// response reports. The model calls the proxy with no key of its own, so the key never reaches it.
package openrouter

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"path"
	"sync"
	"time"

	"github.com/nateinaction/factory/foreman/internal/proxy"
	"github.com/nateinaction/factory/foreman/internal/proxyauth"
	"github.com/nateinaction/factory/foreman/internal/security"
)

// The proxy's fixed destination and default address.
const (
	// Upstream is the only destination the proxy forwards to.
	Upstream = "https://openrouter.ai"
	// APIPrefix is the only path prefix the proxy forwards.
	APIPrefix = "/api/v1"
	// Loopback is the address the proxy binds by default: loopback only, on a free port.
	Loopback = "127.0.0.1:0"
)

// tlsTimeout bounds the TLS handshake with OpenRouter.
const tlsTimeout = 10 * time.Second

// Proxy is a running forwarding proxy.
type Proxy struct {
	key       string
	token     string
	url       string
	upstream  *url.URL
	server    *http.Server
	transport *http.Transport
	reverse   *httputil.ReverseProxy
	log       *slog.Logger
	mu        sync.Mutex
	usage     Usage
	refusals  map[security.Outcome]int
}

// StartWithLogger binds a proxy holding key on addr, forwarding only to OpenRouter's API. The proxy
// answers only a request carrying token, the random value minted for this run, so no other local
// caller can spend the key. addr must be a loopback address. A bind or parse failure is an error: the
// run has no other way to reach a model. A request without the run token is refused and emits one
// security event (internal/security); logger nil discards those diagnostics.
func StartWithLogger(ctx context.Context, key, token, addr string, logger *slog.Logger) (*Proxy, error) {
	return startAt(ctx, key, token, addr, Upstream, logger)
}

// startAt is StartWithLogger with the destination named. Only StartWithLogger's OpenRouter
// destination is trusted; specs name a stand-in so none reaches the network.
func startAt(
	ctx context.Context, key, token, addr, upstream string, logger *slog.Logger,
) (*Proxy, error) {
	if token == "" {
		return nil, errors.New("openrouter proxy: a per-run token is required")
	}

	if err := proxy.Address("openrouter", addr); err != nil {
		return nil, err
	}

	dest, err := url.Parse(upstream)
	if err != nil {
		return nil, fmt.Errorf("parse upstream: %w", err)
	}

	ln, err := (&net.ListenConfig{}).Listen(ctx, "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("bind openrouter proxy: %w", err)
	}

	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}

	p := &Proxy{
		key: key, token: token, url: "http://" + ln.Addr().String() + APIPrefix, upstream: dest, log: logger,
		refusals: map[security.Outcome]int{},
	}
	// The transport ignores the proxy environment variables, so the key can never be sent anywhere
	// but OpenRouter.
	p.transport = proxy.Transport((&net.Dialer{Timeout: proxy.DialTimeout}).DialContext, tlsTimeout)
	p.reverse = p.reverseProxy()
	// Inbound request headers take the upstream header timeout: one ceiling serves both directions.
	p.server = &http.Server{Handler: http.HandlerFunc(p.serve), ReadHeaderTimeout: proxy.HeaderTimeout}

	// The serve loop reports anything but a clean stop through the default logger, as the reverse
	// proxy's upstream failures are.
	proxy.Serve(slog.Default(), "openrouter", p.server, ln)

	return p, nil
}

// reverseProxy forwards a request through the standard reverse proxy: it strips every hop-by-hop
// header, including those a Connection header names, flushes each streamed write as it arrives, and
// wraps the response body so the usage it reports is recorded.
func (p *Proxy) reverseProxy() *httputil.ReverseProxy {
	return &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.URL = p.forwardURL(pr.In)
			// The outbound URL names the destination, so this server's own host must not travel too.
			pr.Out.Host = ""
			// The client transport negotiates compression, so the usage reader sees plain text.
			pr.Out.Header.Del("Accept-Encoding")
			pr.Out.Header.Set("Authorization", "Bearer "+p.key)
		},
		Transport:      p.transport,
		FlushInterval:  -1,
		ModifyResponse: p.recordUsage,
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, err error) {
			// The proxy's own error carries the destination, never the key, the run token or a
			// response header. Log it once where it is handled, then answer the caller.
			slog.Error("openrouter proxy: upstream request failed", "error", err)
			http.Error(w, "openrouter proxy: upstream request failed", http.StatusBadGateway)
		},
	}
}

// URL is the OpenRouter base URL the model should call: this proxy's loopback address.
func (p *Proxy) URL() string {
	return p.url
}

// Usage is what the responses forwarded so far reported.
func (p *Proxy) Usage() Usage {
	p.mu.Lock()
	defer p.mu.Unlock()

	return p.usage
}

// Refusals counts every refused request by outcome, which the alert rules read.
func (p *Proxy) Refusals() map[security.Outcome]int {
	p.mu.Lock()
	defer p.mu.Unlock()

	out := make(map[security.Outcome]int, len(p.refusals))
	maps.Copy(out, p.refusals)

	return out
}

// Close stops the proxy, ending the listener and any request still in flight. It is safe to call
// twice: the listener is already closed the second time.
func (p *Proxy) Close() error {
	err := p.server.Close()
	p.transport.CloseIdleConnections()

	if err != nil {
		return fmt.Errorf("stop openrouter proxy: %w", err)
	}

	return nil
}

// serve forwards one authenticated request, or refuses it when it names anything but OpenRouter's
// API.
func (p *Proxy) serve(w http.ResponseWriter, r *http.Request) {
	if !proxyauth.Authorized(r, p.token) {
		// A call to the key proxy without the run token is a security event: it is what a model that
		// tries to spend the key outside its run looks like. The event carries the destination and
		// outcome, never the token or a header.
		security.Emit(p.log, security.EventKeyProxyRefused, security.RefusedBadToken, p.upstream.Hostname())
		p.mu.Lock()
		p.refusals[security.RefusedBadToken]++
		p.mu.Unlock()
		http.Error(w, "openrouter proxy: run token required", http.StatusUnauthorized)

		return
	}

	if !p.permits(r) {
		http.Error(w, "openrouter proxy: refused destination", http.StatusForbidden)

		return
	}

	p.reverse.ServeHTTP(w, r)
}

// permits reports whether r names OpenRouter's API. The proxy has one fixed upstream, so no request
// can choose where the key goes.
func (p *Proxy) permits(r *http.Request) bool {
	if r.Method == http.MethodConnect || r.URL.IsAbs() {
		return false
	}

	return proxyauth.UnderPrefix(r.URL.Path, APIPrefix)
}

// forwardURL is the OpenRouter URL for r, which permits has already checked.
func (p *Proxy) forwardURL(r *http.Request) *url.URL {
	dest := *p.upstream
	dest.Path = path.Join(p.upstream.Path, path.Clean(r.URL.Path))
	dest.RawQuery = r.URL.RawQuery

	return &dest
}

// recordUsage wraps the response body so the usage it reports is recorded as the body streams, or
// as far as it got when the model's connection ends it.
func (p *Proxy) recordUsage(resp *http.Response) error {
	resp.Body = newUsageBody(resp.Body, p.record)

	return nil
}

// record keeps the usage one response reported.
func (p *Proxy) record(u Usage) {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.usage = p.usage.Add(u)
}
