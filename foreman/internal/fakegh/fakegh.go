// Package fakegh is a recording stand-in for GitHub's REST API, for tests. Routes use ServeMux
// patterns ("GET /repos/{o}/{r}"); a request no route matches is answered 500 and listed by
// Unexpected, which a test asserts is empty.
package fakegh

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
)

// Request is one call the fake GitHub received.
type Request struct {
	Method string
	Path   string
	Query  string
	Header http.Header
	Body   string
}

// Server is a recording GitHub stand-in.
type Server struct {
	srv        *httptest.Server
	mu         sync.Mutex
	reqs       []Request
	unexpected []string
	routes     *http.ServeMux
}

// New starts a fake. The caller closes it.
func New() *Server {
	f := &Server{routes: http.NewServeMux()}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	return f
}

// Close shuts the fake down.
func (f *Server) Close() { f.srv.Close() }

// Unexpected lists the requests no route matched, and any body that could not be read.
func (f *Server) Unexpected() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.unexpected...)
}

// fault records a request the test did not expect.
func (f *Server) fault(text string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.unexpected = append(f.unexpected, text)
}

// URL is the fake's API root.
func (f *Server) URL() string { return f.srv.URL }

// Client is an HTTP client that reaches the fake.
func (f *Server) Client() *http.Client { return f.srv.Client() }

// serve records the request, then dispatches it.
func (f *Server) serve(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		f.fault("read body: " + err.Error())
	}
	f.mu.Lock()
	f.reqs = append(f.reqs, Request{r.Method, r.URL.Path, r.URL.RawQuery, r.Header.Clone(), string(body)})
	f.mu.Unlock()
	r.Body = io.NopCloser(strings.NewReader(string(body)))
	if _, pattern := f.routes.Handler(r); pattern == "" {
		f.fault(r.Method + " " + r.URL.Path)
		http.Error(w, "unexpected", http.StatusInternalServerError)
		return
	}
	f.routes.ServeHTTP(w, r)
}

// Handle registers a route.
func (f *Server) Handle(pattern string, h http.HandlerFunc) {
	f.routes.HandleFunc(pattern, h)
}

// JSON registers a route that answers with v as JSON.
func (f *Server) JSON(pattern string, v any) {
	f.Handle(pattern, func(w http.ResponseWriter, _ *http.Request) { WriteJSON(w, v) })
}

// Status registers a route that answers with an empty body and a status code.
func (f *Server) Status(pattern string, code int) {
	f.Handle(pattern, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(code) })
}

// WriteJSON answers with v.
func WriteJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

// Calls returns a copy of the recorded requests.
func (f *Server) Calls() []Request {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Request(nil), f.reqs...)
}

// Summary lists "METHOD path" per request, in order.
func (f *Server) Summary() []string {
	var out []string
	for _, r := range f.Calls() {
		out = append(out, r.Method+" "+r.Path)
	}
	return out
}

// Bodies returns the request bodies for calls matching "METHOD path".
func (f *Server) Bodies(method, path string) []string {
	var out []string
	for _, r := range f.Calls() {
		if r.Method == method && r.Path == path {
			out = append(out, r.Body)
		}
	}
	return out
}
