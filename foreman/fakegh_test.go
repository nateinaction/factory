package foreman

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// request is one call the fake GitHub received.
type request struct {
	Method string
	Path   string
	Query  string
	Header http.Header
	Body   string
}

// fakeGH is a recording GitHub stand-in. Routes use ServeMux patterns ("GET /repos/{o}/{r}").
type fakeGH struct {
	t      *testing.T
	srv    *httptest.Server
	mu     sync.Mutex
	reqs   []request
	routes *http.ServeMux
}

// newFakeGH starts a fake whose unhandled calls fail the test.
func newFakeGH(t *testing.T) *fakeGH {
	t.Helper()
	f := &fakeGH{t: t, routes: http.NewServeMux()}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

// serve records the request, then dispatches it.
func (f *fakeGH) serve(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		f.t.Errorf("read body: %v", err)
	}
	f.mu.Lock()
	f.reqs = append(f.reqs, request{r.Method, r.URL.Path, r.URL.RawQuery, r.Header.Clone(), string(body)})
	f.mu.Unlock()
	r.Body = io.NopCloser(strings.NewReader(string(body)))
	if _, pattern := f.routes.Handler(r); pattern == "" {
		f.t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		http.Error(w, "unexpected", http.StatusInternalServerError)
		return
	}
	f.routes.ServeHTTP(w, r)
}

// handle registers a route.
func (f *fakeGH) handle(pattern string, h http.HandlerFunc) {
	f.routes.HandleFunc(pattern, h)
}

// json registers a route that answers with v as JSON.
func (f *fakeGH) json(pattern string, v any) {
	f.handle(pattern, func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, v) })
}

// status registers a route that answers with an empty body and a status code.
func (f *fakeGH) status(pattern string, code int) {
	f.handle(pattern, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(code) })
}

// writeJSON answers with v.
func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

// calls returns a copy of the recorded requests.
func (f *fakeGH) calls() []request {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]request(nil), f.reqs...)
}

// summary lists "METHOD path" per request, in order.
func (f *fakeGH) summary() []string {
	var out []string
	for _, r := range f.calls() {
		out = append(out, r.Method+" "+r.Path)
	}
	return out
}

// bodies returns the request bodies for calls matching "METHOD path".
func (f *fakeGH) bodies(method, path string) []string {
	var out []string
	for _, r := range f.calls() {
		if r.Method == method && r.Path == path {
			out = append(out, r.Body)
		}
	}
	return out
}
