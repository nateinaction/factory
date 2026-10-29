package proxy

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// logBuffer is a test log sink safe for the serve goroutine.
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

// newLogger returns a logger writing to b.
func newLogger(b *logBuffer) *slog.Logger {
	return slog.New(slog.NewTextHandler(b, nil))
}

var _ = Describe("Address", func() {
	DescribeTable("accepts loopback only",
		func(addr, refused string) {
			err := Address("test", addr)

			if refused == "" {
				Expect(err).NotTo(HaveOccurred())

				return
			}

			Expect(err).To(MatchError(ContainSubstring(refused)))
			Expect(err.Error()).To(ContainSubstring(addr))
			Expect(err.Error()).To(ContainSubstring("test proxy address"))
		},
		Entry("a loopback IPv4 address", "127.0.0.1:0", ""),
		Entry("a loopback IPv6 address", "[::1]:8080", ""),
		Entry("every IPv4 interface", "0.0.0.0:0", "loopback only"),
		Entry("a private IPv4 address", "192.168.1.5:8080", "loopback only"),
		Entry("a routable IPv4 address", "93.184.216.34:8080", "loopback only"),
		Entry("a hostname, which names no literal address", "localhost:0", "address"),
		Entry("an address with no port", "127.0.0.1", "address"),
	)
})

var _ = Describe("Transport", func() {
	It("ignores the proxy environment and dials through the dial it was given", func() {
		GinkgoT().Setenv("HTTP_PROXY", "http://host-proxy.example:3128")
		GinkgoT().Setenv("HTTPS_PROXY", "http://host-proxy.example:3128")

		dialed := false

		tr := Transport(func(_ context.Context, _, _ string) (net.Conn, error) {
			dialed = true

			return nil, errors.New("test dial: no upstream")
		}, 5*time.Second)

		Expect(tr.Proxy).To(BeNil(), "the host's proxy settings must not carry a run's traffic")
		Expect(tr.TLSHandshakeTimeout).To(Equal(5 * time.Second))
		Expect(tr.ResponseHeaderTimeout).To(Equal(HeaderTimeout))
		Expect(tr.IdleConnTimeout).To(Equal(IdleTimeout))

		_, err := tr.DialContext(context.Background(), "tcp", "127.0.0.1:1")
		Expect(err).To(MatchError(ContainSubstring("test dial")))
		Expect(dialed).To(BeTrue(), "the transport dialed with its own dialer")
	})
})

var _ = Describe("Serve", func() {
	It("logs a stop the server did not cause", func() {
		var logged logBuffer

		ln, err := (&net.ListenConfig{}).Listen(GinkgoT().Context(), "tcp", "127.0.0.1:0")
		Expect(err).NotTo(HaveOccurred())

		server := &http.Server{Handler: http.NotFoundHandler(), ReadHeaderTimeout: time.Second}

		// A listener that ends under the server is a fatal accept error: nothing answers the run's
		// requests afterwards, so it must not be silent.
		Expect(ln.Close()).To(Succeed())

		Serve(newLogger(&logged), "test", server, ln)

		Eventually(logged.String).Should(ContainSubstring("test proxy: serve stopped"))
	})

	It("says nothing when the server is closed", func() {
		var logged logBuffer

		ln, err := (&net.ListenConfig{}).Listen(GinkgoT().Context(), "tcp", "127.0.0.1:0")
		Expect(err).NotTo(HaveOccurred())

		server := &http.Server{Handler: http.NotFoundHandler(), ReadHeaderTimeout: time.Second}

		Serve(newLogger(&logged), "test", server, ln)

		// A request the server answers proves the serve loop is running before it is closed.
		resp, err := http.Get("http://" + ln.Addr().String()) //nolint:noctx // a test's own loopback request
		Expect(err).NotTo(HaveOccurred())
		Expect(resp.Body.Close()).To(Succeed())

		Expect(server.Close()).To(Succeed())

		Consistently(logged.String, "200ms").ShouldNot(ContainSubstring("serve stopped"))
	})
})
