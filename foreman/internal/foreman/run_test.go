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

	"github.com/nateinaction/factory/foreman/internal/config"
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

		It("on a first run clones, runs the model, force-pushes, opens the PR and posts the retro", func() {
			h.opencodeWrites("--session", h.retroOut(workflow.Implementer), validRetro)
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
			h.expectNoSecretInFiles(work, "or-key")
			Expect(oc[0].Timeout).To(beNear(testAgent().Deadline()))
			Expect(joined(oc[0])).NotTo(ContainSubstring("Review:"), "first run carries review feedback")

			push := h.runner.find("noreply.github.com push")
			Expect(push).To(HaveLen(1))
			Expect(joined(push[0])).To(HaveSuffix("push --force origin agent/issue-5"))
			Expect(push[0].Env).To(HaveKeyWithValue("GIT_CONFIG_VALUE_0", Not(BeEmpty())), "push without credentials in env")

			Expect(h.gh.Bodies("POST", "/repos/o/r/pulls")).To(ConsistOf(And(
				ContainSubstring(`"title":"Add x"`), ContainSubstring(`"head":"agent/issue-5"`),
				ContainSubstring(`"base":"main"`), ContainSubstring(`Closes #5\n\nOpened by the factory agent.`),
			)))
			Expect(h.gh.Bodies("POST", "/repos/o/r/issues/5/comments")).To(ContainElement(
				ContainSubstring("Pull request: " + prURL)))
			Expect(pr).NotTo(BeZero())
			Expect(h.out.String()).To(HavePrefix(prURL + "\n"))

			Expect(h.retroComments()).To(ConsistOf(And(
				HavePrefix("<!-- factory:retro issue=5 role=implementer key=sha-head -->"),
				ContainSubstring("`skill:implement` / `guide-misses-tests`"),
				ContainSubstring(`"target":"skill:implement"`),
			)))
			retro := h.runner.find("--session")
			Expect(retro).To(HaveLen(1))
			Expect(retro[0].Timeout).To(beNear(testRetro().Deadline()))
			h.expectLeakFree("repo-token", "or-key")
		})

		It("keys the retro by the commit the run produced, not the one it started from", func() {
			heads := []string{"sha-start\n", "sha-new\n"}

			h.runner.wrap(func(c proc.Cmd, base func(proc.Cmd) (string, error)) (string, error) {
				if strings.Contains(joined(c), "rev-parse HEAD") {
					out := heads[0]
					if len(heads) > 1 {
						heads = heads[1:]
					}

					return out, nil
				}

				return base(c)
			})
			h.opencodeWrites("--session", h.retroOut(workflow.Implementer), validRetro)

			Expect(run(workflow.Implementer)).To(BeZero())
			Expect(h.retroComments()).To(ConsistOf(
				ContainSubstring("<!-- factory:retro issue=5 role=implementer key=sha-new -->")))
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
			Expect(h.logs.String()).To(And(ContainSubstring("retro failed"), ContainSubstring("retro blew up")))
		})

		It("logs an invalid retro.json and posts nothing", func() {
			h.opencodeWrites("--session", h.retroOut(workflow.Implementer), `{"version":1,"findings":[{"target":"nope"}]}`)
			Expect(run(workflow.Implementer)).To(BeZero())
			Expect(h.logs.String()).To(And(
				ContainSubstring("retro failed"), ContainSubstring("retro target")))
			Expect(h.retroComments()).To(BeEmpty(), "posted an invalid retro")
		})

		It("gives a run that failed at the model its retro", func() {
			h.answerOpencode(errStream("APIError", 500))
			h.opencodeWrites("--session", h.retroOut(workflow.Implementer), validRetro)

			code, err := run(workflow.Implementer)
			Expect(err).To(HaveOccurred())
			Expect(code).NotTo(BeZero())
			Expect(h.retroComments()).To(ConsistOf(
				ContainSubstring("<!-- factory:retro issue=5 role=implementer key=sha-head -->")))
		})

		DescribeTable("skips the retro when the failure leaves nothing to resume",
			func(name string, status int) {
				h.answerOpencode(errStream(name, status))

				code, err := run(workflow.Implementer)
				Expect(err).To(HaveOccurred())
				Expect(code).NotTo(BeZero())
				Expect(h.opencodeCalls()).To(HaveLen(1), "ran a retro it should have skipped")
				Expect(h.retroComments()).To(BeEmpty())
			},
			Entry("context overflow", "ContextOverflowError", 0),
			Entry("out of credits", "APIError", 402),
			Entry("unknown model", "APIError", 404),
		)

		It("drops a retro over its own step cap and leaves the run's outcome alone", func() {
			h.answerOpencode(okStream("ses_1"), stepsStream(testRetro().Steps))
			Expect(run(workflow.Implementer)).To(BeZero())
			Expect(h.logs.String()).To(And(
				ContainSubstring("retro failed"), ContainSubstring("step cap")))
			Expect(h.retroComments()).To(BeEmpty())
		})

		It("drops a retro over its own deadline and leaves the run's outcome alone", func() {
			short := config.Budget{Steps: testRetro().Steps}

			code, err := Run(context.Background(), h.deps, workflow.Implementer, "o/r", testAgent(), short, 5)
			Expect(err).NotTo(HaveOccurred())
			Expect(code).To(BeZero())
			Expect(h.logs.String()).To(And(
				ContainSubstring("retro failed"), ContainSubstring("deadline")))
			Expect(h.retroComments()).To(BeEmpty())
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
			Expect(os.MkdirAll(h.deps.Settings.WorkRoot, 0o700)).To(Succeed())
			Expect(os.WriteFile(h.retroOut(workflow.Implementer), []byte(validRetro), 0o600)).To(Succeed())
			Expect(run(workflow.Implementer)).Error().NotTo(HaveOccurred())
			Expect(h.retroComments()).To(BeEmpty(), "posted a stale retro")
		})
	})

	It("on a rework builds on the reviewed commits and fast-forwards the open PR", func() {
		h.implementerGH(map[string]any{"number": 8, "html_url": "http://github/pr/8"},
			[]workflow.Item{{Body: workflow.ReviewMarker("old") + "\nFIX THE THING"}})
		h.writeToken(workflow.Implementer, "repo-token")
		Expect(run(workflow.Implementer)).Error().NotTo(HaveOccurred())

		_, prURL := h.pullRequest()
		Expect(h.runner.find("fetch origin agent/issue-5")).To(HaveLen(1))
		Expect(h.runner.find("checkout -B agent/issue-5 origin/agent/issue-5")).To(HaveLen(1))
		oc := h.runner.find("opencode run --agent implementer")
		Expect(oc).NotTo(BeEmpty())
		Expect(joined(oc[0])).To(
			ContainSubstring("Review:\n"+workflow.ReviewMarker("old")+"\nFIX THE THING"), "feedback missing")

		push := h.runner.find("noreply.github.com push")
		Expect(push).To(HaveLen(1))
		Expect(joined(push[0])).NotTo(ContainSubstring("--force"), "rework must fast-forward")
		Expect(h.gh.Bodies("POST", "/repos/o/r/pulls")).To(BeEmpty(), "opened a second PR")

		for _, comment := range h.gh.Bodies("POST", "/repos/o/r/issues/5/comments") {
			Expect(comment).NotTo(ContainSubstring("Pull request:"), "commented a PR link again")
		}

		Expect(h.out.String()).To(HavePrefix(prURL + "\n"))
	})

	Describe("the Reviewer", func() {
		It("approves a clean review of the head commit, then posts a retro keyed by its head SHA", func() {
			h.reviewerGH(nil)
			h.opencodeWrites("code-review", h.reviewOut(), cleanReview)
			h.opencodeWrites("--session", h.retroOut(workflow.Reviewer), validRetro)
			Expect(run(workflow.Reviewer)).To(Equal(workflow.ReviewClean))
			Expect(h.gh.Bodies("POST", h.reviewPath())).To(ConsistOf(And(
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
			Expect(oc[0].Timeout).To(beNear(testAgent().Deadline()))
			Expect(oc[0].Env).NotTo(HaveKey("OPENROUTER_API_KEY"), "the key reached the model")
			Expect(baseURL(oc[0])).To(MatchRegexp(`^http://127\.0\.0\.1:\d+/api/v1$`), "the model calls the proxy")
			h.expectNoSecretInFiles(work, "or-key")

			spec, err := os.ReadFile(filepath.Join(h.deps.Settings.WorkRoot, "review-5.spec.md"))
			Expect(err).NotTo(HaveOccurred())
			Expect(string(spec)).To(Equal("# Add x\n\nDo x\n"))

			Expect(h.retroComments()).To(ConsistOf(
				ContainSubstring("<!-- factory:retro issue=5 role=reviewer key=sha-head -->")))
			h.expectLeakFree("repo-token", "or-key")
		})

		It("keys the retro by the PR head SHA it reviewed", func() {
			h.reviewerGH(nil)
			h.runner.wrap(func(c proc.Cmd, base func(proc.Cmd) (string, error)) (string, error) {
				if strings.Contains(joined(c), "rev-parse HEAD") {
					return "sha-reviewed\n", nil
				}

				return base(c)
			})
			h.opencodeWrites("code-review", h.reviewOut(), cleanReview)
			h.opencodeWrites("--session", h.retroOut(workflow.Reviewer), validRetro)

			Expect(run(workflow.Reviewer)).To(Equal(workflow.ReviewClean))
			Expect(h.retroComments()).To(ConsistOf(
				ContainSubstring("<!-- factory:retro issue=5 role=reviewer key=sha-reviewed -->")))
		})

		It("never shows the Reviewer the Implementer's retro, but does show its own groups", func() {
			h.reviewerGH(nil)
			h.seedComment(5, retroBody(workflow.Implementer, "commit-1", workflow.RetroFinding{
				Target: workflow.RetroTarget{Kind: workflow.TargetSkill, Name: "implement"},
				Group:  "guide-misses-tests", Severity: workflow.SeverityLow,
				Claim: "IMPL CLAIM", Evidence: "IMPL EVIDENCE",
			}))
			h.seedComment(5, retroBody(workflow.Reviewer, "older-head", workflow.RetroFinding{
				Target: workflow.RetroTarget{Kind: workflow.TargetFactory, Name: "retro"},
				Group:  "slow-retro", Severity: workflow.SeverityLow,
				Claim: "review retros run long", Evidence: "REVIEW EVIDENCE",
			}))

			h.opencodeWrites("code-review", h.reviewOut(), cleanReview)
			h.opencodeWrites("--session", h.retroOut(workflow.Reviewer), validRetro)
			Expect(run(workflow.Reviewer)).To(Equal(workflow.ReviewClean))

			for _, c := range h.opencodeCalls() {
				prompt := joined(c)
				Expect(prompt).NotTo(ContainSubstring("IMPL EVIDENCE"), "the Implementer's evidence leaked")
				Expect(prompt).NotTo(ContainSubstring("IMPL CLAIM"), "the Implementer's claim leaked")
				Expect(prompt).NotTo(ContainSubstring("role=implementer"), "the Implementer's retro leaked")
			}

			retro := h.runner.find("--session")
			Expect(retro).To(HaveLen(1))
			Expect(joined(retro[0])).To(And(
				ContainSubstring("`slow-retro`: review retros run long"),
				ContainSubstring("findings may target the Implementer's skills"),
			))
			Expect(joined(retro[0])).NotTo(ContainSubstring("REVIEW EVIDENCE"), "another retro's evidence leaked")
		})

		It("posts a retro whose findings target the Implementer's skills", func() {
			h.reviewerGH(nil)
			h.opencodeWrites("code-review", h.reviewOut(), cleanReview)
			h.opencodeWrites("--session", h.retroOut(workflow.Reviewer), validRetro)
			Expect(run(workflow.Reviewer)).To(Equal(workflow.ReviewClean))
			Expect(h.retroComments()).To(ConsistOf(
				ContainSubstring(`"target":"skill:implement"`)))
		})

		It("updates the retro comment for a run key rather than adding another", func() {
			h.reviewerGH(nil)
			existing := h.seedComment(5, retroBody(workflow.Reviewer, "sha-head", workflow.RetroFinding{
				Target: workflow.RetroTarget{Kind: workflow.TargetSkill, Name: "implement"},
				Group:  "guide-misses-tests", Severity: workflow.SeverityLow,
				Claim: "an earlier wording", Evidence: "e",
			}))

			h.opencodeWrites("code-review", h.reviewOut(), cleanReview)
			h.opencodeWrites("--session", h.retroOut(workflow.Reviewer), validRetro)
			Expect(run(workflow.Reviewer)).To(Equal(workflow.ReviewClean))

			Expect(h.gh.Bodies("PATCH", fmt.Sprintf("/repos/o/r/issues/comments/%d", existing))).To(ConsistOf(
				ContainSubstring("<!-- factory:retro issue=5 role=reviewer key=sha-head -->")))
			Expect(h.retroComments()).To(BeEmpty(), "added a second retro comment")
		})

		It("sends findings back as a comment review", func() {
			h.reviewerGH(nil)
			h.opencodeWrites("code-review", h.reviewOut(), blockingReview)
			Expect(run(workflow.Reviewer)).To(Equal(workflow.ReviewFindings))
			Expect(h.gh.Bodies("POST", h.reviewPath())).To(ConsistOf(And(
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
				if c.Argv[0] == "opencode" && strings.Contains(joined(c), "code-review") {
					Expect(os.Symlink(outside, h.reviewOut())).To(Succeed())
				}

				return base(c)
			})

			code, err := run(workflow.Reviewer)
			Expect(err).To(MatchError(ContainSubstring("read review")))
			Expect(code).NotTo(Equal(workflow.ReviewClean))
			Expect(h.gh.Bodies("POST", h.reviewPath())).To(BeEmpty())
		})

		It("fails without an open pull request", func() {
			h.ensureIssue("o/r", 5)
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
