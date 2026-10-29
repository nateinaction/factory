package foreman

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/nateinaction/factory/foreman/internal/config"
	"github.com/nateinaction/factory/foreman/internal/openrouter"
	"github.com/nateinaction/factory/foreman/internal/proc"
	"github.com/nateinaction/factory/foreman/internal/prompt"
	"github.com/nateinaction/factory/foreman/internal/sandbox"
	"github.com/nateinaction/factory/foreman/internal/workflow"
)

var _ = Describe("the opencode adapter", func() {
	var h *harness

	BeforeEach(func() {
		h = newHarness()
	})

	It("runs the named agent with repository config and external skills off", func() {
		Expect(h.runImpl(testAgent())).Error().NotTo(HaveOccurred())
		calls := h.opencodeCalls()
		Expect(calls).To(HaveLen(2), "want the run and its retro")

		for _, c := range calls {
			Expect(joined(c)).To(ContainSubstring("opencode run --agent implementer --format json"))
			Expect(joined(c)).NotTo(ContainSubstring("--model"))
			Expect(c.Env).To(HaveKeyWithValue("OPENCODE_DISABLE_PROJECT_CONFIG", "1"))
			Expect(c.Env).To(HaveKeyWithValue("OPENCODE_DISABLE_EXTERNAL_SKILLS", "1"))
		}
	})

	It("hands the model no OTEL_* variable, and replaces the inherited environment", func() {
		GinkgoT().Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://collector.invalid:4318")
		GinkgoT().Setenv("OTEL_EXPORTER_OTLP_HEADERS", "authorization=secret")

		Expect(h.runImpl(testAgent())).Error().NotTo(HaveOccurred())

		calls := h.opencodeCalls()
		Expect(calls).NotTo(BeEmpty())

		for _, c := range calls {
			Expect(c.ReplaceEnv).To(BeTrue(), "the model must not inherit foreman's environment")

			for name := range c.Env {
				Expect(name).NotTo(HavePrefix("OTEL_"))
			}
		}
	})

	It("gives each run fresh state, deletes it afterwards, and never continues by recency", func() {
		var seen []string

		h.runner.wrap(func(c proc.Cmd, base func(proc.Cmd) (string, error)) (string, error) {
			if isModel(c) {
				db := c.Env["OPENCODE_DB"]
				if len(seen) == 0 {
					Expect(db).NotTo(BeAnExistingFile(), "state not fresh")
				}

				seen = append(seen, db)
				Expect(os.WriteFile(db, []byte("session"), 0o600)).To(Succeed())
			}

			return base(c)
		})
		Expect(h.runImpl(testAgent())).Error().NotTo(HaveOccurred())
		Expect(seen).To(HaveLen(2))
		Expect(seen[1]).To(Equal(seen[0]), "run and retro share one state")
		Expect(seen[0]).To(HavePrefix(h.deps.Settings.WorkRoot))
		Expect(filepath.Dir(seen[0])).NotTo(BeAnExistingFile(), "state directory survived the run")
		Expect(h.runner.find("--continue")).To(BeEmpty(), "a session was continued by recency")
	})

	It("resumes the run's session for the retro by explicit id", func() {
		h.answerOpencode(okStream("ses_run"))
		Expect(h.runImpl(testAgent())).Error().NotTo(HaveOccurred())
		calls := h.opencodeCalls()
		Expect(calls).To(HaveLen(2))
		Expect(joined(calls[0])).NotTo(ContainSubstring("--session"))
		Expect(joined(calls[1])).To(ContainSubstring("--session ses_run"))
	})

	It("keeps the Reviewer's state for its retro, then deletes it", func() {
		h.reviewerGH(nil)
		h.roleWrites(workflow.Reviewer, 5, cleanReview)
		h.roleWritesRetro(workflow.Reviewer, validRetro)

		var seen []string

		h.runner.wrap(func(c proc.Cmd, base func(proc.Cmd) (string, error)) (string, error) {
			if isModel(c) {
				seen = append(seen, c.Env["OPENCODE_DB"])
			}

			return base(c)
		})
		Expect(Run(context.Background(), h.deps, workflow.Reviewer, testRef(5), testAgent(), testRetro(), 3)).
			Error().NotTo(HaveOccurred())
		Expect(seen).To(HaveLen(2), "the review and its retro")
		Expect(seen[0]).To(And(HavePrefix(h.deps.Settings.WorkRoot), HaveSuffix("opencode.db")),
			"the review's state does not outlive the process")
		Expect(seen[1]).To(Equal(seen[0]), "the retro cannot resume the review's session")
		Expect(filepath.Dir(seen[0])).NotTo(BeAnExistingFile(), "state survived the run and its retro")
	})

	It("gives the run the agent role's wall-clock deadline", func() {
		agent := testAgent()
		agent.DeadlineMinutes = 20
		Expect(h.runImpl(agent)).Error().NotTo(HaveOccurred())
		Expect(h.opencodeCalls()[0].Timeout).To(beNear(20 * time.Minute))
	})

	It("gives the Reviewer its own deadline", func() {
		h.reviewerGH(nil)
		h.roleWrites(workflow.Reviewer, 5, cleanReview)

		agent := testAgent()
		agent.DeadlineMinutes = 30
		cfg := testConfig()
		cfg.Agents[string(workflow.Reviewer)] = agent

		_, err := Run(
			context.Background(), h.deps, workflow.Reviewer, testRef(5),
			cfg.Agents[string(workflow.Reviewer)], cfg.RetroBudget(workflow.Reviewer), cfg.ReviewRoundLimit,
		)
		Expect(err).NotTo(HaveOccurred())
		Expect(h.opencodeCalls()[0].Timeout).To(beNear(30 * time.Minute))
	})

	It("fails the run at the agent role's step cap, without a retry", func() {
		h.answerOpencode(stepsStream(3))

		agent := testAgent()
		agent.Steps = 3
		_, err := h.runImpl(agent)
		Expect(err).To(MatchError(ContainSubstring("step cap")))
		Expect(h.runCalls()).To(HaveLen(1), "a step cap is not retried")
		Expect(h.securityEvents()).To(ConsistOf(And(
			HaveKeyWithValue("event", "run_capped"),
			HaveKeyWithValue("outcome", "step_cap"),
			HaveKeyWithValue("agent_role", "implementer"),
			HaveKeyWithValue("issue", "5"),
		)))
	})

	It("keeps a run that stays below its step cap", func() {
		h.answerOpencode(stepsStream(3))

		agent := testAgent()
		agent.Steps = 4
		Expect(h.runImpl(agent)).Error().NotTo(HaveOccurred())
	})

	It("fails the run when the model prints a line over the line limit", func() {
		// The step after the oversized line is never parsed, so the stream cannot be read as a run
		// that finished.
		h.answerOpencode(`{"type":"text","sessionID":"ses_1","part":{"text":"` +
			strings.Repeat("x", 4<<20) + `"}}` + "\n" + okStream("ses_1"))

		_, err := h.runImpl(testAgent())
		Expect(err).To(MatchError(ContainSubstring("line over")))
		Expect(h.runCalls()).To(HaveLen(1), "a stream foreman could not read is not retried")
	})

	DescribeTable("passes compaction per agent role as inline config",
		func(on bool) {
			agent := testAgent()
			agent.Compaction = on
			Expect(h.runImpl(agent)).Error().NotTo(HaveOccurred())

			var cfg struct {
				Compaction struct {
					Auto bool `json:"auto"`
				} `json:"compaction"`
			}
			Expect(json.Unmarshal([]byte(h.opencodeCalls()[0].Env["OPENCODE_CONFIG_CONTENT"]), &cfg)).To(Succeed())
			Expect(cfg.Compaction.Auto).To(Equal(on))
		},
		Entry("on", true), Entry("off", false),
	)

	It("fails a context overflow at once, without a retry", func() {
		h.answerOpencode(errStream("ContextOverflowError", 0))

		agent := testAgent()
		agent.Compaction = false
		_, err := h.runImpl(agent)
		Expect(err).To(MatchError(ContainSubstring("context overflow")))
		Expect(h.runCalls()).To(HaveLen(1))
	})

	DescribeTable("retries rate limits, 5xx and timeouts up to twice, and nothing else",
		func(out string, calls int, kind string) {
			h.answerOpencode(out)
			_, err := h.runImpl(testAgent())
			Expect(err).To(MatchError(ContainSubstring(kind)))
			Expect(h.runCalls()).To(HaveLen(calls))
			Expect(err.Error()).NotTo(Or(ContainSubstring("boom"), ContainSubstring("or-key")), "failure text leaks")
		},
		Entry("rate limit", errStream("APIError", 429), 3, "rate limit"),
		Entry("server error", errStream("APIError", 503), 3, "server error"),
		Entry("timeout", errStream("TimeoutError", 0), 3, "timeout"),
		Entry("out of credits", errStream("APIError", 402), 1, "out of credits"),
		Entry("unknown model", errStream("APIError", 404), 1, "unknown model"),
		Entry("anything else", errStream("APIError", 400), 1, "model error"),
	)

	It("finishes a run whose retry succeeds, after backing off twice", func() {
		h.answerOpencode(errStream("APIError", 429), errStream("APIError", 429), okStream("ses_1"))
		Expect(h.runImpl(testAgent())).To(BeZero())
		Expect(h.sleeps).To(HaveLen(2))
	})

	It("logs a permission denial at debug, where an operator can find it", func() {
		h.answerOpencode(`{"type":"permission","sessionID":"ses_1","permission":"bash","action":"deny"}` + "\n")
		Expect(h.runImpl(testAgent())).Error().NotTo(HaveOccurred())
		Expect(h.logs.String()).To(ContainSubstring(`"action":"deny"`))
		Expect(h.logs.StringFrom(slog.LevelInfo)).NotTo(ContainSubstring(`"action":"deny"`),
			"a denial is transcript, so it stays below the default level")
	})

	It("keeps the transcript out of the default info-level diagnostics", func() {
		h.answerOpencode(`{"type":"text","part":{"text":"private repository code"}}` + "\n")
		Expect(h.runImpl(testAgent())).Error().NotTo(HaveOccurred())
		Expect(h.logs.String()).To(ContainSubstring("private repository code"), "the transcript is still logged")
		Expect(h.logs.StringFrom(slog.LevelInfo)).NotTo(ContainSubstring("private repository code"))
	})

	It("removes the key from what it echoes to the log", func() {
		h.answerOpencode(`{"type":"error","error":{"name":"APIError",` +
			`"data":{"statusCode":400,"message":"bad key or-key"}}}` + "\n")
		_, err := h.runImpl(testAgent())
		Expect(err).To(HaveOccurred())
		Expect(h.logs.String()).NotTo(ContainSubstring("or-key"))
		Expect(h.out.String()).NotTo(ContainSubstring("or-key"))
	})

	It("never puts the run's proxy token in what it logs", func() {
		h.answerOpencode(`{"type":"error","error":{"name":"APIError",` +
			`"data":{"statusCode":401,"message":"bad token ` + testToken + `"}}}` + "\n")
		_, err := h.runImpl(testAgent())
		Expect(err).To(HaveOccurred())
		Expect(h.logs.String()).NotTo(ContainSubstring(testToken))
		Expect(h.out.String()).NotTo(ContainSubstring(testToken))
	})

	DescribeTable("redacts every secret the run holds from both of the model's streams",
		func(secret string, onStdout bool) {
			line := "echo " + secret + " done\n"
			if onStdout {
				h.answerOpencodeStreams(line, "")
			} else {
				h.answerOpencodeStreams("", line)
			}

			// The run's outcome is not under test, only what reached a log.
			_, _ = h.runImpl(testAgent())
			Expect(h.logs.String()).NotTo(ContainSubstring(secret))
			Expect(h.out.String()).NotTo(ContainSubstring(secret))
			Expect(h.errOut.String()).NotTo(ContainSubstring(secret))
			Expect(h.logs.String() + h.errOut.String()).To(ContainSubstring("[redacted]"))
		},
		Entry("OpenRouter key on stdout", "or-key", true),
		Entry("OpenRouter key on stderr", "or-key", false),
		Entry("proxy token on stdout", testToken, true),
		Entry("proxy token on stderr", testToken, false),
		Entry("read-only GitHub token on stdout", "read-only-token-implementer", true),
		Entry("read-only GitHub token on stderr", "read-only-token-implementer", false),
	)

	It("redacts a secret that arrives split across two stderr writes", func() {
		h.runner.wrap(func(c proc.Cmd, base func(proc.Cmd) (string, error)) (string, error) {
			if !isModel(c) {
				return base(c)
			}

			_, _ = io.WriteString(c.Stderr, "token run-prox")
			_, _ = io.WriteString(c.Stderr, "y-token end\n")

			return "", nil
		})
		_, _ = h.runImpl(testAgent())
		Expect(h.errOut.String()).NotTo(ContainSubstring("run-proxy-token"))
		Expect(h.errOut.String()).To(ContainSubstring("token [redacted] end"))
	})

	It("mints a fresh random proxy token for each run", func() {
		first, err := (Deps{}).token()
		Expect(err).NotTo(HaveOccurred())
		second, err := (Deps{}).token()
		Expect(err).NotTo(HaveOccurred())

		Expect(first).To(MatchRegexp(`^[0-9a-f]{64}$`))
		Expect(second).To(MatchRegexp(`^[0-9a-f]{64}$`))
		Expect(first).NotTo(Equal(second))
	})

	Describe("the usage line", func() {
		It("takes the tokens and cost the run's proxy read", func() {
			input, output, cached, cost := 70, 7, 3, 0.125
			proxy := &fakeProxy{
				url: "http://127.0.0.1:9/api/v1",
				usage: openrouter.Usage{
					Input: &input, Output: &output, Cached: &cached, Cost: &cost,
				},
			}
			h.deps.Proxy = func(key, _, _ string) (Forwarder, error) {
				proxy.key = key

				return proxy, nil
			}
			egress := &fakeEgress{
				url: proxy.url, address: "127.0.0.1:9", domains: []string{"api.example.com"},
				refused: []string{"blocked.example"},
			}
			h.deps.Egress = func(_ context.Context, _ sandbox.Network, _, _ string) (EgressProxy, error) { return egress, nil }

			Expect(h.runImpl(testAgent())).Error().NotTo(HaveOccurred())
			lines := h.usageLines()
			Expect(lines).To(HaveLen(1), h.out.String())
			Expect(lines[0]).To(SatisfyAll(
				HaveKeyWithValue("input_tokens", 70.0), HaveKeyWithValue("output_tokens", 7.0),
				HaveKeyWithValue("cached_tokens", 3.0), HaveKeyWithValue("cost", 0.125),
				HaveKeyWithValue("peak_context", 1400.0),
			), "the adapter still reports the context peak")
			Expect(proxy.key).To(Equal("or-key"), "the proxy never got the key")
			Expect(proxy.closed).To(BeTrue(), "the proxy outlived the run")
			Expect(egress.closed).To(BeTrue(), "the egress proxy outlived the run")
			Expect(baseURL(h.opencodeCalls()[0])).To(Equal(egress.url))
			Expect(h.opencodeCalls()[0].Env).NotTo(HaveKey("OPENROUTER_API_KEY"))
			Expect(lines[0]).To(HaveKeyWithValue("domains", []any{"api.example.com"}))
			Expect(lines[0]).To(HaveKeyWithValue("refused_domains", []any{"blocked.example"}),
				"refused domains are listed apart from allowed ones")
		})

		It("is one JSON line per run, summed over the run and its retro", func() {
			h.deps.Host = "pi-1"
			Expect(h.runImpl(testAgent())).Error().NotTo(HaveOccurred())
			lines := h.usageLines()
			Expect(lines).To(HaveLen(1), h.out.String())
			Expect(lines[0]).To(SatisfyAll(
				HaveKeyWithValue("repository", "o/r"), HaveKeyWithValue("issue", 5.0),
				HaveKeyWithValue("agent_role", "implementer"), HaveKeyWithValue("model", "prov/model"),
				HaveKeyWithValue("worker_host", "pi-1"), HaveKeyWithValue("input_tokens", 2000.0),
				HaveKeyWithValue("output_tokens", 100.0), HaveKeyWithValue("cached_tokens", 800.0),
				HaveKeyWithValue("context_cap", 200000.0), HaveKeyWithValue("peak_context", 1400.0),
				HaveKeyWithValue("cost", 0.004),
			))
			Expect(lines[0]).To(HaveKeyWithValue("missing", []any{}), "domains are reported by the egress proxy")
		})

		It("lists a field the adapter cannot report as missing, not zero", func() {
			h.answerOpencode(`{"type":"text","sessionID":"ses_1"}` + "\n")
			Expect(h.runImpl(testAgent())).Error().NotTo(HaveOccurred())
			lines := h.usageLines()
			Expect(lines).To(HaveLen(1), h.out.String())

			for _, k := range []string{"input_tokens", "output_tokens", "cached_tokens", "peak_context", "cost", "worker_host"} {
				Expect(lines[0]).NotTo(HaveKey(k))
			}

			Expect(lines[0]["missing"]).To(HaveLen(6))
		})

		It("carries the duration, the attempts and the outcome of a run that succeeds", func() {
			// A clock that moves one second per reading makes the duration a whole number of seconds.
			tick := time.Date(2026, 10, 28, 10, 0, 0, 0, time.UTC)
			h.deps.Now = func() time.Time { tick = tick.Add(time.Second); return tick }

			Expect(h.runImpl(testAgent())).Error().NotTo(HaveOccurred())
			lines := h.usageLines()
			Expect(lines).To(HaveLen(1))
			Expect(lines[0]).To(SatisfyAll(
				HaveKeyWithValue("attempts", 1.0), HaveKeyWithValue("outcome", "handed_off"),
				Not(HaveKey("failure_kind")),
			))
			Expect(lines[0]["duration_ms"]).To(BeNumerically(">=", 1000))
			duration, isNumber := lines[0]["duration_ms"].(float64)
			Expect(isNumber).To(BeTrue(), "duration_ms is not a number")
			Expect(math.Mod(duration, 1000)).To(BeZero())
			Expect(lines[0]["missing"]).NotTo(ContainElements("duration_ms", "attempts"))
		})

		It("counts the attempts of a model that is retried and then succeeds", func() {
			h.answerOpencode(errStream("APIError", 429), okStream("ses_1"))
			Expect(h.runImpl(testAgent())).Error().NotTo(HaveOccurred())

			lines := h.usageLines()
			Expect(lines).To(HaveLen(1))
			Expect(lines[0]).To(SatisfyAll(
				HaveKeyWithValue("attempts", 2.0), HaveKeyWithValue("outcome", "handed_off"),
				Not(HaveKey("failure_kind")),
			))
		})

		DescribeTable("names the failure kind of a run that fails",
			func(stream string, attempts float64, kind string, agent func(config.AgentConfig) config.AgentConfig) {
				h.answerOpencode(stream)
				_, err := h.runImpl(agent(testAgent()))
				Expect(err).To(HaveOccurred())

				lines := h.usageLines()
				Expect(lines).To(HaveLen(1))
				Expect(lines[0]).To(SatisfyAll(
					HaveKeyWithValue("outcome", "failed"), HaveKeyWithValue("failure_kind", kind),
					HaveKeyWithValue("attempts", attempts),
				))
			},
			Entry("rate limit", errStream("APIError", 429), 3.0, "rate limit", same),
			Entry("server", errStream("APIError", 503), 3.0, "server error", same),
			Entry("timeout", errStream("TimeoutError", 0), 3.0, "timeout", same),
			Entry("step cap", stepsStream(3), 1.0, "step cap", func(a config.AgentConfig) config.AgentConfig {
				a.Steps = 3

				return a
			}),
			Entry("other", errStream("APIError", 400), 1.0, "model error", same),
		)

		It("lists the attempts as missing for a run that failed before the model ran", func() {
			h.deps.Settings.KeyFile = filepath.Join(h.dir, "no-such-key")
			_, err := h.runImpl(testAgent())
			Expect(err).To(HaveOccurred())

			lines := h.usageLines()
			Expect(lines).To(HaveLen(1))
			Expect(lines[0]).To(SatisfyAll(
				HaveKeyWithValue("outcome", "failed"), HaveKeyWithValue("failure_kind", "model error"),
				Not(HaveKey("attempts")),
			))
			Expect(lines[0]["missing"]).To(ContainElement("attempts"))
		})

		It("reports a review that found problems as rework", func() {
			h.reviewerGH(nil)
			h.roleWrites(workflow.Reviewer, 5, blockingReview)
			Expect(Run(context.Background(), h.deps, workflow.Reviewer, testRef(5), testAgent(), testRetro(), 3)).
				To(Equal(workflow.ReviewFindings))
			Expect(h.usageLines()).To(ConsistOf(HaveKeyWithValue("outcome", "rework")))
		})

		It("is still logged when the run fails", func() {
			h.answerOpencode(okStream("ses_1") + errStream("APIError", 400))
			_, err := h.runImpl(testAgent())
			Expect(err).To(HaveOccurred())

			lines := h.usageLines()
			Expect(lines).To(HaveLen(1))
			// The failed run and the retro that follows it both spent a step of 1000 input tokens.
			Expect(lines[0]).To(HaveKeyWithValue("input_tokens", 2000.0))
		})
	})

	It("keeps the prompt prefix byte-identical across runs, role prompt before repository docs", func() {
		// promptOf runs the Implementer under its own work root, with a clone that leaves an AGENTS.md,
		// and returns the prompt and the clone it ran in.
		promptOf := func(root string) (string, string) {
			h := newHarness()
			h.deps.Settings.WorkRoot = filepath.Join(h.dir, root, "work")
			h.runner.wrap(func(c proc.Cmd, base func(proc.Cmd) (string, error)) (string, error) {
				if len(c.Argv) > 3 && c.Argv[0] == "git" && c.Argv[1] == "clone" {
					dest := c.Argv[len(c.Argv)-1]
					Expect(os.MkdirAll(dest, 0o700)).To(Succeed())
					Expect(os.WriteFile(filepath.Join(dest, "AGENTS.md"), []byte("AGENTS-MD-CONTENT"), 0o600)).To(Succeed())
				}

				return base(c)
			})
			Expect(h.runImpl(testAgent())).Error().NotTo(HaveOccurred())
			argv := h.opencodeCalls()[0].Argv

			return argv[len(argv)-1], h.rolePaths(workflow.Implementer, 5).workdir
		}
		a, cloned := promptOf("a")
		b, _ := promptOf("b")
		prefixA, _, found := strings.Cut(a, prompt.IssueMaterialHeading)
		Expect(found).To(BeTrue(), "no issue material")

		prefixB, _, _ := strings.Cut(b, prompt.IssueMaterialHeading)
		Expect(prefixA).To(Equal(prefixB))
		Expect(strings.Index(prefixA, "implement")).To(BeNumerically("<", strings.Index(prefixA, "AGENTS-MD-CONTENT")))
		Expect(prefixA).NotTo(Or(
			ContainSubstring(cloned), ContainSubstring(filepath.Base(cloned)),
			ContainSubstring("#5"), ContainSubstring("work")))
	})
})

// same leaves an agent role's config as it is.
func same(a config.AgentConfig) config.AgentConfig { return a }
