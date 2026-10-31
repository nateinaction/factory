package egress

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/nateinaction/factory/foreman/internal/sandbox"
	"github.com/nateinaction/factory/foreman/internal/security"
)

// headerSecret is a request header's value the event must never carry.
const headerSecret = "header-secret-value"

// loggedProxy binds a proxy over g whose diagnostics are JSON lines in the returned buffer, scoped
// to one run the way foreman scopes them.
func loggedProxy(n sandbox.Network, apiURL string, g gateway) (*Proxy, *logBuffer) {
	GinkgoHelper()

	var logged logBuffer

	logger := slog.New(slog.NewJSONHandler(&logged, nil)).
		With("agent_role", "Developer", "repository", "o/r", "issue", 7)

	p, err := startProxy(GinkgoT().Context(), n, apiURL, testToken, "127.0.0.1:0", logger, g, nil)
	Expect(err).NotTo(HaveOccurred())
	DeferCleanup(p.Close)

	return p, &logged
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

		if record["msg"] == "security event" {
			events = append(events, record)
		}
	}

	return events
}

// publicGateway resolves every name to a public address and fails any dial.
var publicGateway = gateway{ //nolint:gochecknoglobals // a read-only fixture
	resolve: staticResolver{netip.MustParseAddr(publicAddress)},
	dial: func(context.Context, string, string) (net.Conn, error) {
		return nil, errors.New("test dial: must not be reached")
	},
}

var _ = Describe("security events", func() {
	expectOne := func(logged *logBuffer, outcome, host string) {
		GinkgoHelper()

		events := securityEvents(logged)
		Expect(events).To(HaveLen(1))
		Expect(events[0]).To(And(
			HaveKeyWithValue("event", "egress_refused"),
			HaveKeyWithValue("outcome", outcome),
			HaveKeyWithValue("host", host),
			HaveKeyWithValue("agent_role", "Developer"),
			HaveKeyWithValue("repository", "o/r"),
			HaveKeyWithValue("issue", BeNumerically("==", 7)),
		))
	}

	It("reports a CONNECT the allowlist refuses", func() {
		p, logged := loggedProxy(policy("api.example.com"), "", publicGateway)

		_, conn := connect(p.Address(), "blocked.example:443")
		_ = conn.Close() // test cleanup

		expectOne(logged, "refused_by_policy", "blocked.example")
		Expect(p.Domains()).NotTo(ContainElement("blocked.example"))
		Expect(p.Refused()).To(Equal([]string{"blocked.example"}))
	})

	It("reports a plain HTTP request the allowlist refuses", func() {
		p, logged := loggedProxy(policy("api.example.com"), "", publicGateway)

		_, conn := forward(p.Address(), "http://blocked.example/thing")
		_ = conn.Close() // test cleanup

		expectOne(logged, "refused_by_policy", "blocked.example")
	})

	It("reports an API call the allowlist refuses", func() {
		api := apiServer()
		p, logged := loggedProxy(policy("api.example.com"), api.URL+"/api/v1", publicGateway)

		resp, err := apiGet(p.URL() + "/chat/completions")
		Expect(err).NotTo(HaveOccurred())
		Expect(resp.Body.Close()).To(Succeed())

		expectOne(logged, "refused_by_policy", APIDomain)
	})

	It("reports a destination that is not publicly routable, on both paths, as refused only", func() {
		p, logged := loggedProxy(policy(), "", gateway{
			resolve: staticResolver{netip.MustParseAddr("10.1.2.3")}, dial: publicGateway.dial,
		})

		_, conn := connect(p.Address(), "10.1.2.3:443")
		_ = conn.Close() // test cleanup
		_, conn = forward(p.Address(), "http://10.1.2.3/thing")
		_ = conn.Close() // test cleanup

		events := securityEvents(logged)
		Expect(events).To(HaveLen(2))

		for _, event := range events {
			Expect(event).To(HaveKeyWithValue("outcome", "refused_not_routable"))
			Expect(event).To(HaveKeyWithValue("host", "10.1.2.3"))
		}

		Expect(p.Domains()).To(BeEmpty())
		Expect(p.Refused()).To(Equal([]string{"10.1.2.3"}))
		Expect(p.Refusals()).To(Equal(map[security.Outcome]int{security.RefusedNotRoutable: 2}))
	})

	It("refuses a sandboxed run the collector's loopback OTLP address", func() {
		// srt sends the model's egress through this proxy and blocks it elsewhere, so a model asking
		// for the host's collector (factory_collector_http_port, 4318) is refused here, before a dial.
		p, logged := loggedProxy(policy(), "", gateway{
			resolve: staticResolver{netip.MustParseAddr("127.0.0.1")}, dial: publicGateway.dial,
		})

		_, conn := connect(p.Address(), "127.0.0.1:4318")
		_ = conn.Close() // test cleanup

		expectOne(logged, string(security.RefusedNotRoutable), "127.0.0.1")
		Expect(p.Refusals()).To(Equal(map[security.Outcome]int{security.RefusedNotRoutable: 1}))
		Expect(p.Domains()).NotTo(ContainElement("127.0.0.1"))
	})

	It("reports a tunnel refused at the limit, and lists the host as refused", func() {
		p, logged := loggedProxy(policy(), "", publicGateway)

		for range MaxTunnels {
			Expect(p.reserveTunnel()).To(BeTrue())
		}

		resp, conn := connect(p.Address(), "limit.example:443")
		_ = conn.Close() // test cleanup

		Expect(resp.StatusCode).To(Equal(http.StatusTooManyRequests))
		expectOne(logged, "refused_tunnel_limit", "limit.example")
		Expect(p.Domains()).To(BeEmpty())
		Expect(p.Refused()).To(Equal([]string{"limit.example"}))
	})

	It("reports an API call with a missing or wrong token and never carries the token", func() {
		api := apiServer()
		p, logged := loggedProxy(policy(), api.URL+"/api/v1", publicGateway)

		for _, authorization := range []string{"", "Bearer wrong-token-value"} {
			req, err := http.NewRequestWithContext(GinkgoT().Context(), http.MethodGet, p.URL()+"/x", nil)
			Expect(err).NotTo(HaveOccurred())
			// A header the event must never carry, whatever the outcome.
			req.Header.Set("X-Private", headerSecret)

			if authorization != "" {
				req.Header.Set("Authorization", authorization)
			}

			resp, err := http.DefaultClient.Do(req)
			Expect(err).NotTo(HaveOccurred())
			Expect(resp.Body.Close()).To(Succeed())
		}

		events := securityEvents(logged)
		Expect(events).To(HaveLen(2))

		for _, event := range events {
			Expect(event).To(HaveKeyWithValue("outcome", "refused_bad_token"))
		}

		Expect(logged.String()).NotTo(ContainSubstring("wrong-token-value"))
		Expect(logged.String()).NotTo(ContainSubstring(testToken))
		Expect(logged.String()).NotTo(ContainSubstring(headerSecret))
	})

	It("rate limits a flood of refusals while counting every one", func() {
		p, logged := loggedProxy(policy("api.example.com"), "", publicGateway)

		const refusals = 50

		for range refusals {
			resp, conn := connect(p.Address(), "blocked.example:443")
			// test cleanup: a close error changes no assertion
			_ = conn.Close()

			Expect(resp.StatusCode).To(Equal(http.StatusForbidden))
		}

		Expect(securityEvents(logged)).To(HaveLen(security.RefusalLogBurst),
			"a refusal flood reached the log without a limit")
		Expect(p.Refusals()).To(Equal(map[security.Outcome]int{security.RefusedByPolicy: refusals}))
	})

	It("emits no event for an allowed request", func() {
		echo, stop := echoServer()
		DeferCleanup(stop)

		p, logged := loggedProxy(policy(), "", loopbackGateway())

		resp, conn := connect(p.Address(), echo)
		_ = conn.Close() // test cleanup

		Expect(resp.StatusCode).To(Equal(http.StatusOK))
		Expect(securityEvents(logged)).To(BeEmpty())
		Expect(p.Refused()).To(BeEmpty())
	})
})
