// Package openrouter is the loopback forwarding proxy that holds a run's OpenRouter key. It adds
// Authorization to every request, forwards only to OpenRouter's API, and reads the usage every
// response reports. The model calls the proxy with no key of its own, so the key never reaches it.
package openrouter

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"path"
	"sync"
	"time"

	"github.com/nateinaction/factory/foreman/internal/proxy"
	"github.com/nateinaction/factory/foreman/internal/proxyauth"
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
	mu        sync.Mutex
	usage     Usage
}

// Start binds a proxy holding key on addr, forwarding only to OpenRouter's API. The proxy answers
// only a request carrying token, the random value minted for this run, so no other local caller can
// spend the key. addr must be a loopback address. A bind or parse failure is an error: the run has no
// other way to reach a model.
func Start(key, token, addr string) (*Proxy, error) {
	return start(key, token, addr, Upstream)
}

// start is Start with the destination named. Only Start's OpenRouter destination is trusted; tests
// name a stand-in so no spec reaches the network.
func start(key, token, addr, upstream string) (*Proxy, error) {
	if token == "" {
		return nil, fmt.Errorf("openrouter proxy: a per-run token is required")
	}

	if err := proxy.Address("openrouter", addr); err != nil {
		return nil, err
	}

	dest, err := url.Parse(upstream)
	if err != nil {
		return nil, fmt.Errorf("parse upstream: %w", err)
	}

	ln, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("bind openrouter proxy: %w", err)
	}

	p := &Proxy{key: key, token: token, url: "http://" + ln.Addr().String() + APIPrefix, upstream: dest}
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
