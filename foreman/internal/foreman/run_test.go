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

	// run runs the agent role on issue 5 of o/r.
	run := func(role workflow.Role) (int, error) {
		return Run(context.Background(), h.deps, role, testRef(5), testAgent(), testRetro(), 3)
	}

	Describe("the Implementer", func() {
		BeforeEach(func() {
			h.implementerGH(nil, nil)
			h.writeToken(workflow.Implementer, "repo-token\n")
		})

		It("commits with hooks disabled, pushes, opens the PR and posts the retro", func() {
			h.implementerWrites(firstRunOutput)
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
			Expect(joined(oc[0])).To(ContainSubstring("Write the JSON to " + h.implOut()))
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

		It("shows the retro the groups other issues reported on the same target", func() {
			h.implementerWrites(firstRunOutput)
			h.opencodeWrites("--session", h.retroOut(workflow.Implementer), validRetro)

			earlier := h.createAs("o", nil, "An earlier issue")

			body, err := workflow.RenderRetro(
				workflow.RetroMarker(earlier, workflow.Implementer, "sha-earlier"),
				[]workflow.RetroFinding{{
					Target:   workflow.RetroTarget{Kind: workflow.TargetSkill, Name: "implement"},
					Group:    "guide-misses-tests",
					Severity: workflow.SeverityLow,
					Claim:    "the guide never mentions tests",
					Evidence: "an earlier issue's evidence",
				}})
			Expect(err).NotTo(HaveOccurred())
			h.comment(earlier, appLogin(workflow.Implementer), body)

			Expect(run(workflow.Implementer)).To(BeZero())

			retro := h.runner.find("--session")
			Expect(retro).To(HaveLen(1))
			Expect(joined(retro[0])).To(ContainSubstring("`guide-misses-tests`: the guide never mentions tests"))
			Expect(joined(retro[0])).NotTo(ContainSubstring("an earlier issue's evidence"),
				"the retro read another retro's evidence")
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
			h.implementerWrites(firstRunOutput)
			h.opencodeWrites("--session", h.retroOut(workflow.Implementer), validRetro)

			Expect(run(workflow.Implementer)).To(BeZero())
			Expect(h.retroComments()).To(ConsistOf(
				ContainSubstring("<!-- factory:retro issue=5 role=implementer key=sha-new -->")))
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

		It("logs an invalid retro.json and posts nothing", func() {
			h.implementerWrites(firstRunOutput)
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
			h.implementerWrites(firstRunOutput)
			Expect(run(workflow.Implementer)).To(BeZero())
			Expect(h.logs.String()).To(And(
				ContainSubstring("retro failed"), ContainSubstring("step cap")))
			Expect(h.retroComments()).To(BeEmpty())
		})

		It("drops a retro over its own deadline and leaves the run's outcome alone", func() {
			h.implementerWrites(firstRunOutput)

			short := config.Budget{Steps: testRetro().Steps}

			code, err := Run(context.Background(), h.deps, workflow.Implementer, testRef(5), testAgent(), short, 3)
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
			h.implementerWrites(firstRunOutput)
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

		for _, comment := range h.gh.Bodies("POST", "/repos/o/r/issues/5/comments") {
			Expect(comment).NotTo(ContainSubstring("Pull request:"), "commented a PR link again")
		}

		Expect(h.out.String()).To(HavePrefix(prURL + "\n"))
	})

	Describe("the Reviewer", func() {
		It("approves a clean review of the head commit, posts its retro and asks a human to merge", func() {
			h.reviewerGH(nil)
			h.opencodeWrites("code-review", h.reviewOut(), cleanReview)
			h.opencodeWrites("--session", h.retroOut(workflow.Reviewer), validRetro)

			var spec []byte

			h.runner.wrap(func(c proc.Cmd, base func(proc.Cmd) (string, error)) (string, error) {
				if isModel(c) {
					var err error

					spec, err = os.ReadFile(filepath.Join(h.deps.Settings.WorkRoot, "reviewer-5", "issue.md"))
					Expect(err).NotTo(HaveOccurred())
				}

				return base(c)
			})
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

			Expect(string(spec)).To(Equal("# Add x\n\nDo x\n"))

			Expect(h.retroComments()).To(ConsistOf(
				ContainSubstring("<!-- factory:retro issue=5 role=reviewer key=sha-head -->")))
			h.expectLeakFree("repo-token", "read-only-token", "or-key")
		})

		It("reads the spec a single proposal stored in its final-round comment", func() {
			h.reviewerGH(nil)

			block, err := workflow.ProposalBlock(workflow.PlannerProposal{
				Shape: workflow.ShapeSingle,
				Spec:  "## Plan\n\nDo the thing.\n\n## Domain changes\n\nNone.\n",
			})
			Expect(err).NotTo(HaveOccurred())

			h.comment(5, appLogin(workflow.Planner),
				"The plan.\n\nIs this ready to build?\n\n"+block)
			h.opencodeWrites("code-review", h.reviewOut(), cleanReview)
			h.opencodeWrites("--session", h.retroOut(workflow.Reviewer), validRetro)

			var spec []byte

			h.runner.wrap(func(c proc.Cmd, base func(proc.Cmd) (string, error)) (string, error) {
				if isModel(c) {
					var readErr error

					spec, readErr = os.ReadFile(filepath.Join(h.deps.Settings.WorkRoot, "reviewer-5", "issue.md"))
					Expect(readErr).NotTo(HaveOccurred())
				}

				return base(c)
			})
			Expect(run(workflow.Reviewer)).To(Equal(workflow.ReviewClean))
			Expect(string(spec)).To(ContainSubstring("## Plan\n\nDo the thing.\n\n## Domain changes\n\nNone."))
		})

		It("asks the latest human writer to merge when the issue was authored by a bot", func() {
			h.reviewerGH(nil)
			h.comment(5, "o", "Let's take this one")
			pr, _ := h.pullRequest()

			code, err := postVerdict(context.Background(), h.deps, h.deps.client("minted-token-reviewer"),
				testRepo("o/r"), pr, "sha-head", workflow.ReviewerOutput{
					Version: 1, Verdict: workflow.Approve, Summary: "fine",
				}, workflow.Issue{Number: 5, Author: "factory[bot]"}, 3)
			Expect(err).NotTo(HaveOccurred())
			Expect(code).To(Equal(workflow.ReviewClean))
			Expect(h.gh.Bodies("POST", fmt.Sprintf("/repos/o/r/pulls/%d/requested_reviewers", pr))).To(
				ConsistOf(ContainSubstring(`"reviewers":["o"]`)), "a bot author asked nobody to merge")
		})

		It("skips the review request with a log when no human writer exists", func() {
			h.reviewerGH(nil)
			pr, _ := h.pullRequest()

			code, err := postVerdict(context.Background(), h.deps, h.deps.client("minted-token-reviewer"),
				testRepo("o/r"), pr, "sha-head", workflow.ReviewerOutput{
					Version: 1, Verdict: workflow.Approve, Summary: "fine",
				}, workflow.Issue{Number: 5, Author: "factory[bot]"}, 3)
			Expect(err).NotTo(HaveOccurred())
			Expect(code).To(Equal(workflow.ReviewClean))
			Expect(h.gh.Bodies("POST", fmt.Sprintf("/repos/o/r/pulls/%d/requested_reviewers", pr))).To(BeEmpty())
			Expect(h.logs.String()).To(ContainSubstring("no human writer to ask for an approval review"))
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
			h.seedComment(retroBody(workflow.Implementer, "commit-1", workflow.RetroFinding{
				Target: workflow.RetroTarget{Kind: workflow.TargetSkill, Name: "implement"},
				Group:  "guide-misses-tests", Severity: workflow.SeverityLow,
				Claim: "IMPL CLAIM", Evidence: "IMPL EVIDENCE",
			}))
			h.seedComment(retroBody(workflow.Reviewer, "older-head", workflow.RetroFinding{
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
			existing := h.seedComment(retroBody(workflow.Reviewer, "sha-head", workflow.RetroFinding{
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
				if isModel(c) && strings.Contains(joined(c), "code-review") {
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

	Describe("the Researcher", func() {
		BeforeEach(func() {
			h.installed[workflow.Researcher] = []string{"o/r"}
			h.writeToken(workflow.Researcher, "repo-token\n")
			h.researchTicket()
		})

		It("commits the findings, pushes research/<name>, posts the resolution and appends the decision", func() {
			h.researcherWrites(researcherRun)
			h.opencodeWrites("--session", h.retroOut(workflow.Researcher), validRetro)

			Expect(run(workflow.Researcher)).To(BeZero())

			work := filepath.Join(h.deps.Settings.WorkRoot, "research-5")
			Expect(h.runner.find("git clone https://github.com/o/r.git " + work)).To(HaveLen(1))
			Expect(h.runner.find("checkout -B research/emulate")).To(HaveLen(1))
			Expect(h.runner.find("fetch origin research/emulate")).To(BeEmpty())

			oc := h.runner.find("opencode run --agent researcher --format json Use the `research` skill")
			Expect(oc).To(HaveLen(1))
			Expect(oc[0].Dir).To(Equal(work))
			Expect(oc[0].Env).To(HaveKeyWithValue("GH_TOKEN", "read-only-token-researcher"))
			Expect(oc[0].Env).NotTo(HaveKey("OPENROUTER_API_KEY"), "the key reached the model")
			Expect(joined(oc[0])).To(ContainSubstring("Write the JSON to " + h.researchOut()))
			Expect(oc[0].Timeout).To(beNear(testAgent().Deadline()))

			Expect(h.runner.find("add -A")).To(HaveLen(1))
			Expect(joined(h.runner.find("commit -m")[0])).To(ContainSubstring("core.hooksPath=/dev/null"))

			push := h.runner.find("noreply.github.com push")
			Expect(push).To(HaveLen(1))
			Expect(joined(push[0])).To(HaveSuffix("push --force origin research/emulate"))
			Expect(push[0].Env).To(HaveKeyWithValue("GIT_CONFIG_VALUE_0", Not(BeEmpty())))

			Expect(h.gh.Bodies("POST", "/repos/o/r/issues/5/comments")).To(ContainElement(And(
				ContainSubstring(workflow.ResearchMarker("research/emulate")),
				ContainSubstring("Findings are in the file."),
				ContainSubstring("https://github.com/o/r/tree/research/emulate"),
			)))
			Expect(h.gh.Bodies("POST", "/repos/o/r/pulls")).To(BeEmpty(), "opened a pull request")
			Expect(h.gh.Bodies("PATCH", "/repos/o/r/issues/6")).To(ConsistOf(
				ContainSubstring(`- [Add x](https://github.com/o/r/issues/5): Use emulate.`)),
				"the map did not gain the decision line")
			Expect(h.bodyOf(6)).To(ContainSubstring(
				"## Decisions so far\n\n- [Add x](https://github.com/o/r/issues/5): Use emulate.\n\n## Not yet"))

			Expect(h.retroComments()).To(ConsistOf(
				ContainSubstring("<!-- factory:retro issue=5 role=researcher key=sha-head -->")))
			h.expectLeakFree("repo-token", "read-only-token", "or-key")
		})

		It("runs the retro after the push, keyed by the branch's commit", func() {
			h.researcherWrites(researcherRun)
			h.opencodeWrites("--session", h.retroOut(workflow.Researcher), validRetro)
			Expect(run(workflow.Researcher)).To(BeZero())

			push, retro := -1, -1

			for i, c := range h.runner.cmds {
				switch {
				case strings.Contains(joined(c), "push --force origin research/emulate"):
					push = i
				case strings.Contains(joined(c), "--session") && strings.Contains(joined(c), "researcher"):
					retro = i
				}
			}

			Expect(push).To(BeNumerically(">=", 0))
			Expect(retro).To(BeNumerically(">", push), "the retro ran before the push")
		})

		It("reuses the branch and the resolution comment on a re-run", func() {
			h.runner.wrap(func(c proc.Cmd, base func(proc.Cmd) (string, error)) (string, error) {
				if strings.Contains(joined(c), "ls-remote --heads origin research/emulate") {
					return "sha-branch\trefs/heads/research/emulate\n", nil
				}

				return base(c)
			})
			h.researcherWrites(researcherRun)

			Expect(run(workflow.Researcher)).Error().NotTo(HaveOccurred())
			Expect(run(workflow.Researcher)).Error().NotTo(HaveOccurred())

			Expect(h.runner.find("fetch origin research/emulate")).To(HaveLen(2))
			Expect(h.runner.find("checkout -B research/emulate")).To(HaveLen(2))
			Expect(h.runner.find("reset --soft FETCH_HEAD")).To(HaveLen(2))
			Expect(h.runner.find("noreply.github.com push")).To(HaveLen(2))
			Expect(h.runner.find("--force")).To(BeEmpty(), "forced over an existing findings branch")

			Expect(h.gh.Bodies("POST", "/repos/o/r/issues/5/comments")).To(HaveLen(1), "posted twice")
			Expect(h.gh.Summary()).To(ContainElement(ContainSubstring("PATCH /repos/o/r/issues/comments/")))
			Expect(h.gh.Bodies("PATCH", "/repos/o/r/issues/6")).To(HaveLen(1), "appended the decision twice")
		})

		It("rejects a file with no branch, applying nothing", func() {
			h.researcherWrites(`{"version":1,"resolution":"r","decision_line":"d"}`)

			code, err := run(workflow.Researcher)
			Expect(code).To(Equal(workflow.ExitReported))
			Expect(err).To(HaveOccurred())
			Expect(h.gh.Bodies("POST", "/repos/o/r/issues/5/comments")).To(ConsistOf(
				ContainSubstring("output file was rejected")))
			Expect(h.runner.find("noreply.github.com push")).To(BeEmpty())
		})

		It("fails without a parent map, pushing nothing", func() {
			h.setBody(5, "## Question\n\nWhich?")
			h.researcherWrites(researcherRun)

			code, err := run(workflow.Researcher)
			Expect(err).To(MatchError(ContainSubstring("parent")))
			Expect(code).NotTo(BeZero())
			Expect(h.runner.find("noreply.github.com push")).To(BeEmpty())
		})

		It("gives a run that failed at the model its retro", func() {
			h.answerOpencode(errStream("APIError", 500))
			h.opencodeWrites("--session", h.retroOut(workflow.Researcher), validRetro)

			code, err := run(workflow.Researcher)
			Expect(err).To(HaveOccurred())
			Expect(code).NotTo(BeZero())
			Expect(h.retroComments()).To(ConsistOf(
				ContainSubstring("<!-- factory:retro issue=5 role=researcher key=sha-head -->")))
		})
	})

	It("fails without a token file, never calling GitHub", func() {
		code, err := run(workflow.Implementer)
		Expect(err).To(HaveOccurred())
		Expect(code).NotTo(BeZero())
		Expect(h.gh.Calls()).To(BeEmpty())
	})
})
