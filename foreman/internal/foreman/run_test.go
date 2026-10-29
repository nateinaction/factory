package foreman

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/nateinaction/factory/foreman/internal/proc"
	"github.com/nateinaction/factory/foreman/internal/workflow"
)

var _ = Describe("Run", func() {
	var h *harness

	BeforeEach(func() {
		h = newHarness()
	})

	// run runs role on issue 5 of o/r.
	run := func(role workflow.Role) (int, error) {
		return Run(context.Background(), h.deps, role, "o/r", testAgent, 5)
	}

	Describe("the Implementer", func() {
		BeforeEach(func() {
			h.implementerGH(nil, nil)
			h.writeToken(workflow.Implementer, "repo-token\n")
		})

		// retroOut is where the retro of issue 5 is written.
		retroOut := func() string { return filepath.Join(h.deps.Settings.WorkRoot, "issue-5.retro.md") }

		It("on a first run clones, runs the model, force-pushes, opens the PR and posts the retro", func() {
			h.opencodeWrites("--session", retroOut(), "lessons\n")
			Expect(run(workflow.Implementer)).To(BeZero())

			work := filepath.Join(h.deps.Settings.WorkRoot, "issue-5")
			Expect(h.runner.find("git clone https://github.com/o/r.git " + work)).To(HaveLen(1))
			Expect(h.runner.find("checkout -B agent/issue-5")).To(HaveLen(1))
			Expect(h.runner.find("fetch origin")).To(BeEmpty())

			oc := h.runner.find("opencode run --agent implementer --format json Use the `implement` skill")
			Expect(oc).To(HaveLen(1))
			Expect(oc[0].Dir).To(Equal(work))
			Expect(oc[0].Env).To(HaveKeyWithValue("OPENROUTER_API_KEY", "or-key"))
			Expect(oc[0].Timeout).To(beNear(AgentTimeout))
			Expect(joined(oc[0])).NotTo(ContainSubstring("Review:"), "first run carries review feedback")

			push := h.runner.find("noreply.github.com push")
			Expect(push).To(HaveLen(1))
			Expect(joined(push[0])).To(HaveSuffix("push --force origin agent/issue-5"))
			Expect(push[0].Env).To(HaveKeyWithValue("GIT_CONFIG_VALUE_0", Not(BeEmpty())), "push without credentials in env")

			Expect(h.gh.Bodies("POST", "/repos/o/r/pulls")).To(ConsistOf(And(
				ContainSubstring(`"title":"Add x"`), ContainSubstring(`"head":"agent/issue-5"`),
				ContainSubstring(`"base":"main"`), ContainSubstring(`Closes #5\n\nOpened by the factory agent.`),
			)))
			Expect(h.gh.Bodies("POST", "/repos/o/r/issues/5/comments")).To(ConsistOf(
				ContainSubstring("Pull request: http://github/pr/8")))
			Expect(h.out.String()).To(HavePrefix("http://github/pr/8\n"))

			Expect(h.gh.Bodies("POST", "/repos/o/r/issues/41/comments")).To(ConsistOf(
				ContainSubstring(`<!-- agent-retro:5:sha-head -->\n## Retro: #5\n\nlessons`)))
			retro := h.runner.find("--session")
			Expect(retro).To(HaveLen(1))
			Expect(retro[0].Timeout).To(beNear(RetroTimeout))
			h.expectLeakFree("repo-token", "or-key")
		})

		It("fails without new commits, publishing nothing", func() {
			h.runner.wrap(func(c proc.Cmd, base func(proc.Cmd) (string, error)) (string, error) {
				if strings.Contains(joined(c), "rev-list --count") {
					return "0\n", nil
				}
				return base(c)
			})
			code, err := run(workflow.Implementer)
			Expect(err).To(MatchError(ContainSubstring("no commits")))
			Expect(code).NotTo(BeZero())
			Expect(h.runner.find("noreply.github.com push")).To(BeEmpty())
			Expect(h.gh.Bodies("POST", "/repos/o/r/pulls")).To(BeEmpty())
		})

		It("logs a failed retro but does not fail the run", func() {
			h.runner.wrap(func(c proc.Cmd, base func(proc.Cmd) (string, error)) (string, error) {
				if strings.Contains(joined(c), "--session") {
					return "", errors.New("retro blew up")
				}
				return base(c)
			})
			Expect(run(workflow.Implementer)).To(BeZero())
			Expect(h.errb.String()).To(And(ContainSubstring("retro failed"), ContainSubstring("retro blew up")))
		})

		It("logs an empty retro and posts nothing", func() {
			h.opencodeWrites("--session", retroOut(), "  \n")
			Expect(run(workflow.Implementer)).To(BeZero())
			Expect(h.errb.String()).To(ContainSubstring("the retro wrote nothing"))
			Expect(h.gh.Bodies("POST", "/repos/o/r/issues/41/comments")).To(BeEmpty())
		})

		It("clears stale scratch from an earlier run", func() {
			Expect(os.MkdirAll(h.deps.Settings.WorkRoot, 0o700)).To(Succeed())
			Expect(os.WriteFile(retroOut(), []byte("stale retro"), 0o600)).To(Succeed())
			Expect(run(workflow.Implementer)).Error().NotTo(HaveOccurred())
			Expect(h.gh.Bodies("POST", "/repos/o/r/issues/41/comments")).To(BeEmpty(), "posted a stale retro")
		})
	})

	It("on a rework builds on the reviewed commits and fast-forwards the open PR", func() {
		h.implementerGH(map[string]any{"number": 8, "html_url": "http://github/pr/8"},
			[]workflow.Item{{Body: workflow.ReviewMarker("old") + "\nFIX THE THING"}})
		h.writeToken(workflow.Implementer, "repo-token")
		Expect(run(workflow.Implementer)).Error().NotTo(HaveOccurred())
		Expect(h.runner.find("fetch origin agent/issue-5")).To(HaveLen(1))
		Expect(h.runner.find("checkout -B agent/issue-5 origin/agent/issue-5")).To(HaveLen(1))
		oc := h.runner.find("opencode run --agent implementer")
		Expect(oc).NotTo(BeEmpty())
		Expect(joined(oc[0])).To(ContainSubstring("Review:\n"+workflow.ReviewMarker("old")+"\nFIX THE THING"), "feedback missing")
		push := h.runner.find("noreply.github.com push")
		Expect(push).To(HaveLen(1))
		Expect(joined(push[0])).NotTo(ContainSubstring("--force"), "rework must fast-forward")
		Expect(h.gh.Bodies("POST", "/repos/o/r/pulls")).To(BeEmpty(), "opened a second PR")
		Expect(h.gh.Bodies("POST", "/repos/o/r/issues/5/comments")).To(BeEmpty(), "commented a PR link again")
		Expect(h.out.String()).To(HavePrefix("http://github/pr/8\n"))
	})

	Describe("the Reviewer", func() {
		It("approves a clean review of the head commit", func() {
			h.reviewerGH(nil)
			h.opencodeWrites("code-review", h.reviewOut(), cleanReview)
			Expect(run(workflow.Reviewer)).To(Equal(workflow.ReviewClean))
			Expect(h.gh.Bodies("POST", "/repos/o/r/pulls/8/reviews")).To(ConsistOf(And(
				ContainSubstring(`"event":"APPROVE"`), ContainSubstring(`"commit_id":"sha-head"`),
				ContainSubstring("<!-- agent-review:sha-head -->"),
			)))
			work := filepath.Join(h.deps.Settings.WorkRoot, "review-5")
			Expect(h.runner.find("git clone https://github.com/o/r.git " + work)).To(HaveLen(1))
			Expect(h.runner.find("fetch origin agent/issue-5")).To(HaveLen(1))
			Expect(h.runner.find("checkout -B agent/issue-5 origin/agent/issue-5")).To(HaveLen(1))
			oc := h.runner.find("opencode run --agent reviewer --format json Use the `code-review` skill")
			Expect(oc).To(HaveLen(1))
			Expect(oc[0].Dir).To(Equal(work))
			Expect(oc[0].Timeout).To(beNear(ReviewTimeout))
			Expect(oc[0].Env).To(HaveKeyWithValue("OPENROUTER_API_KEY", "or-key"))
			spec, err := os.ReadFile(filepath.Join(h.deps.Settings.WorkRoot, "review-5.spec.md"))
			Expect(err).NotTo(HaveOccurred())
			Expect(string(spec)).To(Equal("# Add x\n\nDo x\n"))
			h.expectLeakFree("repo-token", "or-key")
		})

		It("sends findings back as a comment review", func() {
			h.reviewerGH(nil)
			h.opencodeWrites("code-review", h.reviewOut(), blockingReview)
			Expect(run(workflow.Reviewer)).To(Equal(workflow.ReviewFindings))
			Expect(h.gh.Bodies("POST", "/repos/o/r/pulls/8/reviews")).To(ConsistOf(And(
				ContainSubstring(`"event":"COMMENT"`), ContainSubstring("### High (1)"),
			)))
		})

		It("stops after the maximum number of rounds", func() {
			h.reviewerGH([]workflow.Item{{Body: workflow.ReviewMarker("s1")}, {Body: workflow.ReviewMarker("s2")}})
			h.opencodeWrites("code-review", h.reviewOut(), blockingReview)
			Expect(run(workflow.Reviewer)).To(Equal(workflow.ReviewExhausted))
		})

		It("posts nothing for a commit it already reviewed", func() {
			h.reviewerGH([]workflow.Item{{Body: workflow.ReviewMarker("sha-head") + "\nearlier"}})
			h.opencodeWrites("code-review", h.reviewOut(), blockingReview)
			Expect(run(workflow.Reviewer)).To(Equal(workflow.ReviewFindings))
			Expect(h.gh.Bodies("POST", "/repos/o/r/pulls/8/reviews")).To(BeEmpty(), "posted again")
		})

		It("fails closed on a malformed review, posting nothing", func() {
			h.reviewerGH(nil)
			h.opencodeWrites("code-review", h.reviewOut(), `{"summary":"ok"}`)
			code, err := run(workflow.Reviewer)
			Expect(err).To(HaveOccurred())
			Expect(code).NotTo(Equal(workflow.ReviewClean))
			Expect(h.gh.Bodies("POST", "/repos/o/r/pulls/8/reviews")).To(BeEmpty())
		})

		It("fails closed when the model wrote no review", func() {
			h.reviewerGH(nil)
			code, err := run(workflow.Reviewer)
			Expect(err).To(HaveOccurred())
			Expect(code).NotTo(Equal(workflow.ReviewClean))
		})

		It("fails without an open pull request", func() {
			h.gh.JSON("GET /repos/o/r/issues/5", map[string]any{"number": 5})
			h.gh.JSON("GET /repos/o/r/pulls", []map[string]any{})
			h.writeToken(workflow.Reviewer, "t")
			code, err := run(workflow.Reviewer)
			Expect(err).To(MatchError(ContainSubstring("no open pull request for agent/issue-5")))
			Expect(code).NotTo(BeZero())
		})
	})

	DescribeTable("refuses an agent role without behaviour, doing nothing",
		func(role workflow.Role) {
			code, err := run(role)
			Expect(err).To(HaveOccurred())
			Expect(code).NotTo(BeZero())
			h.expectNoWork()
		},
		Entry(nil, workflow.Planner), Entry(nil, workflow.Researcher), Entry(nil, workflow.Curator),
	)

	It("fails without a token file, never calling GitHub", func() {
		code, err := run(workflow.Implementer)
		Expect(err).To(HaveOccurred())
		Expect(code).NotTo(BeZero())
		Expect(h.gh.Calls()).To(BeEmpty())
	})
})
