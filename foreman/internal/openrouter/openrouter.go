// Package openrouter is the loopback forwarding proxy that holds a run's OpenRouter key. It adds
// Authorization to every request, forwards only to OpenRouter's API, and reads the usage every
// response reports. The model calls the proxy with no key of its own, so the key never reaches it.
package openrouter

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"path"
	"strings"
	"sync"
	"time"
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

// Ceilings on the proxy's own connections.
const (
	// dialTimeout bounds reaching OpenRouter.
	dialTimeout = 10 * time.Second
	// tlsTimeout bounds the TLS handshake.
	tlsTimeout = 10 * time.Second
	// headerTimeout bounds waiting for OpenRouter's response headers. The body may then stream for
	// as long as the model run lasts.
	headerTimeout = 60 * time.Second
	// idleTimeout bounds a kept-alive connection between requests.
	idleTimeout = 90 * time.Second
)

// Proxy is a running forwarding proxy.
type Proxy struct {
	key      string
	url      string
	upstream *url.URL
	ctx      context.Context
	cancel   context.CancelFunc
	server   *http.Server
	client   *http.Client
	mu       sync.Mutex
	usage    Usage
}

// Start binds a proxy holding key on addr, forwarding only to OpenRouter's API. addr must be a
// loopback address. A bind or parse failure is an error: the run has no other way to reach a model.
func Start(key, addr string) (*Proxy, error) {
	return start(key, addr, Upstream)
}

// start is Start with the destination named. Only Start's OpenRouter destination is trusted; tests
// name a stand-in so no spec reaches the network.
func start(key, addr, upstream string) (*Proxy, error) {
	if err := loopback(addr); err != nil {
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

	ctx, cancel := context.WithCancel(context.Background())
	p := &Proxy{
		key: key, url: "http://" + ln.Addr().String() + APIPrefix, upstream: dest, ctx: ctx, cancel: cancel,
	}
	p.client = &http.Client{Transport: transport()}
	p.server = &http.Server{Handler: http.HandlerFunc(p.serve), ReadHeaderTimeout: headerTimeout}

	go func() {
		// Serve returns ErrServerClosed when Close stops it, which is the only outcome: there is no
		// error to report anywhere after the listener is closed.
		_ = p.server.Serve(ln)
	}()

	return p, nil
}

// loopback checks that addr names a literal loopback IP, the only host the proxy may listen on.
func loopback(addr string) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("proxy address %q: %w", addr, err)
	}

	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return fmt.Errorf("proxy address %q: listens on loopback only", addr)
	}

	return nil
}

// transport is the proxy's client transport. It ignores the proxy environment variables, so the key
// can never be sent anywhere but OpenRouter.
func transport() *http.Transport {
	return &http.Transport{
		Proxy:                 nil,
		DialContext:           (&net.Dialer{Timeout: dialTimeout}).DialContext,
		TLSHandshakeTimeout:   tlsTimeout,
		ResponseHeaderTimeout: headerTimeout,
		IdleConnTimeout:       idleTimeout,
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
	p.cancel()

	err := p.server.Close()
	p.client.CloseIdleConnections()

	if err != nil {
		return fmt.Errorf("stop openrouter proxy: %w", err)
	}

	return nil
}

// serve forwards one request, or refuses it when it names anything but OpenRouter's API.
func (p *Proxy) serve(w http.ResponseWriter, r *http.Request) {
	dest, ok := p.destination(r)
	if !ok {
		http.Error(w, "openrouter proxy: refused destination", http.StatusForbidden)

		return
	}

	resp, err := p.client.Do(p.forward(r, dest))
	if err != nil {
		http.Error(w, "openrouter proxy: upstream request failed", http.StatusBadGateway)

		return
	}

	defer func() {
		_ = resp.Body.Close()
	}()

	copyHeader(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)

	if err := p.relay(w, resp); err != nil {
		// The headers are already written, so the model's connection ended early and there is
		// nothing left to report to it.
		return
	}
}

// relay streams the response to the model and records the usage it reports.
func (p *Proxy) relay(w http.ResponseWriter, resp *http.Response) error {
	usage := &usageStream{}

	sink := flushWriter{w: w}

	// usage comes first, so the telemetry sees a chunk even when the model's connection fails as it
	// is written.
	_, err := io.Copy(io.MultiWriter(usage, sink), resp.Body)
	// Whatever the response reported before it ended is still worth keeping.
	usage.Finish()
	p.record(usage)

	if err != nil {
		return fmt.Errorf("relay response: %w", err)
	}

	return nil
}

// forward is the upstream request for r: the key added, the destination fixed, nothing that could
// redirect the request or hide its body from the usage reader.
func (p *Proxy) forward(r *http.Request, dest *url.URL) *http.Request {
	req := r.Clone(p.ctx)
	req.URL = dest
	req.RequestURI = ""
	req.Host = dest.Host
	req.Header = forwardHeaders(r)
	req.Header.Set("Authorization", "Bearer "+p.key)

	return req
}

// forwardHeaders is the client's headers without the ones this connection, not the message, owns.
// Accept-Encoding goes too, so the client transport negotiates it and the usage reader sees plain
// text. An Authorization the model invented is replaced by the key.
func forwardHeaders(r *http.Request) http.Header {
	h := r.Header.Clone()
	for _, key := range []string{
		"Accept-Encoding", "Connection", "Keep-Alive", "Proxy-Authenticate", "Proxy-Authorization",
		"Te", "Trailer", "Transfer-Encoding", "Upgrade",
	} {
		h.Del(key)
	}

	return h
}

// record keeps the usage one response reported, if it reported any.
func (p *Proxy) record(usage *usageStream) {
	u, ok := usage.Total()
	if !ok {
		return
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	p.usage = p.usage.Add(u)
}

// destination is the upstream request for r, or false when r names anything but OpenRouter's API.
// The proxy has one fixed upstream, so no request can choose where the key goes.
func (p *Proxy) destination(r *http.Request) (*url.URL, bool) {
	if r.Method == http.MethodConnect || r.URL.IsAbs() {
		return nil, false
	}

	clean := path.Clean(r.URL.Path)
	if clean != APIPrefix && !strings.HasPrefix(clean, APIPrefix+"/") {
		return nil, false
	}

	dest := *p.upstream
	dest.Path = path.Join(p.upstream.Path, clean)
	dest.RawQuery = r.URL.RawQuery

	return &dest, true
}

// flushWriter sends each write on at once, so a streamed answer is not held in the server's buffer
// until the handler returns.
type flushWriter struct {
	w http.ResponseWriter
}

// Write writes p and flushes it.
func (f flushWriter) Write(p []byte) (int, error) {
	n, err := f.w.Write(p)
	if err != nil {
		return n, fmt.Errorf("write response: %w", err)
	}

	if err := http.NewResponseController(f.w).Flush(); err != nil && !errors.Is(err, http.ErrNotSupported) {
		return n, fmt.Errorf("flush response: %w", err)
	}

	return n, nil
}

// copyHeader copies response headers the relay keeps. Length and connection headers are left for
// this server to decide; the body may be decompressed or streamed.
func copyHeader(dst, src http.Header) {
	for key, values := range src {
		if hopByHop(key) || key == "Content-Length" {
			continue
		}

		for _, value := range values {
			dst.Add(key, value)
		}
	}
}

// hopByHop reports a header that describes one connection rather than the message.
func hopByHop(key string) bool {
	switch http.CanonicalHeaderKey(key) {
	case "Connection", "Keep-Alive", "Proxy-Authenticate", "Proxy-Authorization", "Te", "Trailer",
		"Transfer-Encoding", "Upgrade":
		return true
	}

	return false
}
