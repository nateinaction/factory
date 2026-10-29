package foreman

import (
	"context"
	"errors"
	"os"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/nateinaction/factory/foreman/internal/proc"
	"github.com/nateinaction/factory/foreman/internal/workflow"
)

var _ = Describe("Start", func() {
	var h *harness

	BeforeEach(func() {
		h = newHarness()
	})

	// start starts the agent role on issue 5 of o/r.
	start := func(role workflow.Role) (int, error) {
		return Start(context.Background(), h.deps, testConfig(), role, testRef(5))
	}

	// expectTokenRemoved fails the spec when the token file outlived the run.
	expectTokenRemoved := func(role workflow.Role) {
		GinkgoHelper()

		_, err := os.Stat(h.deps.Settings.TokenPath(role, h.deps.RunID))
		Expect(err).To(MatchError(os.ErrNotExist), "token file left behind")
	}

	Describe("the Implementer", func() {
		BeforeEach(func() {
			h.acceptWrites()
		})

		It("runs the factory half with the token in place, then queues the review", func() {
			seen := h.sudoExit(0)

			Expect(start(workflow.Implementer)).To(BeZero())

			sudo := h.runner.find("sudo")
			Expect(sudo).To(HaveLen(1))
			// The supervisor outlives the run, its retro and the git work around them.
			Expect(sudo[0].Timeout).To(BeNumerically(">", testAgent().Deadline()+testRetro().Deadline()))
			Expect(sudo[0].Argv).To(Equal([]string{
				"sudo", "-n", "-u", "factory", "/usr/local/bin/foreman", "run", "implementer", "o/r", "5",
			}))
			Expect(sudo[0].Env).To(HaveKeyWithValue("DAG_RUN_ID", "run-1"),
				"the factory half records the same markers, so it must name the same run")
			Expect(*seen).To(Equal(factoryHalf{token: "minted-token-implementer", mode: 0o640}))
			expectTokenRemoved(workflow.Implementer)

			labels, comments := h.outcome()
			Expect(labels).To(ConsistOf(ContainSubstring(workflow.LabelReview)))
			Expect(comments[0]).To(ContainSubstring(
				"The Implementer finished; see the linked pull request. Queued for review."))
			Expect(comments[0]).To(ContainSubstring(
				"<!-- factory:handoff role=implementer run=run-1 " +
					"from=factory:implementing to=factory:review sha= -->"))
			Expect(h.gh.Summary()).To(
				ContainElement("DELETE /repos/o/r/issues/5/labels/factory:implementing"), "did not release the claim")
			Expect(h.runner.find("github-app-token")).To(HaveLen(2), "want a fresh token after the run")
			h.expectLeakFree("minted-token")
		})

		It("marks the issue failed when the factory half fails", func() {
			h.sudoExit(1)
			Expect(start(workflow.Implementer)).To(Equal(1))

			labels, comments := h.outcome()
			Expect(labels[0]).To(ContainSubstring(workflow.LabelFailed))
			Expect(comments[0]).To(ContainSubstring(
				"The Implementer failed. Check the dagu run log, then comment to send it back to the Implementer."))
			Expect(comments[0]).To(ContainSubstring("to=factory:failed"))
			expectTokenRemoved(workflow.Implementer)
		})

		It("marks the issue failed when the factory half cannot run at all", func() {
			h.runner.wrap(func(c proc.Cmd, base func(proc.Cmd) (string, error)) (string, error) {
				if c.Argv[0] == "sudo" {
					return "", errors.New("sudo missing")
				}

				return base(c)
			})
			Expect(start(workflow.Implementer)).To(Equal(1))

			labels, _ := h.outcome()
			Expect(labels[0]).To(ContainSubstring(workflow.LabelFailed))
			Expect(h.logs.String()).To(ContainSubstring("sudo missing"))
		})

		It("fails the issue without describing the output twice when the factory half already did", func() {
			h.acceptWrites()
			h.sudoExit(workflow.ExitReported)

			Expect(start(workflow.Implementer)).To(Equal(1))

			labels, comments := h.outcome()
			Expect(labels[0]).To(ContainSubstring(workflow.LabelFailed))
			Expect(comments).To(HaveLen(1), "a second comment described the reported errors again")
			Expect(comments[0]).To(ContainSubstring("to=factory:failed"))
			Expect(comments[0]).NotTo(ContainSubstring("Check the dagu run log"))
		})
	})

	DescribeTable("the Reviewer routes the issue by the factory half's exit code",
		func(code int, label, text string, exit int) {
			h.acceptWrites()
			h.sudoExit(code)
			Expect(start(workflow.Reviewer)).To(Equal(exit))

			labels, comments := h.outcome()
			Expect(labels[0]).To(ContainSubstring(label))
			Expect(comments[0]).To(ContainSubstring(text))
			Expect(comments[0]).To(ContainSubstring("from=factory:reviewing to=" + label))
			Expect(h.gh.Summary()).To(
				ContainElement("DELETE /repos/o/r/issues/5/labels/factory:reviewing"), "did not release the claim")
			Expect(joined(h.runner.find("sudo")[0])).To(ContainSubstring("run reviewer o/r 5"))
		},
		Entry("clean", workflow.ReviewClean, workflow.LabelDone,
			"Review found no high or medium issues; see the pull request.", 0),
		Entry("findings", workflow.ReviewFindings, workflow.LabelImplement,
			"Review found high or medium issues, posted on the pull request. Sent back to the Implementer.", 0),
		Entry("exhausted", workflow.ReviewExhausted, workflow.LabelFailed,
			"Review still found high or medium issues after 3 rounds. A person needs to look at the pull request.", 1),
		Entry("failed", 2, workflow.LabelFailed,
			"The Reviewer failed. Check the dagu run log, then comment to send it back to the Reviewer.", 1),
	)

	Describe("the Researcher", func() {
		BeforeEach(func() {
			h.installed[workflow.Researcher] = []string{"o/r"}
			h.acceptWrites()
		})

		It("closes the ticket, releasing the claim", func() {
			h.sudoExit(0)

			Expect(start(workflow.Researcher)).To(BeZero())

			labels, comments := h.outcome()
			Expect(labels).To(BeEmpty(), "a closed ticket carries no factory label")
			Expect(comments[0]).To(ContainSubstring(
				"<!-- factory:handoff role=researcher run=run-1 " +
					"from=factory:researching to=closed sha= -->"))
			Expect(h.gh.Summary()).To(
				ContainElement("DELETE /repos/o/r/issues/5/labels/factory:researching"), "did not release the claim")
			Expect(h.gh.Summary()).To(ContainElement("PATCH /repos/o/r/issues/5"), "did not close the ticket")

			issue, err := emulator.Issue(context.Background(), "o/r", 5)
			Expect(err).NotTo(HaveOccurred())
			Expect(issue.State).To(Equal("closed"))
			expectTokenRemoved(workflow.Researcher)
			h.expectLeakFree("minted-token")
		})

		It("marks the ticket failed when the factory half fails", func() {
			h.sudoExit(1)

			Expect(start(workflow.Researcher)).To(Equal(1))

			labels, comments := h.outcome()
			Expect(labels[0]).To(ContainSubstring(workflow.LabelFailed))
			Expect(comments[0]).To(ContainSubstring(
				"The Researcher failed. Check the dagu run log, then comment to send it back to the Researcher."))

			issue, err := emulator.Issue(context.Background(), "o/r", 5)
			Expect(err).NotTo(HaveOccurred())
			Expect(issue.State).To(Equal("open"))
		})
	})

	Describe("the Curator", func() {
		BeforeEach(func() {
			h.installed[workflow.Curator] = []string{"o/r"}
		})

		It("runs the factory half without touching an issue's labels", func() {
			seen := h.sudoExit(0)
			h.out.Reset()

			Expect(start(workflow.Curator)).To(BeZero())

			sudo := h.runner.find("sudo")
			Expect(sudo).To(HaveLen(1))
			Expect(sudo[0].Argv).To(Equal([]string{
				"sudo", "-n", "-u", "factory", "/usr/local/bin/foreman", "run", "curator", "o/r", "5",
			}))
			Expect(*seen).To(Equal(factoryHalf{token: "minted-token-curator", mode: 0o640}))
			Expect(h.out.String()).To(ContainSubstring("The Curator finished; see the retro dashboard."))
			expectTokenRemoved(workflow.Curator)
			h.expectAuthorizedAs(workflow.Curator)
		})

		It("reports a failed run without failing the dashboard", func() {
			h.sudoExit(1)

			code, err := start(workflow.Curator)
			Expect(code).NotTo(BeZero())
			Expect(err).To(MatchError(ContainSubstring("Curator failed")))
			Expect(h.gh.Bodies("POST", "/repos/o/r/issues/5/labels")).To(BeEmpty())
			Expect(h.gh.Bodies("POST", "/repos/o/r/issues/5/comments")).To(BeEmpty())
		})
	})

	Describe("the Planner", func() {
		BeforeEach(func() {
			h.installed[workflow.Planner] = []string{"o/r"}
		})

		It("records the factory half's round without a second transition", func() {
			h.acceptWrites()
			h.sudoExit(0)
			h.out.Reset()

			Expect(start(workflow.Planner)).To(BeZero())
			Expect(h.out.String()).To(ContainSubstring("The Planner posted a round"))
			Expect(h.markerComments(5)).To(BeEmpty(), "the factory half already moved the issue")
			Expect(h.runner.find("sudo")).To(HaveLen(1))
			Expect(joined(h.runner.find("sudo")[0])).To(ContainSubstring("run planner o/r 5"))
		})

		It("mints only its own App's tokens", func() {
			h.acceptWrites()
			seen := h.sudoExit(0)

			Expect(start(workflow.Planner)).Error().NotTo(HaveOccurred())

			mints := h.runner.find("app-token", " token ")
			Expect(mints).To(HaveLen(1), "a successful round needs no second transition")

			for _, c := range mints {
				Expect(appOf(c)).To(Equal(string(workflow.Planner)))
				Expect(c.Argv[len(c.Argv)-2:]).To(Equal([]string{"--repo", "o/r"}))
			}

			h.expectAuthorizedAs(workflow.Planner)
			Expect(seen.token).To(Equal("minted-token-planner"))
		})

		It("moves a failed run from planning to failed", func() {
			h.acceptWrites()
			h.sudoExit(2)

			Expect(start(workflow.Planner)).To(Equal(1))

			labels, comments := h.outcome()
			Expect(labels[0]).To(ContainSubstring(workflow.LabelFailed))
			Expect(comments[0]).To(ContainSubstring(
				"from=factory:planning to=factory:failed"))
			Expect(comments[0]).To(ContainSubstring("The Planner failed"))
		})
	})

	Describe("identity", func() {
		DescribeTable("mints only the owning App's tokens: no App falls back to another's",
			func(role workflow.Role) {
				h.installed[role] = []string{"o/r"}
				h.acceptWrites()
				seen := h.sudoExit(0)

				Expect(start(role)).Error().NotTo(HaveOccurred())

				mints := h.runner.find("app-token", " token ")
				Expect(mints).To(HaveLen(2))

				for _, c := range mints {
					Expect(appOf(c)).To(Equal(string(role)))
					Expect(c.Argv[len(c.Argv)-2:]).To(Equal([]string{"--repo", "o/r"}))
				}

				h.expectAuthorizedAs(role)
				Expect(seen.token).To(Equal("minted-token-" + string(role)))
			},
			Entry(nil, workflow.Researcher), Entry(nil, workflow.Implementer), Entry(nil, workflow.Reviewer),
		)

		It("fails a run whose App credential is missing, reported by an App still installed", func() {
			h.acceptWrites()
			h.failMint(workflow.Implementer, false)
			Expect(start(workflow.Implementer)).To(Equal(1))
			Expect(h.runner.find("sudo")).To(BeEmpty(), "ran the agent without its credential")
			labels, comments := h.outcome()
			Expect(labels).To(ConsistOf(ContainSubstring(workflow.LabelFailed)))
			Expect(comments[0]).To(ContainSubstring("credential"))
			h.expectAuthorizedAs(workflow.Reviewer)
		})

		It("still fails, writing nothing, when no App is left to report it", func() {
			h.failMint(workflow.Implementer, false)
			h.failMint(workflow.Reviewer, false)

			code, err := start(workflow.Implementer)
			Expect(err).To(HaveOccurred())
			Expect(code).To(Equal(1))
			Expect(h.gh.Calls()).To(BeEmpty(), "wrote without a token")
		})
	})
})
