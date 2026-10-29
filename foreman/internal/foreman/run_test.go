package foreman

import (
	"context"
	"errors"
	"fmt"
	"net"
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
		return Run(context.Background(), h.deps, role, "o/r", testAgent(), testRetro(), 5)
	}

	Describe("the Implementer", func() {
		BeforeEach(func() {
			h.implementerGH(nil, nil)
			h.writeToken(workflow.Implementer, "repo-token\n")
		})

		// retroOut is where the retro of issue 5 is written.
		retroOut := func() string { return filepath.Join(h.deps.Settings.WorkRoot, "issue-5.retro.md") }

		// implOut is where the Implementer's output is written.
		implOut := func() string { return h.implOut() }

		It("commits with hooks disabled, pushes, opens the PR and posts the retro", func() {
			h.implementerWrites(firstRunOutput)
			h.opencodeWrites("--session", retroOut(), "lessons\n")
			Expect(run(workflow.Implementer)).To(BeZero())

			pr, prURL := h.pullRequest()

			work := filepath.Join(h.deps.Settings.WorkRoot, "issue-5")
			Expect(h.runner.find("git clone https://github.com/o/r.git " + work)).To(HaveLen(1))
			Expect(h.runner.find("checkout -B agent/issue-5")).To(HaveLen(1))
			Expect(h.runner.find("fetch origin")).To(BeEmpty())

			oc := h.runner.find("opencode run --agent implementer --format json Use the `implement` skill")
			Expect(oc).To(HaveLen(1))
			Expect(oc[0].Dir).To(Equal(work))
			Expect(oc[0].Env).NotTo(HaveKey("OPENROUTER_API_KEY"), "the key reached the model")
			Expect(baseURL(oc[0])).To(MatchRegexp(`^http://127\.0\.0\.1:\d+/api/v1$`), "the model calls the proxy")
			Expect(joined(oc[0])).To(ContainSubstring("Write the JSON to " + implOut()))
			h.expectNoSecretInFiles(work, "or-key")
			Expect(oc[0].Timeout).To(beNear(testAgent().Deadline()))
			Expect(joined(oc[0])).NotTo(ContainSubstring("Review:"), "first run carries review feedback")

			Expect(h.runner.find("add -A")).To(HaveLen(1))
			commit := h.runner.find("commit -m Add x")
			Expect(commit).To(HaveLen(1))
			Expect(joined(commit[0])).To(ContainSubstring("core.hooksPath=/dev/null"), "hooks left enabled")

			push := h.runner.find("noreply.github.com push")
			Expect(push).To(HaveLen(1))
			Expect(joined(push[0])).To(HaveSuffix("push --force origin agent/issue-5"))
			Expect(push[0].Env).To(HaveKeyWithValue("GIT_CONFIG_VALUE_0", Not(BeEmpty())), "push without credentials in env")

			Expect(h.gh.Bodies("POST", "/repos/o/r/pulls")).To(ConsistOf(And(
				ContainSubstring(`"title":"Add x"`), ContainSubstring(`"head":"agent/issue-5"`),
				ContainSubstring(`"base":"main"`), ContainSubstring(`"body":"Closes #5"`),
			)))
			Expect(h.gh.Bodies("POST", "/repos/o/r/issues/5/comments")).To(ConsistOf(
				ContainSubstring("Pull request: " + prURL)))
			Expect(pr).NotTo(BeZero())
			Expect(h.out.String()).To(HavePrefix(prURL + "\n"))

			Expect(h.gh.Bodies("POST", h.retroPath())).To(ConsistOf(
				ContainSubstring(`<!-- agent-retro:5:sha-head -->\n## Retro: #5\n\nlessons`)))
			retro := h.runner.find("--session")
			Expect(retro).To(HaveLen(1))
			Expect(retro[0].Timeout).To(beNear(testRetro().Deadline()))
			h.expectLeakFree("repo-token", "or-key")
		})

		It("gives the model a read-only token of its own App, scoped to the repository", func() {
			h.implementerWrites(firstRunOutput)
			Expect(run(workflow.Implementer)).To(BeZero())

			oc := h.runner.find("opencode run --agent implementer")
			Expect(oc).NotTo(BeEmpty())
			Expect(oc[0].Env).To(HaveKeyWithValue("GH_TOKEN", "read-only-token-implementer"))

			mint := h.runner.find("app-token", " token ", "--read-only")
			Expect(mint).To(HaveLen(1))
			Expect(appOf(mint[0])).To(Equal(string(workflow.Implementer)))
			Expect(mint[0].Argv[len(mint[0].Argv)-3:]).To(Equal([]string{"--repo", "o/r", "--read-only"}))
			h.expectLeakFree("repo-token", "read-only-token", "or-key")
		})

		It("rejects a file with an unknown or labelled field, applying nothing", func() {
			h.implementerWrites(`{"version":1,"commit_message":"Add x",` +
				`"pr_title":"Add x","pr_body":"Closes #5","labels":["factory:done"]}`)

			code, err := run(workflow.Implementer)
			Expect(code).To(Equal(workflow.ExitReported))
			Expect(err).To(HaveOccurred())

			comments := h.gh.Bodies("POST", "/repos/o/r/issues/5/comments")
			Expect(comments).To(HaveLen(1), "the errors go in one comment")
			Expect(comments[0]).To(And(
				ContainSubstring("output file was rejected"), ContainSubstring(`unknown field \"labels\"`),
			))
			Expect(h.runner.find("commit -m")).To(BeEmpty(), "committed a rejected file")
			Expect(h.runner.find("noreply.github.com push")).To(BeEmpty(), "pushed a rejected file")
			Expect(h.gh.Bodies("POST", "/repos/o/r/pulls")).To(BeEmpty())
		})

		It("rejects a factory marker in the model's text", func() {
			h.implementerWrites(strings.Replace(firstRunOutput, "Add x", "Add x <!-- factory:handoff -->", 1))

			code, err := run(workflow.Implementer)
			Expect(code).To(Equal(workflow.ExitReported))
			Expect(err).To(HaveOccurred())
			Expect(h.gh.Bodies("POST", "/repos/o/r/issues/5/comments")).To(ConsistOf(
				ContainSubstring("carries a factory marker")))
			Expect(h.runner.find("noreply.github.com push")).To(BeEmpty())
		})

		It("rejects a run whose model wrote no output file", func() {
			code, err := run(workflow.Implementer)
			Expect(code).To(Equal(workflow.ExitReported))
			Expect(err).To(HaveOccurred())
			Expect(h.gh.Bodies("POST", "/repos/o/r/issues/5/comments")).To(ConsistOf(
				ContainSubstring("cannot be read")))
			Expect(h.runner.find("noreply.github.com push")).To(BeEmpty())
		})

		It("answers the review's findings on the pull request", func() {
			h.implementerGH(map[string]any{"number": 8, "html_url": "http://github/pr/8"},
				[]workflow.Item{{Body: workflow.ReviewMarker("old") + "\nFIX THE THING"}})
			h.implementerWrites(reworkOutput)

			Expect(run(workflow.Implementer)).Error().NotTo(HaveOccurred())

			pr, _ := h.pullRequest()
			quotes := h.gh.Bodies("POST", fmt.Sprintf("/repos/o/r/issues/%d/comments", pr))
			Expect(quotes).To(ConsistOf(And(
				ContainSubstring(workflow.RepliesMarker("sha-head")),
				ContainSubstring("- **fixed** `bug`: fixed it"),
			)))
		})

		It("changes nothing when the same output is applied twice", func() {
			h.implementerGH(map[string]any{"number": 8, "html_url": "http://github/pr/8"},
				[]workflow.Item{{Body: workflow.ReviewMarker("old") + "\nFIX THE THING"}})
			h.implementerWrites(reworkOutput)

			Expect(run(workflow.Implementer)).Error().NotTo(HaveOccurred())
			Expect(run(workflow.Implementer)).Error().NotTo(HaveOccurred())

			pr, _ := h.pullRequest()
			quotes := h.gh.Bodies("POST", fmt.Sprintf("/repos/o/r/issues/%d/comments", pr))
			Expect(quotes).To(HaveLen(1), "posted the replies twice")
			Expect(h.gh.Summary()).To(ContainElement(
				ContainSubstring("PATCH /repos/o/r/issues/comments/")), "did not update the replies")
			Expect(h.gh.Bodies("POST", "/repos/o/r/pulls")).To(BeEmpty(), "opened a second pull request")
		})

		It("fails without new commits, publishing nothing", func() {
			h.implementerWrites(firstRunOutput)
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
			h.implementerWrites(firstRunOutput)
			h.runner.wrap(func(c proc.Cmd, base func(proc.Cmd) (string, error)) (string, error) {
				if strings.Contains(joined(c), "--session") {
					return "", errors.New("retro blew up")
				}

				return base(c)
			})
			Expect(run(workflow.Implementer)).To(BeZero())
			Expect(h.logs.String()).To(And(ContainSubstring("retro failed"), ContainSubstring("retro blew up")))
		})

		It("logs an empty retro and posts nothing", func() {
			h.implementerWrites(firstRunOutput)
			h.opencodeWrites("--session", retroOut(), "  \n")
			Expect(run(workflow.Implementer)).To(BeZero())
			Expect(h.logs.String()).To(ContainSubstring("the retro wrote nothing"))
			Expect(h.retroNumber()).To(BeZero(), "created a retro ticket")
		})

		It("fails closed when the proxy cannot bind", func() {
			taken, err := net.Listen("tcp", "127.0.0.1:0")
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(taken.Close)

			h.deps.ProxyAddr = taken.Addr().String()

			code, err := run(workflow.Implementer)
			Expect(err).To(MatchError(ContainSubstring("bind")))
			Expect(code).NotTo(BeZero())
			Expect(h.opencodeCalls()).To(BeEmpty(), "the model ran without its proxy")
			Expect(h.gh.Calls()).To(BeEmpty(), "the run touched GitHub after failing closed")
		})

		It("clears stale scratch from an earlier run", func() {
			h.implementerWrites(firstRunOutput)
			Expect(os.MkdirAll(h.deps.Settings.WorkRoot, 0o700)).To(Succeed())
			Expect(os.WriteFile(retroOut(), []byte("stale retro"), 0o600)).To(Succeed())
			Expect(run(workflow.Implementer)).Error().NotTo(HaveOccurred())
			Expect(h.retroNumber()).To(BeZero(), "posted a stale retro")
		})
	})

	It("on a rework builds on the reviewed commits and fast-forwards the open PR", func() {
		h.implementerGH(map[string]any{"number": 8, "html_url": "http://github/pr/8"},
			[]workflow.Item{{Body: workflow.ReviewMarker("old") + "\nFIX THE THING"}})
		h.writeToken(workflow.Implementer, "repo-token")
		h.implementerWrites(reworkOutput)
		Expect(run(workflow.Implementer)).Error().NotTo(HaveOccurred())

		_, prURL := h.pullRequest()
		Expect(h.runner.find("fetch origin agent/issue-5")).To(HaveLen(1))
		Expect(h.runner.find("checkout -B agent/issue-5 origin/agent/issue-5")).To(HaveLen(1))
		oc := h.runner.find("opencode run --agent implementer")
		Expect(oc).NotTo(BeEmpty())
		Expect(joined(oc[0])).To(
			ContainSubstring("Review:\n"+workflow.ReviewMarker("old")+"\nFIX THE THING"), "feedback missing")
		Expect(joined(oc[0])).To(ContainSubstring(`"replies"`), "the review round was not asked to answer")

		push := h.runner.find("noreply.github.com push")
		Expect(push).To(HaveLen(1))
		Expect(joined(push[0])).NotTo(ContainSubstring("--force"), "rework must fast-forward")
		Expect(h.gh.Bodies("POST", "/repos/o/r/pulls")).To(BeEmpty(), "opened a second PR")
		Expect(h.gh.Bodies("POST", "/repos/o/r/issues/5/comments")).To(BeEmpty(), "commented a PR link again")
		Expect(h.out.String()).To(HavePrefix(prURL + "\n"))
	})

	Describe("the Reviewer", func() {
		It("approves a clean review of the head commit and asks a human to merge", func() {
			h.reviewerGH(nil)
			h.opencodeWrites("code-review", h.reviewOut(), cleanReview)
			Expect(run(workflow.Reviewer)).To(Equal(workflow.ReviewClean))
			Expect(h.gh.Bodies("POST", h.reviewPath())).To(ConsistOf(And(
				ContainSubstring(`"event":"APPROVE"`), ContainSubstring(`"commit_id":"sha-head"`),
				ContainSubstring("<!-- agent-review:sha-head -->"),
			)))

			pr, _ := h.pullRequest()
			Expect(h.gh.Bodies("POST", fmt.Sprintf("/repos/o/r/pulls/%d/requested_reviewers", pr))).To(
				ConsistOf(ContainSubstring(`"reviewers":["o"]`)), "no human was asked to merge")

			work := filepath.Join(h.deps.Settings.WorkRoot, "review-5")
			Expect(h.runner.find("git clone https://github.com/o/r.git " + work)).To(HaveLen(1))
			Expect(h.runner.find("fetch origin agent/issue-5")).To(HaveLen(1))
			Expect(h.runner.find("checkout -B agent/issue-5 origin/agent/issue-5")).To(HaveLen(1))
			oc := h.runner.find("opencode run --agent reviewer --format json Use the `code-review` skill")
			Expect(oc).To(HaveLen(1))
			Expect(oc[0].Dir).To(Equal(work))
			Expect(oc[0].Timeout).To(beNear(testAgent().Deadline()))
			Expect(oc[0].Env).NotTo(HaveKey("OPENROUTER_API_KEY"), "the key reached the model")
			Expect(oc[0].Env).To(HaveKeyWithValue("GH_TOKEN", "read-only-token-reviewer"))
			Expect(baseURL(oc[0])).To(MatchRegexp(`^http://127\.0\.0\.1:\d+/api/v1$`), "the model calls the proxy")
			h.expectNoSecretInFiles(work, "or-key")

			spec, err := os.ReadFile(filepath.Join(h.deps.Settings.WorkRoot, "reviewer-5", "issue.md"))
			Expect(err).NotTo(HaveOccurred())
			Expect(string(spec)).To(Equal("# Add x\n\nDo x\n"))
			h.expectLeakFree("repo-token", "read-only-token", "or-key")
		})

		It("requests changes as a native review of the head commit", func() {
			h.reviewerGH(nil)
			h.opencodeWrites("code-review", h.reviewOut(), blockingReview)
			Expect(run(workflow.Reviewer)).To(Equal(workflow.ReviewFindings))
			Expect(h.gh.Bodies("POST", h.reviewPath())).To(ConsistOf(And(
				ContainSubstring(`"event":"REQUEST_CHANGES"`), ContainSubstring("### High (1)"),
			)))
		})

		It("rejects a high finding without REQUEST_CHANGES, posting no review", func() {
			h.reviewerGH(nil)
			h.opencodeWrites("code-review", h.reviewOut(),
				`{"version":1,"verdict":"APPROVE","summary":"s","findings":[`+
					`{"severity":"high","path":"a.go","claim":"bug"}]}`)

			code, err := run(workflow.Reviewer)
			Expect(code).To(Equal(workflow.ExitReported))
			Expect(err).To(HaveOccurred())
			Expect(h.gh.Bodies("POST", h.reviewPath())).To(BeEmpty())
			Expect(h.gh.Bodies("POST", "/repos/o/r/issues/5/comments")).To(ConsistOf(
				ContainSubstring("a high or medium finding needs REQUEST_CHANGES")))
		})

		It("rejects REQUEST_CHANGES without findings", func() {
			h.reviewerGH(nil)
			h.opencodeWrites("code-review", h.reviewOut(),
				`{"version":1,"verdict":"REQUEST_CHANGES","summary":"s","findings":[]}`)

			code, err := run(workflow.Reviewer)
			Expect(code).To(Equal(workflow.ExitReported))
			Expect(err).To(HaveOccurred())
			Expect(h.gh.Bodies("POST", h.reviewPath())).To(BeEmpty())
			Expect(h.gh.Bodies("POST", "/repos/o/r/issues/5/comments")).To(ConsistOf(
				ContainSubstring("REQUEST_CHANGES needs at least one finding")))
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
			Expect(h.gh.Bodies("POST", h.reviewPath())).To(BeEmpty(), "posted again")
		})

		It("fails closed on a malformed review, posting nothing", func() {
			h.reviewerGH(nil)
			h.opencodeWrites("code-review", h.reviewOut(), `{"summary":"ok"}`)

			code, err := run(workflow.Reviewer)
			Expect(err).To(HaveOccurred())
			Expect(code).NotTo(Equal(workflow.ReviewClean))
			Expect(h.gh.Bodies("POST", h.reviewPath())).To(BeEmpty())
		})

		It("fails closed when the model wrote no review", func() {
			h.reviewerGH(nil)

			code, err := run(workflow.Reviewer)
			Expect(err).To(HaveOccurred())
			Expect(code).NotTo(Equal(workflow.ReviewClean))
		})

		It("refuses a review file that links out of the scratch directory", func() {
			h.reviewerGH(nil)

			outside := filepath.Join(h.dir, "outside.json")
			Expect(os.WriteFile(outside, []byte(cleanReview), 0o600)).To(Succeed())
			h.runner.wrap(func(c proc.Cmd, base func(proc.Cmd) (string, error)) (string, error) {
				if c.Argv[0] == "opencode" {
					Expect(os.MkdirAll(filepath.Dir(h.reviewOut()), 0o700)).To(Succeed())
					Expect(os.Symlink(outside, h.reviewOut())).To(Succeed())
				}

				return base(c)
			})

			code, err := run(workflow.Reviewer)
			Expect(err).To(MatchError(ContainSubstring("cannot be read")))
			Expect(code).NotTo(Equal(workflow.ReviewClean))
			Expect(h.gh.Bodies("POST", h.reviewPath())).To(BeEmpty())
		})

		It("fails without an open pull request", func() {
			h.ensureIssue(5)
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
