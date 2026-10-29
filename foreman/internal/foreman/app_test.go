package foreman

import (
	"context"
	"path/filepath"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/nateinaction/factory/foreman/internal/workflow"
)

var _ = Describe("Main", func() {
	var h *harness

	BeforeEach(func() {
		h = newHarness()
	})

	// main runs one invocation against the harness.
	main := func(args ...string) int {
		return Main(context.Background(), args, h.deps)
	}

	Context("with a valid config", func() {
		BeforeEach(func() {
			h.withConfig()
		})

		DescribeTable("rejects a bad invocation with the usage exit, doing nothing",
			func(args ...string) {
				Expect(main(args...)).To(Equal(ExitUsage))
				Expect(h.logs.String()).To(And(
					ContainSubstring("ERROR bad invocation"),
					ContainSubstring("reason="),
					ContainSubstring("usage="),
				))
				h.expectNoWork()
			},
			Entry("no verb"),
			Entry("unknown verb", "frobnicate"),
			Entry("poll with arguments", "poll", "extra"),
			Entry("start alone", "start"),
			Entry("start without an issue", "start", "implementer", "o/r"),
			Entry("start with an extra argument", "start", "implementer", "o/r", "5", "extra"),
			Entry("run without an issue", "run", "implementer", "o/r"),
			Entry("a malformed repository", "start", "implementer", "not-a-repo", "5"),
			Entry("a word for an issue", "start", "implementer", "o/r", "five"),
			Entry("an injected issue", "run", "implementer", "o/r", "5; rm -rf /"),
		)

		DescribeTable("fails closed on an unknown agent role",
			func(verb string) {
				Expect(main(verb, "janitor", "o/r", "5")).NotTo(BeZero())
				Expect(h.logs.String()).To(ContainSubstring(`unknown agent role "janitor"`))
				h.expectNoWork()
			},
			Entry(nil, "start"), Entry(nil, "run"),
		)

		It("exits non-zero for a known agent role without behaviour", func() {
			Expect(main("start", "planner", "o/r", "5")).NotTo(BeZero())
		})

		It("polls where the configured agent roles' Apps are installed", func() {
			h.queue(nil)
			Expect(main("poll")).To(BeZero(), h.logs.String())
			Expect(h.gh.Calls()[0].Path).To(Equal("/repos/o/r/issues"))
		})

		It("returns the review verdict as the exit code of run", func() {
			h.reviewerGH(nil)
			h.opencodeWrites("code-review", h.reviewOut(), blockingReview)
			Expect(main("run", "reviewer", "o/r", "5")).To(Equal(workflow.ReviewFindings), h.logs.String())
			Expect(h.runner.find("opencode run --agent reviewer")).To(HaveLen(1), "model from config not used")
		})

		It("runs the whole Implementer handoff on start", func() {
			h.acceptWrites("5")
			Expect(main("start", "implementer", "o/r", "5")).To(BeZero(), h.logs.String())
		})

		It("never puts a token in an error", func() {
			h.gh.Status("GET /repos/o/r/issues", 500)
			Expect(main("poll")).To(Equal(1))
			Expect(h.logs.String()).To(ContainSubstring("ERROR foreman failed"))
			Expect(h.logs.String()).NotTo(ContainSubstring("minted-token"))
		})
	})

	DescribeTable("fails closed on a bad config",
		func(cfg string) {
			h.withConfigText(cfg)
			Expect(main("poll")).NotTo(BeZero())
		},
		Entry("empty", ``),
		Entry("no fields", `{}`),
		Entry("unknown role", `{"roles":["janitor"],"agents":{}}`),
		Entry("no agents", `{"roles":["implementer"],"agents":{}}`),
		Entry("no roles", `{"roles":[],"agents":{}}`),
		Entry("roles missing", `{"agents":{}}`),
		Entry("not JSON", `[`),
	)

	It("fails closed on a missing config", func() {
		h.deps.Settings.ConfigFile = filepath.Join(h.dir, "missing.json")

		Expect(main("poll")).NotTo(BeZero())
	})
})
