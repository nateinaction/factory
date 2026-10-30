package openrouter

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/nateinaction/factory/foreman/internal/security"
)

// logBuffer collects the proxy's diagnostics as JSON lines.
type logBuffer struct {
	mu   sync.Mutex
	text strings.Builder
}

// Write appends p to the buffer.
func (b *logBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	n, err := b.text.Write(p)
	if err != nil {
		return n, fmt.Errorf("log buffer: %w", err)
	}

	return n, nil
}

// String returns what was written.
func (b *logBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.text.String()
}

// securityEvents is every security event in the buffer, decoded.
func securityEvents(logged *logBuffer) []map[string]any {
	GinkgoHelper()

	var events []map[string]any

	for line := range strings.SplitSeq(strings.TrimSpace(logged.String()), "\n") {
		if line == "" {
			continue
		}

		var record map[string]any
		Expect(json.Unmarshal([]byte(line), &record)).To(Succeed())

		if record["msg"] == security.Message {
			events = append(events, record)
		}
	}

	return events
}

var _ = Describe("the OpenRouter proxy's security events", func() {
	It("emits one event for a call without the run token, and none for an authenticated one", func() {
		up := newUpstream(nil)
		DeferCleanup(up.Close)

		var logged logBuffer

		log := security.Scope(slog.New(slog.NewJSONHandler(&logged, nil)), "Implementer", "o/r", 7)

		p, err := startAt(GinkgoT().Context(), testKey, testToken, Loopback, up.URL, log)
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(p.Close)

		auth := func(token string) int {
			GinkgoHelper()

			req, err := http.NewRequestWithContext(GinkgoT().Context(), http.MethodGet, p.URL()+"/models", nil)
			Expect(err).NotTo(HaveOccurred())
			req.Header.Set("Authorization", "Bearer "+token)

			resp, err := proxyClient().Do(req)
			Expect(err).NotTo(HaveOccurred())
			Expect(resp.Body.Close()).To(Succeed())

			return resp.StatusCode
		}

		Expect(auth(testToken)).To(Equal(http.StatusOK), "an allowed call emitted an event")
		Expect(auth("another-run-token")).To(Equal(http.StatusUnauthorized))

		dest, err := url.Parse(up.URL)
		Expect(err).NotTo(HaveOccurred())

		events := securityEvents(&logged)
		Expect(events).To(HaveLen(1))
		Expect(events[0]).To(And(
			HaveKeyWithValue("event", "key_proxy_refused"),
			HaveKeyWithValue("outcome", "refused_bad_token"),
			HaveKeyWithValue("host", dest.Hostname()),
			HaveKeyWithValue("agent_role", "Implementer"),
			HaveKeyWithValue("repository", "o/r"),
			HaveKeyWithValue("issue", float64(7)),
		))
		Expect(logged.String()).NotTo(ContainSubstring(testToken))
		Expect(logged.String()).NotTo(ContainSubstring("another-run-token"))
		Expect(p.Refusals()).To(Equal(map[security.Outcome]int{security.RefusedBadToken: 1}))
	})

	It("rate limits a flood of refusals while counting every one", func() {
		up := newUpstream(nil)
		DeferCleanup(up.Close)

		var logged logBuffer

		log := security.Scope(slog.New(slog.NewJSONHandler(&logged, nil)), "Implementer", "o/r", 7)

		p, err := startAt(GinkgoT().Context(), testKey, testToken, Loopback, up.URL, log)
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(p.Close)

		const refusals = 50

		for range refusals {
			req, err := http.NewRequestWithContext(GinkgoT().Context(), http.MethodGet, p.URL()+"/models", nil)
			Expect(err).NotTo(HaveOccurred())
			req.Header.Set("Authorization", "Bearer another-run-token")

			resp, err := proxyClient().Do(req)
			Expect(err).NotTo(HaveOccurred())
			Expect(resp.Body.Close()).To(Succeed())
			Expect(resp.StatusCode).To(Equal(http.StatusUnauthorized))
		}

		Expect(securityEvents(&logged)).To(HaveLen(security.RefusalLogBurst),
			"a refusal flood reached the log without a limit")
		Expect(p.Refusals()).To(Equal(map[security.Outcome]int{security.RefusedBadToken: refusals}))
		Expect(up.calls).To(BeEmpty())
	})
})
