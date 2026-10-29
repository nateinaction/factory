// Package egress is the loopback proxy every run's traffic crosses. srt blocks direct egress and
// points the run at this proxy; the proxy enforces the agent role's allowlist, forwards the traffic,
// and records every domain the run contacted for the usage line. It also serves the run's OpenRouter
// API path, so the model's base URL is reachable through the one port srt bridges on every OS.
package egress

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"path"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/nateinaction/factory/foreman/internal/sandbox"
)

// The proxy's API surface and default address.
const (
	// APIDomain is the domain the model's API calls are reported as. The real upstream is
	// OpenRouter, which the run's key proxy holds.
	APIDomain = "openrouter.ai"
	// APIPrefix is the only origin-form path the proxy answers.
	APIPrefix = "/api/v1"
	// Loopback is the address the proxy binds by default: loopback only, on a free port.
	Loopback = "127.0.0.1:0"
)

// tunnelDirections is how many copies a tunnel runs, one for each direction.
const tunnelDirections = 2

// Ceilings on the proxy's connections.
const (
	// dialTimeout bounds reaching an upstream.
	dialTimeout = 10 * time.Second
	// headerTimeout bounds waiting for an upstream's response headers.
	headerTimeout = 60 * time.Second
	// idleTimeout bounds a kept-alive connection between requests.
	idleTimeout = 90 * time.Second
	// MaxTunnels bounds how many tunnels one run's proxy carries at once. A run that asks for more is
	// refused rather than growing the proxy without limit.
	MaxTunnels = 64
)

// Proxy is a running egress proxy.
type Proxy struct {
	policy    sandbox.Network
	api       *url.URL
	server    *http.Server
	listener  net.Listener
	client    *http.Client
	mu        sync.Mutex
	domains   map[string]struct{}
	tunnels   map[net.Conn]struct{}
	open      int
	log       *slog.Logger
	closeOnce sync.Once
	closeErr  error
}

// StartWithLogger binds a proxy for policy on addr. apiURL is the OpenRouter key proxy that
// origin-form API calls are forwarded to; empty refuses them. addr must be loopback. Any bind or
// parse failure is an error: the run has no egress without its proxy. logger records traffic the
// proxy refuses; nil discards those diagnostics.
func StartWithLogger(
	ctx context.Context, policy sandbox.Network, apiURL, addr string, logger *slog.Logger,
) (*Proxy, error) {
	if err := loopback(addr); err != nil {
		return nil, err
	}

	api, err := parseAPI(apiURL)
	if err != nil {
		return nil, err
	}

	ln, err := (&net.ListenConfig{}).Listen(ctx, "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("bind egress proxy: %w", err)
	}

	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}

	p := &Proxy{
		policy: policy, api: api, listener: ln, log: logger,
		domains: map[string]struct{}{}, tunnels: map[net.Conn]struct{}{},
	}
	p.client = &http.Client{Transport: transport()}
	p.server = &http.Server{Handler: http.HandlerFunc(p.serve), ReadHeaderTimeout: headerTimeout}

	go func() {
		// Serve returns ErrServerClosed when Close stops it, which is the only outcome: there is
		// nothing to report after the listener is closed.
		_ = p.server.Serve(ln)
	}()

	return p, nil
}

// loopback checks that addr is a loopback literal, the only address the proxy may listen on.
func loopback(addr string) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("egress proxy address %q: %w", addr, err)
	}

	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return fmt.Errorf("egress proxy address %q: listens on loopback only", addr)
	}

	return nil
}

// parseAPI validates the key proxy URL. Empty means the run has no model endpoint, which is refused
// rather than guessed.
func parseAPI(apiURL string) (*url.URL, error) {
	if apiURL == "" {
		return nil, nil //nolint:nilnil // an absent API is a valid mode: every API call is refused
	}

	parsed, err := url.Parse(apiURL)
	if err != nil {
		return nil, fmt.Errorf("parse API URL: %w", err)
	}

	if parsed.Scheme == "" || parsed.Host == "" {
		return nil, fmt.Errorf("parse API URL %q: a scheme and host are required", apiURL)
	}

	return parsed, nil
}

// transport ignores the proxy environment variables, so traffic the run sends through this proxy is
// never sent through another one.
func transport() *http.Transport {
	return &http.Transport{
		Proxy:                 nil,
		DialContext:           (&net.Dialer{Timeout: dialTimeout}).DialContext,
		TLSHandshakeTimeout:   dialTimeout,
		ResponseHeaderTimeout: headerTimeout,
		IdleConnTimeout:       idleTimeout,
	}
}

// Address is the loopback host:port srt's settings point at.
func (p *Proxy) Address() string {
	return p.listener.Addr().String()
}

// URL is the base URL the model calls for the OpenRouter API.
func (p *Proxy) URL() string {
	return "http://" + p.Address() + APIPrefix
}

// Domains is every host the run contacted, sorted and without repeats.
func (p *Proxy) Domains() []string {
	p.mu.Lock()
	defer p.mu.Unlock()

	out := make([]string, 0, len(p.domains))
	for host := range p.domains {
		out = append(out, host)
	}

	sort.Strings(out)

	return out
}

// Close stops the proxy, ending the listener and every tunnel still open. It is safe to call twice.
func (p *Proxy) Close() error {
	p.closeOnce.Do(func() {
		p.mu.Lock()
		tunnels := make([]net.Conn, 0, len(p.tunnels))

		for conn := range p.tunnels {
			tunnels = append(tunnels, conn)
		}
		p.mu.Unlock()

		for _, conn := range tunnels {
			_ = conn.Close() // the tunnel's own reader sees the close and ends
		}

		p.client.CloseIdleConnections()

		if err := p.server.Close(); err != nil {
			p.closeErr = fmt.Errorf("stop egress proxy: %w", err)
		}
	})

	return p.closeErr
}

// serve routes one request: a tunnel, a forwarded proxy request, or an API call.
func (p *Proxy) serve(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.Method == http.MethodConnect:
		p.tunnel(w, r)
	case r.URL.IsAbs():
		p.forward(w, r)
	default:
		p.apiCall(w, r)
	}
}

// record notes one contacted domain, whether the policy allowed it or not.
func (p *Proxy) record(host string) {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.domains[strings.ToLower(host)] = struct{}{}
}

// refuse answers a destination the policy does not permit.
func (p *Proxy) refuse(w http.ResponseWriter, host string) {
	p.record(host)
	http.Error(w, "egress proxy: destination not allowed", http.StatusForbidden)
}

// tunnel carries a CONNECT to its destination.
func (p *Proxy) tunnel(w http.ResponseWriter, r *http.Request) {
	host, port, err := net.SplitHostPort(r.Host)
	if err != nil {
		p.record(r.Host)
		http.Error(w, "egress proxy: malformed CONNECT destination", http.StatusBadRequest)

		return
	}

	if !p.policy.Permits(host) {
		p.refuse(w, host)

		return
	}

	p.record(host)

	if !p.reserveTunnel() {
		p.log.Warn("egress proxy: tunnel limit reached; refusing the tunnel", "host", host, "limit", MaxTunnels)
		http.Error(w, "egress proxy: too many tunnels", http.StatusTooManyRequests)

		return
	}

	defer p.releaseTunnel()

	p.relay(w, r, host, port)
}

// relay reaches the tunnel's destination, hijacks the client connection and pipes both ways.
func (p *Proxy) relay(w http.ResponseWriter, r *http.Request, host, port string) {
	upstream, err := (&net.Dialer{Timeout: dialTimeout}).DialContext(r.Context(), "tcp", net.JoinHostPort(host, port))
	if err != nil {
		http.Error(w, "egress proxy: upstream unavailable", http.StatusBadGateway)

		return
	}

	hijacker, ok := w.(http.Hijacker)
	if !ok {
		_ = upstream.Close()

		http.Error(w, "egress proxy: cannot tunnel", http.StatusInternalServerError)

		return
	}

	client, buf, err := hijacker.Hijack()
	if err != nil {
		_ = upstream.Close()
		return
	}

	if _, err := buf.WriteString("HTTP/1.1 200 Connection Established\r\n\r\n"); err != nil {
		_ = client.Close()
		_ = upstream.Close()

		return
	}

	if err := buf.Flush(); err != nil {
		_ = client.Close()
		_ = upstream.Close()

		return
	}

	p.copyTunnel(client, upstream)
}

// reserveTunnel takes one tunnel slot, reporting false when MaxTunnels are already open.
func (p *Proxy) reserveTunnel() bool {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.open >= MaxTunnels {
		return false
	}

	p.open++

	return true
}

// releaseTunnel frees the slot reserveTunnel took.
func (p *Proxy) releaseTunnel() {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.open--
}

// copyTunnel pipes both directions until each side ends, tracking the pair so Close can end it.
func (p *Proxy) copyTunnel(client, upstream net.Conn) {
	p.mu.Lock()
	p.tunnels[client] = struct{}{}
	p.tunnels[upstream] = struct{}{}
	p.mu.Unlock()

	done := make(chan struct{}, tunnelDirections)

	go func() {
		_, _ = io.Copy(upstream, client) // the peer's close ends the loop

		done <- struct{}{}
	}()
	go func() {
		_, _ = io.Copy(client, upstream)

		done <- struct{}{}
	}()

	<-done

	_ = client.Close()
	_ = upstream.Close()

	<-done

	p.mu.Lock()
	delete(p.tunnels, client)
	delete(p.tunnels, upstream)
	p.mu.Unlock()
}

// forward carries one absolute-form HTTP request to its destination.
func (p *Proxy) forward(w http.ResponseWriter, r *http.Request) {
	host := r.URL.Hostname()
	if !p.policy.Permits(host) {
		p.refuse(w, host)

		return
	}

	p.record(host)

	req := r.Clone(r.Context())
	req.RequestURI = ""
	req.Header.Del("Proxy-Authorization")
	req.Header.Del("Proxy-Connection")

	resp, err := p.client.Do(req) //nolint:gosec // the proxy's job is to forward an allowed destination
	if err != nil {
		http.Error(w, "egress proxy: upstream unavailable", http.StatusBadGateway)

		return
	}

	defer func() {
		_ = resp.Body.Close()
	}()

	copyHeader(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body) // the client's close ends the copy
}

// apiCall forwards the model's API requests to the run's OpenRouter key proxy. Any other
// origin-form path is refused: the sandbox gets no general-purpose reverse proxy.
func (p *Proxy) apiCall(w http.ResponseWriter, r *http.Request) {
	if p.api == nil || !underAPIPrefix(r.URL.Path) {
		http.Error(w, "egress proxy: only the OpenRouter API is served", http.StatusForbidden)

		return
	}

	if !p.policy.Permits(APIDomain) {
		p.refuse(w, APIDomain)

		return
	}

	p.record(APIDomain)

	resp, err := p.client.Do(p.apiRequest(r)) //nolint:gosec // the proxy's job is to forward an allowed destination
	if err != nil {
		http.Error(w, "egress proxy: model endpoint unavailable", http.StatusBadGateway)

		return
	}

	defer func() {
		_ = resp.Body.Close()
	}()

	copyHeader(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body) // the client's close ends the copy
}

// apiRequest is r as a request to the key proxy: the same path beneath /api/v1, nothing that could
// redirect it.
func (p *Proxy) apiRequest(r *http.Request) *http.Request {
	clean := path.Clean(r.URL.Path)

	dest := *p.api

	dest.Path = strings.TrimSuffix(p.api.Path, "/") + strings.TrimPrefix(clean, APIPrefix)
	if dest.Path == "" {
		dest.Path = "/"
	}

	dest.RawQuery = r.URL.RawQuery

	req := r.Clone(r.Context())
	req.URL = &dest
	req.RequestURI = ""
	req.Host = dest.Host
	req.Header.Del("Proxy-Authorization")
	req.Header.Del("Proxy-Connection")

	return req
}

// underAPIPrefix reports whether p is the API or a path beneath it.
func underAPIPrefix(p string) bool {
	clean := path.Clean(p)

	return clean == APIPrefix || strings.HasPrefix(clean, APIPrefix+"/")
}

// copyHeader copies response headers, leaving connection headers and length to this server.
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
