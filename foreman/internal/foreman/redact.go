package foreman

import (
	"fmt"
	"io"
	"strings"
)

// redacted replaces every secret in text bound for a log.
const redacted = "[redacted]"

// maxPending bounds what redactWriter holds back while it waits for a newline.
const maxPending = 64 << 10

// redact removes every secret from text. An empty secret would match everywhere, so it is skipped.
func redact(text string, secrets []string) string {
	for _, secret := range secrets {
		if secret == "" {
			continue
		}

		text = strings.ReplaceAll(text, secret, redacted)
	}

	return text
}

// redactWriter passes what is written to dst with every secret removed. It holds back an
// unfinished line, up to maxPending bytes, so a secret split across two writes is still found. A
// full buffer is written out except for its last len(longest secret)-1 bytes: a secret cut by the
// buffer's end is shorter than that. The writer is one use: call flush when the stream ends.
type redactWriter struct {
	dst     io.Writer
	secrets []string
	pending string
	tail    int
}

// newRedactWriter returns a writer to dst that removes secrets.
func newRedactWriter(dst io.Writer, secrets []string) *redactWriter {
	tail := 0

	for _, s := range secrets {
		tail = max(tail, len(s)-1)
	}

	return &redactWriter{dst: dst, secrets: secrets, tail: tail}
}

// Write buffers p and writes out its complete lines.
func (w *redactWriter) Write(p []byte) (int, error) {
	w.pending += string(p)

	if i := strings.LastIndexByte(w.pending, '\n'); i >= 0 {
		line := w.pending[:i+1]
		w.pending = w.pending[i+1:]

		if err := w.emit(redact(line, w.secrets)); err != nil {
			return 0, err
		}
	}

	if len(w.pending) > maxPending {
		clean := redact(w.pending, w.secrets)
		keep := min(w.tail, len(clean))
		w.pending = clean[len(clean)-keep:]

		if err := w.emit(clean[:len(clean)-keep]); err != nil {
			return 0, err
		}
	}

	return len(p), nil
}

// flush writes out the unfinished last line.
func (w *redactWriter) flush() error {
	rest := redact(w.pending, w.secrets)
	w.pending = ""

	return w.emit(rest)
}

// emit writes clean text to dst.
func (w *redactWriter) emit(clean string) error {
	if clean == "" {
		return nil
	}

	_, err := io.WriteString(w.dst, clean)
	if err != nil {
		return fmt.Errorf("write redacted output: %w", err)
	}

	return nil
}
