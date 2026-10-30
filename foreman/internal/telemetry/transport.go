package telemetry

import (
	"net/http"
	"regexp"
	"strings"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// Attribute keys of an outgoing request's span.
const (
	// AttrHTTPMethod is the request's method.
	AttrHTTPMethod = "http.request.method"
	// AttrHTTPStatus is the answer's status code.
	AttrHTTPStatus = "http.response.status_code"
)

// routePattern matches a path segment that names one object: an issue or comment number, or a commit.
var routePattern = regexp.MustCompile(`^(\d+|[0-9a-f]{7,40})$`)

// route is the request path with what varies per object folded away, so a span's name stays one of a
// few dozen and tells a slow call's kind without naming its repository, issue or branch.
func route(path string) string {
	segments := strings.Split(path, "/")

	// /repos/{owner}/{name}/... folds the two segments after repos into one.
	if len(segments) > 3 && segments[1] == "repos" {
		segments = append([]string{"", "repos", "{repo}"}, segments[4:]...)
	}

	for i, segment := range segments {
		if routePattern.MatchString(segment) {
			segments[i] = "{n}"
		}
	}

	return strings.Join(segments, "/")
}

// tracedTransport opens a span around every request it sends.
type tracedTransport struct {
	base   http.RoundTripper
	tracer trace.Tracer
}

// TracedTransport returns a transport that records each request as a span under the request's own
// context, so a trace shows where a command's time went. The span carries the method and the status
// code, never the URL, a header or a body. A 4xx answer leaves the span unmarked, since an absent label
// or comment is an answer the caller handles; a 5xx answer or no answer marks it, with a fixed text.
func TracedTransport(base http.RoundTripper, tracer trace.Tracer) http.RoundTripper {
	return tracedTransport{base: base, tracer: tracer}
}

// RoundTrip sends req and records the call.
func (t tracedTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	ctx, span := t.tracer.Start(req.Context(), req.Method+" "+route(req.URL.Path),
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(attribute.String(AttrHTTPMethod, req.Method)))
	defer span.End()

	resp, err := t.base.RoundTrip(req.WithContext(ctx))
	if err != nil {
		span.SetStatus(codes.Error, "request failed")

		return nil, err //nolint:wrapcheck // a transport passes the answer on exactly as it came
	}

	span.SetAttributes(attribute.Int(AttrHTTPStatus, resp.StatusCode))

	if resp.StatusCode >= http.StatusInternalServerError {
		span.SetStatus(codes.Error, "server error")
	}

	return resp, nil
}
