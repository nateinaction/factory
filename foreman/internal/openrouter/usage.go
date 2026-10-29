package openrouter

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"sync"

	"github.com/nateinaction/factory/foreman/internal/sum"
)

// maxUsageLine bounds one line of a response body the proxy reads for usage.
const maxUsageLine = 1 << 20

// Usage is what OpenRouter reported for the responses a proxy forwarded. A nil field is one no
// response reported; it is never zero.
type Usage struct {
	// Input, Output and Cached are token totals.
	Input, Output, Cached *int
	// Cost is OpenRouter's usage.cost.
	Cost *float64
}

// Add sums two usage reports, keeping a field neither report carried unknown.
func (u Usage) Add(o Usage) Usage {
	u.Input = sum.Add(u.Input, o.Input)
	u.Output = sum.Add(u.Output, o.Output)
	u.Cached = sum.Add(u.Cached, o.Cached)
	u.Cost = sum.Add(u.Cost, o.Cost)

	return u
}

// usageJSON is the subset of a response body the proxy reads: a completion, or one SSE chunk.
type usageJSON struct {
	Usage *usageFields `json:"usage"`
}

// usageFields is OpenRouter's usage object, reduced to the usage log's fields.
type usageFields struct {
	PromptTokens     *int           `json:"prompt_tokens"`
	CompletionTokens *int           `json:"completion_tokens"`
	Cost             *float64       `json:"cost"`
	PromptDetails    *promptDetails `json:"prompt_tokens_details"`
}

// promptDetails is the cached half of OpenRouter's usage object.
type promptDetails struct {
	CachedTokens *int `json:"cached_tokens"`
}

// usage is the fields as the proxy records them.
func (f usageFields) usage() Usage {
	u := Usage{Input: f.PromptTokens, Output: f.CompletionTokens, Cost: f.Cost}
	if f.PromptDetails != nil {
		u.Cached = f.PromptDetails.CachedTokens
	}

	return u
}

// parseUsage reads the usage from a JSON body or one SSE data line. It reports false when the line
// carries no usage, so absence is never read as zero.
func parseUsage(raw []byte) (Usage, bool) {
	var body usageJSON
	if json.Unmarshal(raw, &body) != nil || body.Usage == nil {
		return Usage{}, false
	}

	u := body.Usage.usage()
	if u.Input == nil && u.Output == nil && u.Cached == nil && u.Cost == nil {
		return Usage{}, false
	}

	return u, true
}

// usageStream reads response bytes as they pass through and keeps the last usage the response
// reported. OpenRouter puts it in the last SSE message, and a non-streamed body carries it once.
// Only one line is held at a time, so memory is bounded by maxUsageLine.
type usageStream struct {
	line  []byte
	last  Usage
	found bool
}

// Write consumes p, remembering the last usage it holds. It never fails: the bytes are on their way
// to the model regardless of whether telemetry parses. Each pass consumes at least one byte or
// returns, so the loop is bounded by len(p) passes.
func (s *usageStream) Write(p []byte) (int, error) {
	n := len(p)

	for len(p) > 0 {
		i := bytes.IndexByte(p, '\n')
		if i < 0 {
			s.append(p)

			return n, nil
		}

		s.append(p[:i])
		s.consume()

		p = p[i+1:]
	}

	return n, nil
}

// append adds p to the current line, dropping anything past the line cap. A line that long is not a
// usage message, and the truncated line simply fails to parse.
func (s *usageStream) append(p []byte) {
	room := maxUsageLine - len(s.line)
	if room <= 0 {
		return
	}

	if len(p) > room {
		p = p[:room]
	}

	s.line = append(s.line, p...)
}

// Finish reads a last line that ended without a newline, as a non-streamed body does.
func (s *usageStream) Finish() {
	if len(s.line) > 0 {
		s.consume()
	}
}

// Total is the last usage the response reported and whether it reported any.
func (s *usageStream) Total() (Usage, bool) {
	return s.last, s.found
}

// consume parses the buffered line and clears it.
func (s *usageStream) consume() {
	line := bytes.TrimSpace(s.line)
	s.line = s.line[:0]
	line = bytes.TrimSpace(bytes.TrimPrefix(line, []byte("data:")))

	if u, ok := parseUsage(line); ok {
		s.last, s.found = u, true
	}
}

// newUsageBody wraps body so the usage it reports reaches record once the body ends or is closed,
// whichever comes first.
func newUsageBody(body io.ReadCloser, record func(Usage)) io.ReadCloser {
	return &usageBody{ReadCloser: body, stream: &usageStream{}, record: record}
}

// usageBody reads a response body as it streams to the model and records the last usage it
// reported. Recording once means a body read to EOF and then closed counts once, not twice.
type usageBody struct {
	io.ReadCloser
	stream *usageStream
	record func(Usage)
	once   sync.Once
}

// Read passes p through to the model and feeds it to the usage reader.
func (b *usageBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if n > 0 {
		// usageStream.Write never fails: the bytes are already on their way to the model.
		_, _ = b.stream.Write(p[:n])
	}

	if err != nil {
		b.recordOnce()

		//nolint:wrapcheck // io.EOF must stay comparable so the proxy's copy ends normally
		return n, err
	}

	return n, nil
}

// Close ends the body and records the usage it reported before it ended.
func (b *usageBody) Close() error {
	err := b.ReadCloser.Close()
	b.recordOnce()

	if err != nil {
		return fmt.Errorf("close response body: %w", err)
	}

	return nil
}

// recordOnce finishes the last line and records the usage, once per body.
func (b *usageBody) recordOnce() {
	b.once.Do(func() {
		b.stream.Finish()

		if u, ok := b.stream.Total(); ok {
			b.record(u)
		}
	})
}
