package egress

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"sync"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/nateinaction/factory/foreman/internal/sandbox"
)

// publicAddress is a name-independent address no refusal rule covers: everything a spec stands up
// itself listens on loopback, so a spec that wants a successful dial fakes the resolution.
const publicAddress = "93.184.216.34"

// testToken is the per-run token every spec sends the proxy.
const testToken = "run-token"

// failedHijacker is a ResponseWriter whose connection cannot be hijacked.
type failedHijacker struct {
	*httptest.ResponseRecorder
}

// Hijack fails the way a connection already gone would.
func (failedHijacker) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	return nil, nil, errors.New("test hijack: connection gone")
}

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

	p, err := StartWithLogger(GinkgoT().Context(), n, apiURL, testToken, "127.0.0.1:0", nil)
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
				// test cleanup: a close error changes no assertion
				defer func() { _ = conn.Close() }()

				// the loop ends when the connection does
				_, _ = io.Copy(conn, conn)
			}()
		}
	}()

	return ln.Addr().String(), func() {
		// test cleanup: a close error changes no assertion
		_ = ln.Close()

		<-done
	}
}

// apiServer is a stand-in OpenRouter API at /api/v1 that reports the path it saw.
func apiServer() *httptest.Server {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// writing to a test ResponseWriter cannot change the assertion
		_, _ = fmt.Fprintf(w, "api %s %s", r.Method, r.URL.Path)
	}))

	DeferCleanup(server.Close)

	return server
}

// staticResolver answers every lookup with its addresses.
type staticResolver []netip.Addr

// LookupNetIP returns the resolver's addresses whatever it was asked for.
func (r staticResolver) LookupNetIP(context.Context, string, string) ([]netip.Addr, error) {
	return r, nil
}

// startWith binds a proxy on a free loopback port that reaches destinations through g.
func startWith(n sandbox.Network, g gateway) *Proxy {
	GinkgoHelper()

	p, err := startProxy(GinkgoT().Context(), n, "", testToken, "127.0.0.1:0", nil, g)
	Expect(err).NotTo(HaveOccurred())
	DeferCleanup(p.Close)

	return p
}

// loopbackGateway treats every destination as public and dials the loopback server at the port the
// spec asked for, so a spec exercises the proxy's success paths without outside network access.
func loopbackGateway() gateway {
	return gateway{
		resolve: staticResolver{netip.MustParseAddr(publicAddress)},
		dial: func(ctx context.Context, network, address string) (net.Conn, error) {
			_, port, err := net.SplitHostPort(address)
			if err != nil {
				return nil, fmt.Errorf("test dial %q: %w", address, err)
			}

			return (&net.Dialer{}).DialContext(ctx, network, net.JoinHostPort("127.0.0.1", port))
		},
	}
}

// refusedHosts is one address from every range the proxy must not reach.
func refusedHosts() []string {
	return []string{
		"0.0.0.0", "127.0.0.1", "127.9.9.9", "10.1.2.3", "172.16.0.1", "192.168.1.1",
		"169.254.169.254", "100.64.0.1", "224.0.0.1", "255.255.255.255",
		"::1", "fd00::1", "fe80::1", "ff02::1", "::ffff:127.0.0.1", "::ffff:192.168.1.1",
	}
}

// forward sends one absolute-form GET for target through proxy and returns the response.
func forward(proxyAddr, target string) (*http.Response, net.Conn) {
	GinkgoHelper()

	parsed, err := url.Parse(target)
	Expect(err).NotTo(HaveOccurred())

	conn, err := (&net.Dialer{}).DialContext(GinkgoT().Context(), "tcp", proxyAddr)
	Expect(err).NotTo(HaveOccurred())

	_, err = fmt.Fprintf(conn, "GET %s HTTP/1.1\r\nHost: %s\r\nConnection: close\r\n\r\n", target, parsed.Host)
	Expect(err).NotTo(HaveOccurred())

	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	Expect(err).NotTo(HaveOccurred())

	return resp, conn
}

// apiGet asks the proxy's API for rawURL with the run's token.
func apiGet(rawURL string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(GinkgoT().Context(), http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, fmt.Errorf("build API request: %w", err)
	}

	req.Header.Set("Authorization", "Bearer "+testToken)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("call API: %w", err)
	}

	return resp, nil
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

		p := startWith(policy(), loopbackGateway())

		resp, conn := connect(p.Address(), echo)

		// test cleanup: a close error changes no assertion
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

		// test cleanup: a close error changes no assertion
		defer func() { _ = conn.Close() }()

		Expect(resp.StatusCode).To(Equal(http.StatusForbidden))
		Expect(p.Domains()).To(ContainElement("blocked.example"))
	})

	It("forwards an absolute-form request through the same policy", func() {
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// writing to a test ResponseWriter cannot change the assertion
			_, _ = io.WriteString(w, "upstream "+r.URL.Path)
		}))
		DeferCleanup(upstream.Close)

		p := startWith(policy("127.0.0.1"), loopbackGateway())

		conn, err := (&net.Dialer{}).DialContext(GinkgoT().Context(), "tcp", p.Address())
		Expect(err).NotTo(HaveOccurred())

		// test cleanup: a close error changes no assertion
		defer func() { _ = conn.Close() }()

		_, err = fmt.Fprintf(conn, "GET %s/thing HTTP/1.1\r\nHost: %s\r\n\r\n", upstream.URL, upstream.URL[7:])
		Expect(err).NotTo(HaveOccurred())

		resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
		Expect(err).NotTo(HaveOccurred())

		// test cleanup: a close error changes no assertion
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

			// writing to a test ResponseWriter cannot change the assertion
			_, _ = io.WriteString(w, "ok")
		}))
		DeferCleanup(upstream.Close)

		p := startWith(policy("127.0.0.1"), loopbackGateway())

		conn, err := (&net.Dialer{}).DialContext(GinkgoT().Context(), "tcp", p.Address())
		Expect(err).NotTo(HaveOccurred())

		// test cleanup: a close error changes no assertion
		defer func() { _ = conn.Close() }()

		_, err = fmt.Fprintf(conn, "GET %s/thing HTTP/1.1\r\nHost: %s\r\n"+
			"Connection: X-Internal-Token\r\nX-Internal-Token: secret\r\n\r\n",
			upstream.URL, upstream.URL[7:])
		Expect(err).NotTo(HaveOccurred())

		resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
		Expect(err).NotTo(HaveOccurred())

		// test cleanup: a close error changes no assertion
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
			// writing to a test ResponseWriter cannot change the assertion
			_, _ = io.WriteString(w, "first\n")
			// the test reads the stream regardless of a flush error
			_ = http.NewResponseController(w).Flush()

			<-release

			// writing to a test ResponseWriter cannot change the assertion
			_, _ = io.WriteString(w, "second\n")
		}))
		DeferCleanup(upstream.Close)
		DeferCleanup(unblock)

		p := startWith(policy("127.0.0.1"), loopbackGateway())

		client := &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{
			Proxy: http.ProxyURL(&url.URL{Scheme: "http", Host: p.Address()}),
		}}

		req, err := http.NewRequestWithContext(GinkgoT().Context(), http.MethodGet, upstream.URL, nil)
		Expect(err).NotTo(HaveOccurred())

		resp, err := client.Do(req)
		Expect(err).NotTo(HaveOccurred())

		// test cleanup: a close error changes no assertion
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

		resp, err := apiGet(p.URL() + "/chat/completions?x=1")
		Expect(err).NotTo(HaveOccurred())

		// test cleanup: a close error changes no assertion
		defer func() { _ = resp.Body.Close() }()

		body, err := io.ReadAll(resp.Body)
		Expect(err).NotTo(HaveOccurred())
		Expect(string(body)).To(Equal("api GET /api/v1/chat/completions"))
		Expect(p.Domains()).To(ContainElement("openrouter.ai"))
	})

	It("refuses an API call without the run's token before forwarding it", func() {
		calls := make(chan struct{}, 1)

		api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			calls <- struct{}{}

			// writing to a test ResponseWriter cannot change the assertion
			_, _ = io.WriteString(w, "api")
		}))
		DeferCleanup(api.Close)

		p := start(policy(), api.URL+"/api/v1")
		DeferCleanup(p.Close)

		for _, authorization := range []string{"", "Bearer another-run-token"} {
			req, err := http.NewRequestWithContext(
				GinkgoT().Context(), http.MethodGet, p.URL()+"/chat/completions", nil,
			)
			Expect(err).NotTo(HaveOccurred())

			if authorization != "" {
				req.Header.Set("Authorization", authorization)
			}

			resp, err := http.DefaultClient.Do(req)
			Expect(err).NotTo(HaveOccurred())
			Expect(resp.Body.Close()).To(Succeed())

			Expect(resp.StatusCode).To(Equal(http.StatusUnauthorized), authorization)
		}

		Consistently(calls, "100ms").ShouldNot(Receive(), "an unauthenticated call reached the key proxy")
	})

	It("refuses to start without a run token", func() {
		_, err := StartWithLogger(GinkgoT().Context(), policy(), "", "", "127.0.0.1:0", nil)
		Expect(err).To(MatchError(ContainSubstring("token")))
	})

	It("refuses an API call the allowlist does not name", func() {
		api := apiServer()

		p := start(policy("api.example.com"), api.URL+"/api/v1")
		DeferCleanup(p.Close)

		resp, err := apiGet(p.URL() + "/chat/completions")
		Expect(err).NotTo(HaveOccurred())

		// test cleanup: a close error changes no assertion
		defer func() { _ = resp.Body.Close() }()

		Expect(resp.StatusCode).To(Equal(http.StatusForbidden))
	})

	It("refuses an origin-form request for anything but the API", func() {
		p := start(policy(), "")
		DeferCleanup(p.Close)

		resp, err := apiGet(p.URL() + "/elsewhere")
		Expect(err).NotTo(HaveOccurred())

		// test cleanup: a close error changes no assertion
		defer func() { _ = resp.Body.Close() }()

		Expect(resp.StatusCode).To(Equal(http.StatusForbidden))
	})

	It("lists each contacted domain once, in order", func() {
		echo, stop := echoServer()
		DeferCleanup(stop)

		p := startWith(policy(), loopbackGateway())

		for range 2 {
			resp, conn := connect(p.Address(), echo)
			Expect(resp.StatusCode).To(Equal(http.StatusOK))

			// test cleanup: a close error changes no assertion
			_ = conn.Close()
		}

		Expect(p.Domains()).To(Equal([]string{"127.0.0.1"}))
	})

	It("caps open tunnels and logs the refusal", func() {
		var logged logBuffer

		logger := slog.New(slog.NewTextHandler(&logged, nil))

		p, err := StartWithLogger(GinkgoT().Context(), policy(), "", testToken, "127.0.0.1:0", logger)
		Expect(err).NotTo(HaveOccurred())

		DeferCleanup(p.Close)

		for range MaxTunnels {
			Expect(p.reserveTunnel()).To(BeTrue())
		}

		echo, stop := echoServer()
		DeferCleanup(stop)

		resp, conn := connect(p.Address(), echo)
		// test cleanup: a close error changes no assertion
		defer func() { _ = conn.Close() }()

		Expect(resp.StatusCode).To(Equal(http.StatusTooManyRequests))
		Expect(logged.String()).To(ContainSubstring("tunnel limit reached"))
	})

	It("refuses to bind anywhere but loopback", func() {
		_, err := StartWithLogger(GinkgoT().Context(), policy(), "", testToken, "0.0.0.0:0", nil)
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

		resp, err := apiGet(strings.TrimSuffix(p.URL(), "/api/v1") + "/api/v1/models")
		Expect(err).NotTo(HaveOccurred())

		// test cleanup: a close error changes no assertion
		defer func() { _ = resp.Body.Close() }()

		body, err := io.ReadAll(resp.Body)
		Expect(err).NotTo(HaveOccurred())
		Expect(string(body)).To(Equal("api GET /api/v1/models"))
	})

	Describe("relay failures", func() {
		// closedByProxy fails when the peer end of a pipe is not closed by the relay.
		closedByProxy := func(peer net.Conn) {
			GinkgoHelper()

			_, err := peer.Read(make([]byte, 1))
			Expect(errors.Is(err, io.EOF) || errors.Is(err, io.ErrClosedPipe)).To(BeTrue(),
				"the upstream connection was left open: %v", err)
		}

		// pipeWithDeadline gives the peer a read deadline while the pipe is still open, so a
		// connection the relay forgets to close fails the spec instead of hanging it.
		pipeWithDeadline := func() (net.Conn, net.Conn) {
			GinkgoHelper()

			upstream, peer := net.Pipe()
			Expect(peer.SetReadDeadline(time.Now().Add(time.Second))).To(Succeed())

			return upstream, peer
		}

		It("answers bad gateway when the relay cannot dial", func() {
			p := startWith(policy(), gateway{
				resolve: staticResolver{netip.MustParseAddr(publicAddress)},
				dial: func(context.Context, string, string) (net.Conn, error) {
					return nil, errors.New("test dial: unreachable")
				},
			})

			rec := httptest.NewRecorder()
			p.relay(rec, httptest.NewRequest(http.MethodConnect, "http://public.example:443", nil),
				"public.example", "443")

			Expect(rec.Code).To(Equal(http.StatusBadGateway))
			Expect(rec.Body.String()).To(ContainSubstring("upstream unavailable"))
		})

		It("refuses to tunnel when the writer cannot be hijacked", func() {
			upstream, peer := pipeWithDeadline()
			p := startWith(policy(), gateway{
				resolve: staticResolver{netip.MustParseAddr(publicAddress)},
				dial:    func(context.Context, string, string) (net.Conn, error) { return upstream, nil },
			})

			rec := httptest.NewRecorder()
			p.relay(rec, httptest.NewRequest(http.MethodConnect, "http://public.example:443", nil),
				"public.example", "443")

			Expect(rec.Code).To(Equal(http.StatusInternalServerError))
			Expect(rec.Body.String()).To(ContainSubstring("cannot tunnel"))
			closedByProxy(peer)
		})

		It("abandons the tunnel when the hijack fails", func() {
			upstream, peer := pipeWithDeadline()
			p := startWith(policy(), gateway{
				resolve: staticResolver{netip.MustParseAddr(publicAddress)},
				dial:    func(context.Context, string, string) (net.Conn, error) { return upstream, nil },
			})

			rec := failedHijacker{httptest.NewRecorder()}
			p.relay(&rec, httptest.NewRequest(http.MethodConnect, "http://public.example:443", nil),
				"public.example", "443")

			Expect(rec.Body.Len()).To(BeZero(), "wrote a response after the hijack failed")
			closedByProxy(peer)
		})
	})

	Describe("destination refusal", func() {
		It("refuses a CONNECT to every range that is not publicly routable", func() {
			// open egress: the refusal must not depend on the agent role's allowlist
			p := start(policy(), "")
			DeferCleanup(p.Close)

			for _, host := range refusedHosts() {
				resp, conn := connect(p.Address(), net.JoinHostPort(host, "443"))
				// test cleanup: a close error changes no assertion
				_ = conn.Close()

				Expect(resp.StatusCode).To(Equal(http.StatusForbidden), host)
				Expect(p.Domains()).To(ContainElement(host))
			}
		})

		It("refuses a plain HTTP request to every range that is not publicly routable", func() {
			p := start(policy(), "")
			DeferCleanup(p.Close)

			for _, host := range refusedHosts() {
				resp, conn := forward(p.Address(), "http://"+net.JoinHostPort(host, "80")+"/thing")
				// test cleanup: a close error changes no assertion
				_ = conn.Close()

				Expect(resp.StatusCode).To(Equal(http.StatusForbidden), host)
			}
		})

		It("refuses a name that resolves to a refused address", func() {
			p := startWith(policy(), gateway{
				resolve: staticResolver{
					netip.MustParseAddr(publicAddress), netip.MustParseAddr("10.1.2.3"),
				},
				dial: (&net.Dialer{}).DialContext,
			})

			resp, conn := connect(p.Address(), "internal.example:443")
			// test cleanup: a close error changes no assertion
			_ = conn.Close()

			Expect(resp.StatusCode).To(Equal(http.StatusForbidden))
			Expect(p.Domains()).To(ContainElement("internal.example"))

			resp, conn = forward(p.Address(), "http://internal.example/thing")
			// test cleanup: a close error changes no assertion
			_ = conn.Close()

			Expect(resp.StatusCode).To(Equal(http.StatusForbidden))
		})

		It("dials the address it checked instead of resolving the name again", func() {
			dialed := make(chan string, 1)

			p := startWith(policy(), gateway{
				resolve: staticResolver{netip.MustParseAddr(publicAddress)},
				dial: func(_ context.Context, _ string, address string) (net.Conn, error) {
					dialed <- address

					return nil, errors.New("test dial: not reaching anything")
				},
			})

			resp, conn := connect(p.Address(), "public.example:443")
			// test cleanup: a close error changes no assertion
			_ = conn.Close()

			var address string
			Eventually(dialed, "5s").Should(Receive(&address))

			Expect(resp.StatusCode).To(Equal(http.StatusBadGateway))
			Expect(address).To(Equal(publicAddress + ":443"))
		})

		It("bounds how many addresses one DNS answer can name", func() {
			addrs := make(staticResolver, 0, 50)
			for i := range 50 {
				addrs = append(addrs, netip.AddrFrom4([4]byte{93, 184, 216, byte(34 + i)}))
			}

			g := gateway{resolve: addrs, dial: (&net.Dialer{}).DialContext}

			checked, err := g.lookup(context.Background(), "many.example")
			Expect(err).NotTo(HaveOccurred())
			Expect(checked).To(HaveLen(maxResolvedAddresses),
				"a long DNS answer grew the dial list without bound")
		})

		It("answers bad gateway when the name cannot be resolved", func() {
			p := startWith(policy(), gateway{
				resolve: staticResolver{}, dial: (&net.Dialer{}).DialContext,
			})

			resp, conn := connect(p.Address(), "missing.example:443")
			// test cleanup: a close error changes no assertion
			_ = conn.Close()

			Expect(resp.StatusCode).To(Equal(http.StatusBadGateway))
		})
	})
})
