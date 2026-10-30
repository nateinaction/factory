// Package egress is the loopback proxy every run's traffic crosses. srt blocks direct egress and
// points the run at this proxy; the proxy enforces the agent role's allowlist, refuses every
// destination that is not publicly routable, forwards the traffic, and records the domains it allowed
// and, apart, the ones it refused, for the usage line. Each refusal also emits one security event
// (internal/security) naming its outcome. It also serves the run's OpenRouter API path, so the
// model's base URL is reachable through the one port srt bridges on every OS.
package egress

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"path"
	"sort"
	"strings"
	"sync"

	"github.com/nateinaction/factory/foreman/internal/proxy"
	"github.com/nateinaction/factory/foreman/internal/proxyauth"
	"github.com/nateinaction/factory/foreman/internal/sandbox"
	"github.com/nateinaction/factory/foreman/internal/security"
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

// MaxTunnels bounds how many tunnels one run's proxy carries at once. A run that asks for more is
// refused rather than growing the proxy without limit.
const MaxTunnels = 64

// MaxRefusedHosts bounds how many refused hosts one run's proxy remembers for its usage line. A model
// that asks for endless names is refused without growing the record past this.
const MaxRefusedHosts = 256

// Proxy is a running egress proxy.
type Proxy struct {
	policy       sandbox.Network
	api          *url.URL
	token        string
	server       *http.Server
	listener     net.Listener
	gateway      gateway
	transport    *http.Transport
	apiTransport *http.Transport
	forwarder    *httputil.ReverseProxy
	apiProxy     *httputil.ReverseProxy
	mu           sync.Mutex
	domains      map[string]struct{}
	refused      map[string]struct{}
	refusals     *security.Tally
	tunnels      map[net.Conn]struct{}
	open         int
	log          *slog.Logger
	closeOnce    sync.Once
	closeErr     error
}

// StartWithLogger binds a proxy for policy on addr. apiURL is the OpenRouter key proxy that
// origin-form API calls are forwarded to; empty refuses them. token is the run's random token, and
// an API call without it is refused before anything is forwarded, so no other local caller can spend
// the key. addr must be loopback. Any bind or parse failure is an error: the run has no egress
// without its proxy. logger records traffic the proxy refuses; nil discards those diagnostics.
// refusals is the run's one tally, shared with the key proxy, so both count and measure a refusal the
// same way; nil counts without a metric.
func StartWithLogger(
	ctx context.Context, policy sandbox.Network, apiURL, token, addr string, logger *slog.Logger,
	refusals *security.Tally,
) (*Proxy, error) {
	return startProxy(ctx, policy, apiURL, token, addr, logger, gateway{
		resolve: net.DefaultResolver, dial: (&net.Dialer{Timeout: proxy.DialTimeout}).DialContext,
	}, refusals)
}

// startProxy binds a proxy that reaches destinations through g. The gateway is fixed before the
// listener serves, so every request sees one destination decision. The per-run token is required:
// an API call without it never reaches the key.
func startProxy(
	ctx context.Context, policy sandbox.Network, apiURL, token, addr string,
	logger *slog.Logger, g gateway, refusals *security.Tally,
) (*Proxy, error) {
	if token == "" {
		return nil, errors.New("egress proxy: a per-run token is required")
	}

	if err := proxy.Address("egress", addr); err != nil {
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
		policy: policy, api: api, token: token, listener: ln, log: logger, gateway: g,
		domains: map[string]struct{}{}, refused: map[string]struct{}{},
		refusals: security.TallyOr(refusals), tunnels: map[net.Conn]struct{}{},
	}
	plainDial := (&net.Dialer{Timeout: proxy.DialTimeout}).DialContext
	// Both transports ignore the proxy environment variables, so traffic the run sends through this
	// proxy is never sent through another one.
	p.transport = proxy.Transport(p.dial, proxy.DialTimeout)
	// The key proxy is foreman's own loopback listener rather than a model-chosen destination, so
	// its transport dials without the public-address check.
	p.apiTransport = proxy.Transport(plainDial, proxy.DialTimeout)
	p.forwarder = p.reverseProxy(p.transport, func(*httputil.ProxyRequest) {}, "egress proxy: upstream unavailable")
	p.apiProxy = p.reverseProxy(p.apiTransport, func(pr *httputil.ProxyRequest) {
		pr.Out.URL = p.apiURL(pr.In)
		pr.Out.Host = ""
	}, "egress proxy: model endpoint unavailable")
	// Inbound request headers take the upstream header timeout: one ceiling serves both directions.
	p.server = &http.Server{Handler: http.HandlerFunc(p.serve), ReadHeaderTimeout: proxy.HeaderTimeout}

	proxy.Serve(logger, "egress", p.server, ln)

	return p, nil
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

// reverseProxy is a forwarding handler over transport. rewrite names the outbound destination;
// failure is what an upstream reachable in principle answers. The standard proxy strips every
// hop-by-hop header, including those a Connection header names, and flushInterval -1 sends each
// streamed write on as it arrives.
func (p *Proxy) reverseProxy(
	transport *http.Transport, rewrite func(*httputil.ProxyRequest), failure string,
) *httputil.ReverseProxy {
	return &httputil.ReverseProxy{
		Rewrite:       rewrite,
		Transport:     transport,
		FlushInterval: -1,
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			p.failed(r.Context(), w, r.URL.Hostname(), err, failure)
		},
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

// Domains is every host the proxy allowed, sorted and without repeats. Refused hosts are listed apart,
// by Refused.
func (p *Proxy) Domains() []string {
	p.mu.Lock()
	defer p.mu.Unlock()

	return sortedHosts(p.domains)
}

// Refused is every host the proxy refused, for any reason, sorted and without repeats: the usage line
// says what the run tried to reach and was denied.
func (p *Proxy) Refused() []string {
	p.mu.Lock()
	defer p.mu.Unlock()

	return sortedHosts(p.refused)
}

// Refusals counts every refused request by outcome. It is the run's record of what the proxy turned
// away and why, which the alert rules read; the event's destination stays out of a metric.
func (p *Proxy) Refusals() map[security.Outcome]int {
	return p.refusals.Counts()
}

// sortedHosts is the keys of hosts, sorted. It is never nil.
func sortedHosts(hosts map[string]struct{}) []string {
	out := make([]string, 0, len(hosts))
	for host := range hosts {
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

		p.transport.CloseIdleConnections()
		p.apiTransport.CloseIdleConnections()

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

// record notes one domain the proxy allowed.
func (p *Proxy) record(host string) {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.domains[strings.ToLower(host)] = struct{}{}
}

// dial reaches address through the gateway, recording the host only once a connection to it is made.
// Because nothing is recorded before the decision, a refusal leaves an earlier successful request to
// the same host recorded as allowed, and a destination the run never reached is not recorded at all.
func (p *Proxy) dial(ctx context.Context, network, address string) (net.Conn, error) {
	return p.gateway.dialDecided(ctx, network, address, p.record)
}

// deny notes one refused domain and raises the request's security event through the run's tally,
// which counts it, measures it and logs it within the tally's limit. It never takes the request, so no
// header or token can reach the event.
func (p *Proxy) deny(ctx context.Context, host string, outcome security.Outcome) {
	p.mu.Lock()
	if len(p.refused) < MaxRefusedHosts {
		p.refused[strings.ToLower(host)] = struct{}{}
	}
	p.mu.Unlock()

	p.refusals.Refuse(ctx, p.log, security.EventEgressRefused, outcome, host)
}

// refuse answers a destination the proxy will not reach.
func (p *Proxy) refuse(ctx context.Context, w http.ResponseWriter, host string, outcome security.Outcome) {
	p.deny(ctx, host, outcome)
	http.Error(w, "egress proxy: destination not allowed", http.StatusForbidden)
}

// failed answers a dial that did not reach its destination: forbidden when the gateway refused the
// address, failure for every other cause.
func (p *Proxy) failed(ctx context.Context, w http.ResponseWriter, host string, err error, failure string) {
	if refused, ok := refusal(err); ok {
		// The gateway refuses before the host is recorded, so an earlier allowed record stands.
		p.log.Debug("egress proxy: destination is not publicly routable", "host", host, "address", refused.addr.String())
		p.refuse(ctx, w, host, security.RefusedNotRoutable)

		return
	}

	p.log.Warn("egress proxy: upstream unavailable", "host", host, "error", err)
	http.Error(w, failure, http.StatusBadGateway)
}

// tunnel carries a CONNECT to its destination.
func (p *Proxy) tunnel(w http.ResponseWriter, r *http.Request) {
	host, port, err := net.SplitHostPort(r.Host)
	if err != nil {
		p.deny(r.Context(), r.Host, security.RefusedByPolicy)
		http.Error(w, "egress proxy: malformed CONNECT destination", http.StatusBadRequest)

		return
	}

	if !p.policy.Permits(host) {
		p.refuse(r.Context(), w, host, security.RefusedByPolicy)

		return
	}

	if !p.reserveTunnel() {
		p.deny(r.Context(), host, security.RefusedTunnelLimit)
		http.Error(w, "egress proxy: too many tunnels", http.StatusTooManyRequests)

		return
	}

	defer p.releaseTunnel()

	p.relay(w, r, host, port)
}

// relay reaches the tunnel's destination, hijacks the client connection and pipes both ways.
func (p *Proxy) relay(w http.ResponseWriter, r *http.Request, host, port string) {
	upstream, err := p.dial(r.Context(), "tcp", net.JoinHostPort(host, port))
	if err != nil {
		p.failed(r.Context(), w, host, err, "egress proxy: upstream unavailable")

		return
	}

	hijacker, ok := w.(http.Hijacker)
	if !ok {
		// Nothing was tunneled; the close error has no response left to change.
		_ = upstream.Close()

		http.Error(w, "egress proxy: cannot tunnel", http.StatusInternalServerError)

		return
	}

	client, buf, err := hijacker.Hijack()
	if err != nil {
		// The tunnel never opened; the close error has no response left to change.
		_ = upstream.Close()
		return
	}

	if _, err := buf.WriteString("HTTP/1.1 200 Connection Established\r\n\r\n"); err != nil {
		// The tunnel is abandoned; its close errors have no reader.
		_ = client.Close()
		// The upstream is abandoned with it, so its close error has no reader either.
		_ = upstream.Close()

		return
	}

	if err := buf.Flush(); err != nil {
		// The tunnel is abandoned; its close errors have no reader.
		_ = client.Close()
		// The upstream is abandoned with it, so its close error has no reader either.
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

// copyTunnel pipes both directions until the first side ends, tracking the pair so Close can end it.
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
		_, _ = io.Copy(client, upstream) // the peer's close ends the loop

		done <- struct{}{}
	}()

	<-done

	// The first direction has ended; closing both is what ends the second, so neither close error
	// has a reader left.
	_ = client.Close()
	// The upstream is closed with it, for the same reason.
	_ = upstream.Close()

	<-done

	p.mu.Lock()
	delete(p.tunnels, client)
	delete(p.tunnels, upstream)
	p.mu.Unlock()
}

// forward carries one absolute-form HTTP request to its destination. The egress transport records the
// host once the gateway has decided the destination.
func (p *Proxy) forward(w http.ResponseWriter, r *http.Request) {
	host := r.URL.Hostname()
	if !p.policy.Permits(host) {
		p.refuse(r.Context(), w, host, security.RefusedByPolicy)

		return
	}

	p.forwarder.ServeHTTP(w, r)
}

// apiCall forwards the model's API requests to the run's OpenRouter key proxy. A request that does
// not carry the run's token is refused before it is forwarded, and any other origin-form path is
// refused too: the sandbox gets no general-purpose reverse proxy.
func (p *Proxy) apiCall(w http.ResponseWriter, r *http.Request) {
	if !proxyauth.Authorized(r, p.token) {
		p.deny(r.Context(), APIDomain, security.RefusedBadToken)
		http.Error(w, "egress proxy: run token required", http.StatusUnauthorized)

		return
	}

	if p.api == nil || !proxyauth.UnderPrefix(r.URL.Path, APIPrefix) {
		p.deny(r.Context(), APIDomain, security.RefusedByPolicy)
		http.Error(w, "egress proxy: only the OpenRouter API is served", http.StatusForbidden)

		return
	}

	if !p.policy.Permits(APIDomain) {
		p.refuse(r.Context(), w, APIDomain, security.RefusedByPolicy)

		return
	}

	p.record(APIDomain)
	p.apiProxy.ServeHTTP(w, r)
}

// apiURL is r as a request to the key proxy: the same path beneath /api/v1, nothing that could
// redirect it.
func (p *Proxy) apiURL(r *http.Request) *url.URL {
	clean := path.Clean(r.URL.Path)

	dest := *p.api

	dest.Path = strings.TrimSuffix(p.api.Path, "/") + strings.TrimPrefix(clean, APIPrefix)
	if dest.Path == "" {
		dest.Path = "/"
	}

	dest.RawQuery = r.URL.RawQuery

	return &dest
}
