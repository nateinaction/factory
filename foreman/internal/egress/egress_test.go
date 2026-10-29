package egress

import (
	"bufio"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/nateinaction/factory/foreman/internal/sandbox"
)

// logBuffer is a test log sink safe for the proxy's goroutines.
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

// policy builds an allowlist policy, failing the spec on an invalid entry.
func policy(allowed ...string) sandbox.Network {
	GinkgoHelper()

	n, err := sandbox.ParseNetwork(allowed)
	Expect(err).NotTo(HaveOccurred())

	return n
}

// start binds a proxy on a free loopback port.
func start(n sandbox.Network, apiURL string) *Proxy {
	GinkgoHelper()

	p, err := StartWithLogger(GinkgoT().Context(), n, apiURL, "127.0.0.1:0", nil)
	Expect(err).NotTo(HaveOccurred())

	return p
}

// echoServer is a loopback TCP server that answers every line back.
func echoServer() (string, func()) {
	GinkgoHelper()

	ln, err := (&net.ListenConfig{}).Listen(GinkgoT().Context(), "tcp", "127.0.0.1:0")
	Expect(err).NotTo(HaveOccurred())

	done := make(chan struct{})

	go func() {
		defer close(done)

		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}

			go func() {
				defer func() { _ = conn.Close() }()

				_, _ = io.Copy(conn, conn)
			}()
		}
	}()

	return ln.Addr().String(), func() {
		_ = ln.Close()

		<-done
	}
}

// apiServer is a stand-in OpenRouter API at /api/v1 that reports the path it saw.
func apiServer() *httptest.Server {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprintf(w, "api %s %s", r.Method, r.URL.Path)
	}))

	DeferCleanup(server.Close)

	return server
}

// connect asks proxy for a tunnel to target and returns the response.
func connect(addr, target string) (*http.Response, net.Conn) {
	GinkgoHelper()

	conn, err := (&net.Dialer{}).DialContext(GinkgoT().Context(), "tcp", addr)
	Expect(err).NotTo(HaveOccurred())

	_, err = fmt.Fprintf(conn, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", target, target)
	Expect(err).NotTo(HaveOccurred())

	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	Expect(err).NotTo(HaveOccurred())

	return resp, conn
}

var _ = Describe("Proxy", func() {
	It("tunnels an allowed CONNECT and records the domain", func() {
		echo, stop := echoServer()
		DeferCleanup(stop)

		p := start(policy(), "")
		DeferCleanup(p.Close)

		resp, conn := connect(p.Address(), echo)

		defer func() { _ = conn.Close() }()

		Expect(resp.StatusCode).To(Equal(http.StatusOK))

		_, err := conn.Write([]byte("hello\n"))
		Expect(err).NotTo(HaveOccurred())

		line, err := bufio.NewReader(conn).ReadString('\n')
		Expect(err).NotTo(HaveOccurred())
		Expect(line).To(Equal("hello\n"))
		Expect(p.Domains()).To(ContainElement("127.0.0.1"))
	})

	It("refuses a denied CONNECT and still records the domain", func() {
		p := start(policy("api.example.com"), "")
		DeferCleanup(p.Close)

		resp, conn := connect(p.Address(), "blocked.example:443")

		defer func() { _ = conn.Close() }()

		Expect(resp.StatusCode).To(Equal(http.StatusForbidden))
		Expect(p.Domains()).To(ContainElement("blocked.example"))
	})

	It("forwards an absolute-form request through the same policy", func() {
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.WriteString(w, "upstream "+r.URL.Path)
		}))
		DeferCleanup(upstream.Close)

		p := start(policy("127.0.0.1"), "")
		DeferCleanup(p.Close)

		conn, err := (&net.Dialer{}).DialContext(GinkgoT().Context(), "tcp", p.Address())
		Expect(err).NotTo(HaveOccurred())

		defer func() { _ = conn.Close() }()

		_, err = fmt.Fprintf(conn, "GET %s/thing HTTP/1.1\r\nHost: %s\r\n\r\n", upstream.URL, upstream.URL[7:])
		Expect(err).NotTo(HaveOccurred())

		resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
		Expect(err).NotTo(HaveOccurred())

		defer func() { _ = resp.Body.Close() }()

		body, err := io.ReadAll(resp.Body)
		Expect(err).NotTo(HaveOccurred())
		Expect(string(body)).To(Equal("upstream /thing"))
		Expect(p.Domains()).To(ContainElement("127.0.0.1"))
	})

	It("strips a header the client names in Connection before forwarding", func() {
		headers := make(chan http.Header, 1)

		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			headers <- r.Header.Clone()

			_, _ = io.WriteString(w, "ok")
		}))
		DeferCleanup(upstream.Close)

		p := start(policy("127.0.0.1"), "")
		DeferCleanup(p.Close)

		conn, err := (&net.Dialer{}).DialContext(GinkgoT().Context(), "tcp", p.Address())
		Expect(err).NotTo(HaveOccurred())

		defer func() { _ = conn.Close() }()

		_, err = fmt.Fprintf(conn, "GET %s/thing HTTP/1.1\r\nHost: %s\r\n"+
			"Connection: X-Internal-Token\r\nX-Internal-Token: secret\r\n\r\n",
			upstream.URL, upstream.URL[7:])
		Expect(err).NotTo(HaveOccurred())

		resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
		Expect(err).NotTo(HaveOccurred())

		defer func() { _ = resp.Body.Close() }()

		_, err = io.ReadAll(resp.Body)
		Expect(err).NotTo(HaveOccurred())

		var forwarded http.Header
		Eventually(headers, "5s").Should(Receive(&forwarded))

		Expect(forwarded.Values("X-Internal-Token")).To(BeEmpty())
		Expect(forwarded.Values("Connection")).To(BeEmpty())
	})

	It("passes each streamed chunk on before the upstream sends the next one", func() {
		release := make(chan struct{})

		var once sync.Once

		unblock := func() { once.Do(func() { close(release) }) }

		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(w, "first\n")
			_ = http.NewResponseController(w).Flush()

			<-release

			_, _ = io.WriteString(w, "second\n")
		}))
		DeferCleanup(upstream.Close)
		DeferCleanup(unblock)

		p := start(policy("127.0.0.1"), "")
		DeferCleanup(p.Close)

		client := &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{
			Proxy: http.ProxyURL(&url.URL{Scheme: "http", Host: p.Address()}),
		}}

		req, err := http.NewRequestWithContext(GinkgoT().Context(), http.MethodGet, upstream.URL, nil)
		Expect(err).NotTo(HaveOccurred())

		resp, err := client.Do(req)
		Expect(err).NotTo(HaveOccurred())

		defer func() { _ = resp.Body.Close() }()

		reader := bufio.NewReader(resp.Body)

		type read struct {
			line string
			err  error
		}

		first := make(chan read, 1)

		go func() {
			line, err := reader.ReadString('\n')
			first <- read{line: line, err: err}
		}()

		var got read
		Eventually(first, "5s").Should(Receive(&got))
		Expect(got.err).NotTo(HaveOccurred())
		Expect(got.line).To(Equal("first\n"), "the first chunk reached the client before the upstream sent the rest")

		unblock()

		rest, err := io.ReadAll(reader)
		Expect(err).NotTo(HaveOccurred())
		Expect(string(rest)).To(Equal("second\n"))
	})

	It("serves the OpenRouter API on the proxy's port and records its domain", func() {
		api := apiServer()

		p := start(policy(), api.URL+"/api/v1")
		DeferCleanup(p.Close)

		resp, err := http.Get(p.URL() + "/chat/completions?x=1")
		Expect(err).NotTo(HaveOccurred())

		defer func() { _ = resp.Body.Close() }()

		body, err := io.ReadAll(resp.Body)
		Expect(err).NotTo(HaveOccurred())
		Expect(string(body)).To(Equal("api GET /api/v1/chat/completions"))
		Expect(p.Domains()).To(ContainElement("openrouter.ai"))
	})

	It("refuses an API call the allowlist does not name", func() {
		api := apiServer()

		p := start(policy("api.example.com"), api.URL+"/api/v1")
		DeferCleanup(p.Close)

		resp, err := http.Get(p.URL() + "/chat/completions")
		Expect(err).NotTo(HaveOccurred())

		defer func() { _ = resp.Body.Close() }()

		Expect(resp.StatusCode).To(Equal(http.StatusForbidden))
	})

	It("refuses an origin-form request for anything but the API", func() {
		p := start(policy(), "")
		DeferCleanup(p.Close)

		resp, err := http.Get(p.URL() + "/elsewhere")
		Expect(err).NotTo(HaveOccurred())

		defer func() { _ = resp.Body.Close() }()

		Expect(resp.StatusCode).To(Equal(http.StatusForbidden))
	})

	It("lists each contacted domain once, in order", func() {
		echo, stop := echoServer()
		DeferCleanup(stop)

		p := start(policy(), "")
		DeferCleanup(p.Close)

		for range 2 {
			resp, conn := connect(p.Address(), echo)
			Expect(resp.StatusCode).To(Equal(http.StatusOK))

			_ = conn.Close()
		}

		Expect(p.Domains()).To(Equal([]string{"127.0.0.1"}))
	})

	It("caps open tunnels and logs the refusal", func() {
		var logged logBuffer

		logger := slog.New(slog.NewTextHandler(&logged, nil))

		p, err := StartWithLogger(GinkgoT().Context(), policy(), "", "127.0.0.1:0", logger)
		Expect(err).NotTo(HaveOccurred())

		DeferCleanup(p.Close)

		for range MaxTunnels {
			Expect(p.reserveTunnel()).To(BeTrue())
		}

		echo, stop := echoServer()
		DeferCleanup(stop)

		resp, conn := connect(p.Address(), echo)
		defer func() { _ = conn.Close() }()

		Expect(resp.StatusCode).To(Equal(http.StatusTooManyRequests))
		Expect(logged.String()).To(ContainSubstring("tunnel limit reached"))
	})

	It("refuses to bind anywhere but loopback", func() {
		_, err := StartWithLogger(GinkgoT().Context(), policy(), "", "0.0.0.0:0", nil)
		Expect(err).To(MatchError(ContainSubstring("loopback")))
	})

	It("closes twice without an error", func() {
		p := start(policy(), "")
		Expect(p.Close()).NotTo(HaveOccurred())
		Expect(p.Close()).NotTo(HaveOccurred())
	})

	It("joins the API path without doubling the prefix", func() {
		api := apiServer()

		p := start(policy(), api.URL+"/api/v1")
		DeferCleanup(p.Close)

		resp, err := http.Get(strings.TrimSuffix(p.URL(), "/api/v1") + "/api/v1/models")
		Expect(err).NotTo(HaveOccurred())

		defer func() { _ = resp.Body.Close() }()

		body, err := io.ReadAll(resp.Body)
		Expect(err).NotTo(HaveOccurred())
		Expect(string(body)).To(Equal("api GET /api/v1/models"))
	})
})
