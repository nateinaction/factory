package foreman

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/nateinaction/factory/foreman/internal/openrouter"
	"github.com/nateinaction/factory/foreman/internal/proc"
	"github.com/nateinaction/factory/foreman/internal/prompt"
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

	It("gives each run fresh state, deletes it afterwards, and never continues by recency", func() {
		stale := filepath.Join(h.deps.Settings.WorkRoot, "state-implementer-5")
		Expect(os.MkdirAll(stale, 0o700)).To(Succeed())
		Expect(os.WriteFile(filepath.Join(stale, "opencode.db"), []byte("old session"), 0o600)).To(Succeed())

		var seen []string

		h.runner.wrap(func(c proc.Cmd, base func(proc.Cmd) (string, error)) (string, error) {
			if c.Argv[0] == "opencode" {
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

	It("keeps the Reviewer's state in memory", func() {
		h.reviewerGH(nil)
		h.opencodeWrites("code-review", h.reviewOut(), cleanReview)
		Expect(Run(context.Background(), h.deps, workflow.Reviewer, "o/r", testConfig(), 5)).
			Error().NotTo(HaveOccurred())
		calls := h.opencodeCalls()
		Expect(calls).To(HaveLen(2), "the review and its retro")

		for _, c := range calls {
			Expect(c.Env).To(HaveKeyWithValue("OPENCODE_DB", ":memory:"))
		}
	})

	It("gives the run the agent role's wall-clock deadline", func() {
		agent := testAgent()
		agent.DeadlineMinutes = 20
		Expect(h.runImpl(agent)).Error().NotTo(HaveOccurred())
		Expect(h.opencodeCalls()[0].Timeout).To(beNear(20 * time.Minute))
	})

	It("gives the Reviewer its own deadline", func() {
		h.reviewerGH(nil)
		h.opencodeWrites("code-review", h.reviewOut(), cleanReview)

		agent := testAgent()
		agent.DeadlineMinutes = 30
		cfg := testConfig()
		cfg.Agents[string(workflow.Reviewer)] = agent

		_, err := Run(context.Background(), h.deps, workflow.Reviewer, "o/r", cfg, 5)
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
	})

	It("keeps a run that stays below its step cap", func() {
		h.answerOpencode(stepsStream(3))

		agent := testAgent()
		agent.Steps = 4
		Expect(h.runImpl(agent)).Error().NotTo(HaveOccurred())
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

	It("shows a permission denial in the log", func() {
		h.answerOpencode(`{"type":"permission","sessionID":"ses_1","permission":"bash","action":"deny"}` + "\n")
		Expect(h.runImpl(testAgent())).Error().NotTo(HaveOccurred())
		Expect(h.logs.String()).To(ContainSubstring(`"action":"deny"`), "a denial must reach the dagu run log")
	})

	It("removes the key from what it echoes to the log", func() {
		h.answerOpencode(`{"type":"error","error":{"name":"APIError",` +
			`"data":{"statusCode":400,"message":"bad key or-key"}}}` + "\n")
		_, err := h.runImpl(testAgent())
		Expect(err).To(HaveOccurred())
		Expect(h.logs.String()).NotTo(ContainSubstring("or-key"))
		Expect(h.out.String()).NotTo(ContainSubstring("or-key"))
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
			h.deps.Proxy = func(key, _ string) (Forwarder, error) {
				proxy.key = key

				return proxy, nil
			}

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
			Expect(baseURL(h.opencodeCalls()[0])).To(Equal(proxy.url))
			Expect(h.opencodeCalls()[0].Env).NotTo(HaveKey("OPENROUTER_API_KEY"))
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
			Expect(lines[0]).To(HaveKeyWithValue("missing", []any{"domains"}), "domains are not reported yet")
		})

		It("lists a field the adapter cannot report as missing, not zero", func() {
			h.answerOpencode(`{"type":"text","sessionID":"ses_1"}` + "\n")
			Expect(h.runImpl(testAgent())).Error().NotTo(HaveOccurred())
			lines := h.usageLines()
			Expect(lines).To(HaveLen(1), h.out.String())

			for _, k := range []string{"input_tokens", "output_tokens", "cached_tokens", "peak_context", "cost", "worker_host"} {
				Expect(lines[0]).NotTo(HaveKey(k))
			}

			Expect(lines[0]["missing"]).To(HaveLen(7))
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
		// promptOf runs the Implementer under its own work root, with a clone that leaves an AGENTS.md.
		promptOf := func(root string) string {
			h := newHarness()
			h.deps.Settings.WorkRoot = filepath.Join(h.dir, root, "work")
			h.runner.wrap(func(c proc.Cmd, base func(proc.Cmd) (string, error)) (string, error) {
				if len(c.Argv) > 3 && c.Argv[0] == "git" && c.Argv[1] == "clone" {
					Expect(os.MkdirAll(c.Argv[3], 0o700)).To(Succeed())
					Expect(os.WriteFile(filepath.Join(c.Argv[3], "AGENTS.md"), []byte("AGENTS-MD-CONTENT"), 0o600)).To(Succeed())
				}

				return base(c)
			})
			Expect(h.runImpl(testAgent())).Error().NotTo(HaveOccurred())
			argv := h.opencodeCalls()[0].Argv

			return argv[len(argv)-1]
		}
		a, b := promptOf("a"), promptOf("b")
		prefixA, _, found := strings.Cut(a, prompt.IssueMaterialHeading)
		Expect(found).To(BeTrue(), "no issue material")

		prefixB, _, _ := strings.Cut(b, prompt.IssueMaterialHeading)
		Expect(prefixA).To(Equal(prefixB))
		Expect(strings.Index(prefixA, "implement")).To(BeNumerically("<", strings.Index(prefixA, "AGENTS-MD-CONTENT")))
		Expect(prefixA).NotTo(Or(ContainSubstring("issue-5"), ContainSubstring("#5"), ContainSubstring("work")))
	})
})
