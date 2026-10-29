// Package ghtest is test support for GitHub's REST API: a recording proxy that can force failures,
// and the vercel-labs/emulate GitHub emulator the proxy forwards to.
package ghtest

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"time"
)

// proxyTimeout bounds one request the proxy forwards.
const proxyTimeout = 20 * time.Second

// Request is one call the proxy saw.
type Request struct {
	Method string
	Path   string
	Query  string
	Header http.Header
	Body   string
}

// Proxy records GitHub requests. A request matching a registered route is answered by that route;
// everything else is forwarded to upstream. Without an upstream, an unmatched request is answered
// 500 and listed by Unexpected, so a spec fails on a call it did not expect.
type Proxy struct {
	upstream   string
	hc         *http.Client
	srv        *httptest.Server
	mu         sync.Mutex
	reqs       []Request
	unexpected []string
	routes     *http.ServeMux
}

// NewProxy starts a proxy in front of upstream, which may be empty for a proxy that only serves routes.
func NewProxy(upstream string) *Proxy {
	p := &Proxy{
		upstream: strings.TrimRight(upstream, "/"),
		hc:       &http.Client{Timeout: proxyTimeout},
		routes:   http.NewServeMux(),
	}
	p.srv = httptest.NewServer(http.HandlerFunc(p.serve))

	return p
}

// Close shuts the proxy down.
func (p *Proxy) Close() { p.srv.Close() }

// URL is the proxy's base URL.
func (p *Proxy) URL() string { return p.srv.URL }

// Client is an HTTP client that reaches the proxy.
func (p *Proxy) Client() *http.Client { return p.srv.Client() }

// Handle registers a route, which takes precedence over the upstream.
func (p *Proxy) Handle(pattern string, h http.HandlerFunc) {
	p.routes.HandleFunc(pattern, h)
}

// JSON registers a route that answers with v as JSON.
func (p *Proxy) JSON(pattern string, v any) {
	p.Handle(pattern, func(w http.ResponseWriter, _ *http.Request) { WriteJSON(w, v) })
}

// Status registers a route that answers with an empty body and a status code.
func (p *Proxy) Status(pattern string, code int) {
	p.Handle(pattern, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(code) })
}

// Forward passes a request in a route's handler on to the upstream.
func (p *Proxy) Forward(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		p.fault("read body: " + err.Error())
		http.Error(w, "unreadable body", http.StatusBadRequest)

		return
	}

	p.forward(w, r, body)
}

// serve records the request and dispatches it to a route or the upstream.
func (p *Proxy) serve(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		p.fault("read body: " + err.Error())
	}

	p.mu.Lock()
	p.reqs = append(p.reqs, Request{r.Method, r.URL.Path, r.URL.RawQuery, r.Header.Clone(), string(body)})
	p.mu.Unlock()

	r.Body = io.NopCloser(bytes.NewReader(body))

	if h, pattern := p.routes.Handler(r); pattern != "" {
		h.ServeHTTP(w, r)

		return
	}

	if p.upstream == "" {
		p.fault(r.Method + " " + r.URL.Path)
		http.Error(w, "unexpected", http.StatusInternalServerError)

		return
	}

	p.forward(w, r, body)
}

// forward sends the recorded request to the upstream and copies the answer back.
func (p *Proxy) forward(w http.ResponseWriter, r *http.Request, body []byte) {
	//nolint:gosec // the proxy forwards only to the test's emulator
	req, err := http.NewRequestWithContext(r.Context(), r.Method, p.upstream+r.URL.RequestURI(), bytes.NewReader(body))
	if err != nil {
		p.fault("build forward: " + err.Error())
		http.Error(w, "cannot forward", http.StatusInternalServerError)

		return
	}

	req.Header = r.Header.Clone()

	resp, err := p.hc.Do(req) //nolint:gosec // the proxy forwards only to the test's emulator
	if err != nil {
		p.fault("forward: " + err.Error())
		http.Error(w, "cannot forward", http.StatusInternalServerError)

		return
	}

	defer func() {
		// The answer is fully copied or abandoned; a close failure changes nothing for the client.
		_ = resp.Body.Close()
	}()

	for key, values := range resp.Header {
		for _, value := range values {
			w.Header().Add(key, value)
		}
	}

	w.WriteHeader(resp.StatusCode)

	if _, err := io.Copy(w, resp.Body); err != nil {
		p.fault("forward body: " + err.Error())
	}
}

// fault records a request the test did not expect.
func (p *Proxy) fault(text string) {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.unexpected = append(p.unexpected, text)
}

// Unexpected lists the requests nothing answered, and any body that could not be read.
func (p *Proxy) Unexpected() []string {
	p.mu.Lock()
	defer p.mu.Unlock()

	return append([]string(nil), p.unexpected...)
}

// Calls returns a copy of the recorded requests.
func (p *Proxy) Calls() []Request {
	p.mu.Lock()
	defer p.mu.Unlock()

	return append([]Request(nil), p.reqs...)
}

// Summary lists "METHOD path" per request, in order.
func (p *Proxy) Summary() []string {
	calls := p.Calls()

	out := make([]string, 0, len(calls))
	for _, r := range calls {
		out = append(out, r.Method+" "+r.Path)
	}

	return out
}

// Bodies returns the request bodies for calls matching "METHOD path".
func (p *Proxy) Bodies(method, path string) []string {
	var out []string

	for _, r := range p.Calls() {
		if r.Method == method && r.Path == path {
			out = append(out, r.Body)
		}
	}

	return out
}

// WriteJSON answers with v.
func WriteJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")

	if err := json.NewEncoder(w).Encode(v); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}
