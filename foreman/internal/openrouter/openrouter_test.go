package openrouter

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// upstreamCall is one request an upstream test server received.
type upstreamCall struct {
	authorization string
	path          string
	body          string
	header        http.Header
}

// upstream is a test server standing in for OpenRouter's API.
type upstream struct {
	*httptest.Server
	calls []upstreamCall
	// respond answers a call; nil answers 200 with no usage.
	respond func(w http.ResponseWriter, r *http.Request)
}

// newUpstream starts a test upstream answering through respond.
func newUpstream(respond func(http.ResponseWriter, *http.Request)) *upstream {
	u := &upstream{respond: respond}
	u.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "read body", http.StatusBadRequest)

			return
		}

		u.calls = append(u.calls, upstreamCall{
			authorization: r.Header.Get("Authorization"), path: r.URL.Path, body: string(body),
			header: r.Header.Clone(),
		})

		if u.respond == nil {
			w.WriteHeader(http.StatusOK)
			return
		}

		u.respond(w, r)
	}))

	return u
}

// proxyClient is an HTTP client that never follows a redirect away from the proxy.
func proxyClient() *http.Client {
	return &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
}

// get asks for a URL and returns the answer.
func get(rawURL string) (*http.Response, string) {
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, rawURL, nil)
	Expect(err).NotTo(HaveOccurred())

	resp, err := proxyClient().Do(req)
	Expect(err).NotTo(HaveOccurred())
	DeferCleanup(resp.Body.Close)

	body, err := io.ReadAll(resp.Body)
	Expect(err).NotTo(HaveOccurred())

	return resp, string(body)
}

var _ = Describe("the OpenRouter proxy", func() {
	var up *upstream

	AfterEach(func() {
		if up != nil {
			up.Close()
		}
	})

	It("adds its key and forwards to OpenRouter's API", func() {
		up = newUpstream(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")

			_, err := w.Write([]byte(`{"usage":{"prompt_tokens":194,"completion_tokens":2,` +
				`"cost":0.95,"prompt_tokens_details":{"cached_tokens":100}}}`))
			if err != nil {
				return
			}
		})
		p, err := start("sk-or-test", Loopback, up.URL)
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(p.Close)

		req, err := http.NewRequestWithContext(
			context.Background(), http.MethodPost, p.URL()+"/chat/completions?trace=1", strings.NewReader("hi"),
		)
		Expect(err).NotTo(HaveOccurred())
		req.Header.Set("Authorization", "Bearer the-model-invented-this")
		req.Header.Set("Content-Type", "application/json")

		resp, err := proxyClient().Do(req)
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(resp.Body.Close)

		_, err = io.ReadAll(resp.Body)
		Expect(err).NotTo(HaveOccurred())

		Expect(up.calls).To(HaveLen(1))
		Expect(up.calls[0].authorization).To(Equal("Bearer sk-or-test"), "the proxy replaced the key")
		Expect(up.calls[0].path).To(Equal("/api/v1/chat/completions"))
		Expect(up.calls[0].body).To(Equal("hi"))

		Expect(p.Usage().Input).To(HaveValue(BeEquivalentTo(194)))
		Expect(p.Usage().Output).To(HaveValue(BeEquivalentTo(2)))
		Expect(p.Usage().Cached).To(HaveValue(BeEquivalentTo(100)))
		Expect(p.Usage().Cost).To(HaveValue(BeEquivalentTo(0.95)))
	})

	It("passes a streamed response through and reads the usage it ends with", func() {
		up = newUpstream(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")

			for _, line := range []string{
				"data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n",
				"data: {\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":4,\"cost\":0.5}}\n\n",
				"data: [DONE]\n\n",
			} {
				if _, err := io.WriteString(w, line); err != nil {
					return
				}

				if err := http.NewResponseController(w).Flush(); err != nil {
					return
				}
			}
		})
		p, err := start("sk-or-test", Loopback, up.URL)
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(p.Close)

		resp, body := get(p.URL() + "/chat/completions")
		Expect(resp.StatusCode).To(Equal(http.StatusOK))
		Expect(body).To(ContainSubstring(`"delta":{"content":"hi"}`))
		Expect(body).To(HaveSuffix("data: [DONE]\n\n"))
		Expect(p.Usage().Input).To(HaveValue(BeEquivalentTo(10)))
		Expect(p.Usage().Output).To(HaveValue(BeEquivalentTo(4)))
		Expect(p.Usage().Cost).To(HaveValue(BeEquivalentTo(0.5)))
	})

	It("strips a header the client names in Connection before forwarding", func() {
		up = newUpstream(nil)
		p, err := start("sk-or-test", Loopback, up.URL)
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(p.Close)

		req, err := http.NewRequestWithContext(
			GinkgoT().Context(), http.MethodGet, p.URL()+"/models", nil,
		)
		Expect(err).NotTo(HaveOccurred())
		req.Header.Set("Connection", "X-Internal-Token")
		req.Header.Set("X-Internal-Token", "secret")

		resp, err := proxyClient().Do(req)
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(resp.Body.Close)

		_, err = io.ReadAll(resp.Body)
		Expect(err).NotTo(HaveOccurred())

		Expect(up.calls).To(HaveLen(1))
		Expect(up.calls[0].header.Values("X-Internal-Token")).To(BeEmpty())
		Expect(up.calls[0].header.Values("Connection")).To(BeEmpty())
	})

	It("passes each streamed event on before the upstream sends the next one", func() {
		release := make(chan struct{})

		var once sync.Once

		unblock := func() { once.Do(func() { close(release) }) }
		DeferCleanup(unblock)

		up = newUpstream(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")

			_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"first\"}}]}\n\n")
			_ = http.NewResponseController(w).Flush()

			<-release

			_, _ = io.WriteString(w, "data: {\"usage\":{\"prompt_tokens\":5,\"completion_tokens\":2}}\n\n")
			_, _ = io.WriteString(w, "data: [DONE]\n\n")
			_ = http.NewResponseController(w).Flush()
		})
		p, err := start("sk-or-test", Loopback, up.URL)
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(p.Close)

		ctx, cancel := context.WithTimeout(GinkgoT().Context(), 10*time.Second)
		DeferCleanup(cancel)

		req, err := http.NewRequestWithContext(
			ctx, http.MethodGet, p.URL()+"/chat/completions", nil,
		)
		Expect(err).NotTo(HaveOccurred())

		resp, err := proxyClient().Do(req)
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(resp.Body.Close)

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
		Expect(got.line).To(ContainSubstring(`"content":"first"`),
			"the first event reached the model before the upstream sent the rest")

		unblock()

		rest, err := io.ReadAll(reader)
		Expect(err).NotTo(HaveOccurred())
		Expect(string(rest)).To(ContainSubstring(`"prompt_tokens":5`))
		Expect(string(rest)).To(HaveSuffix("data: [DONE]\n\n"))

		Expect(p.Usage().Input).To(HaveValue(BeEquivalentTo(5)))
		Expect(p.Usage().Output).To(HaveValue(BeEquivalentTo(2)))
	})

	It("refuses a path outside OpenRouter's API without calling the upstream", func() {
		up = newUpstream(nil)
		p, err := start("sk-or-test", Loopback, up.URL)
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(p.Close)

		for _, path := range []string{"/chat/completions", "/api/v1x/chat/completions", "/api/v1/../secret"} {
			resp, _ := get(strings.TrimSuffix(p.URL(), APIPrefix) + path)
			Expect(resp.StatusCode).To(Equal(http.StatusForbidden), path)
		}

		Expect(up.calls).To(BeEmpty())
	})

	It("refuses a request that names another destination", func() {
		up = newUpstream(nil)
		p, err := start("sk-or-test", Loopback, up.URL)
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(p.Close)

		addr := strings.TrimPrefix(p.URL(), "http://")
		addr, _, found := strings.Cut(addr, "/")
		Expect(found).To(BeTrue())

		for _, request := range []string{
			"GET http://evil.example/api/v1/chat/completions HTTP/1.1\r\nHost: evil.example\r\n" +
				"Connection: close\r\n\r\n",
			"CONNECT evil.example:443 HTTP/1.1\r\nHost: evil.example:443\r\nConnection: close\r\n\r\n",
		} {
			Expect(rawRequest(addr, request)).To(ContainSubstring("403 Forbidden"))
		}

		Expect(up.calls).To(BeEmpty())
	})

	It("listens on loopback only", func() {
		up = newUpstream(nil)

		_, err := start("sk-or-test", "0.0.0.0:0", up.URL)
		Expect(err).To(MatchError(ContainSubstring("loopback")))

		p, err := start("sk-or-test", Loopback, up.URL)
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(p.Close)

		Expect(p.URL()).To(MatchRegexp(`^http://127\.0\.0\.1:\d+/api/v1$`))
	})

	It("fails to start when its address is taken", func() {
		up = newUpstream(nil)

		taken, err := net.Listen("tcp", Loopback)
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(taken.Close)

		_, err = start("sk-or-test", taken.Addr().String(), up.URL)
		Expect(err).To(MatchError(ContainSubstring("bind")))
		Expect(err.Error()).NotTo(ContainSubstring("sk-or-test"), "the key leaked into an error")
	})

	It("stops when it is closed", func() {
		up = newUpstream(nil)
		p, err := start("sk-or-test", Loopback, up.URL)
		Expect(err).NotTo(HaveOccurred())

		Expect(p.Close()).To(Succeed())

		req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, p.URL()+"/chat/completions", nil)
		Expect(err).NotTo(HaveOccurred())

		_, err = proxyClient().Do(req)
		Expect(err).To(HaveOccurred())
		Expect(up.calls).To(BeEmpty())
	})

	It("never puts its key in what it answers when the upstream is unreachable", func() {
		p, err := start("sk-or-test", Loopback, "http://127.0.0.1:1")
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(p.Close)

		resp, body := get(p.URL() + "/chat/completions")
		Expect(resp.StatusCode).To(Equal(http.StatusBadGateway))
		Expect(body).NotTo(ContainSubstring("sk-or-test"))
	})
})

// rawRequest writes one HTTP/1.1 request to addr and returns the raw response.
func rawRequest(addr, request string) string {
	conn, err := (&net.Dialer{}).DialContext(context.Background(), "tcp", addr)
	Expect(err).NotTo(HaveOccurred())
	DeferCleanup(conn.Close)

	_, err = fmt.Fprint(conn, request)
	Expect(err).NotTo(HaveOccurred())

	var buf bytes.Buffer

	_, err = io.Copy(&buf, bufio.NewReader(conn))
	Expect(err).NotTo(HaveOccurred())

	return buf.String()
}
