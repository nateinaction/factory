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

	// start starts role on issue 5 of o/r.
	start := func(role workflow.Role) (int, error) {
		return Start(context.Background(), h.deps, testConfig(), role, "o/r", 5)
	}

	// expectTokenRemoved fails the spec when the token file outlived the run.
	expectTokenRemoved := func(role workflow.Role) {
		GinkgoHelper()

		_, err := os.Stat(h.deps.Settings.TokenPath(role))
		Expect(err).To(MatchError(os.ErrNotExist), "token file left behind")
	}

	Describe("the Implementer", func() {
		BeforeEach(func() {
			h.acceptWrites("5")
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
			Expect(*seen).To(Equal(factoryHalf{token: "minted-token-implementer", mode: 0o640}))
			expectTokenRemoved(workflow.Implementer)

			labels, comments := h.outcome()
			Expect(labels).To(ConsistOf(ContainSubstring(workflow.LabelReviewReady)))
			Expect(comments[0]).To(ContainSubstring("The agent finished; see the linked pull request. Queued for review."))
			Expect(h.gh.Summary()).To(
				ContainElement("DELETE /repos/o/r/issues/5/labels/agent-working"), "did not release the claim")
			Expect(h.runner.find("github-app-token")).To(HaveLen(2), "want a fresh token after the run")
			h.expectLeakFree("minted-token")
		})

		It("marks the issue failed when the factory half fails", func() {
			h.sudoExit(1)
			Expect(start(workflow.Implementer)).To(Equal(1))

			labels, comments := h.outcome()
			Expect(labels[0]).To(ContainSubstring(workflow.LabelFailed))
			Expect(comments[0]).To(ContainSubstring(
				"The agent failed. Check the dagu run log, then re-apply `ready-for-agent` to retry."))
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
	})

	DescribeTable("the Reviewer routes the issue by the factory half's exit code",
		func(code int, label, text string, exit int) {
			h.acceptWrites("5")
			h.sudoExit(code)
			Expect(start(workflow.Reviewer)).To(Equal(exit))

			labels, comments := h.outcome()
			Expect(labels[0]).To(ContainSubstring(label))
			Expect(comments[0]).To(ContainSubstring(text))
			Expect(h.gh.Summary()).To(
				ContainElement("DELETE /repos/o/r/issues/5/labels/agent-reviewing"), "did not release the claim")
			Expect(joined(h.runner.find("sudo")[0])).To(ContainSubstring("run reviewer o/r 5"))
		},
		Entry("clean", workflow.ReviewClean, workflow.LabelDone,
			"Review found no high or medium issues; see the pull request.", 0),
		Entry("findings", workflow.ReviewFindings, workflow.LabelReady,
			"Review found high or medium issues, posted on the pull request. Sent back to the agent.", 0),
		Entry("exhausted", workflow.ReviewExhausted, workflow.LabelFailed,
			"Review still found high or medium issues after 3 rounds. A person needs to look at the pull request.", 1),
		Entry("failed", 2, workflow.LabelFailed,
			"The review failed. Check the dagu run log, then re-apply `ready-for-agent-review` to retry.", 1),
	)

	DescribeTable("refuses an agent role without behaviour, doing nothing",
		func(role workflow.Role) {
			code, err := start(role)
			Expect(err).To(HaveOccurred())
			Expect(code).NotTo(BeZero())
			h.expectNoWork()
		},
		Entry(nil, workflow.Planner), Entry(nil, workflow.Researcher), Entry(nil, workflow.Curator),
	)

	Describe("identity", func() {
		DescribeTable("mints only the owning App's tokens: no App falls back to another's",
			func(role workflow.Role) {
				h.acceptWrites("5")
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
			Entry(nil, workflow.Implementer), Entry(nil, workflow.Reviewer),
		)

		It("fails a run whose App credential is missing, reported by an App still installed", func() {
			h.acceptWrites("5")
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
