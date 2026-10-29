package foreman

import (
	"context"
	"fmt"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/nateinaction/factory/foreman/internal/ghtest"
	"github.com/nateinaction/factory/foreman/internal/proc"
	"github.com/nateinaction/factory/foreman/internal/workflow"
)

// curatorSkill, curatorDocs and curatorReview are the groups these specs work with.
var ( //nolint:gochecknoglobals // read-only test fixtures
	curatorSkillTarget = workflow.RetroTarget{Kind: workflow.TargetSkill, Name: "implement"}     //nolint:gochecknoglobals
	curatorDocsTarget  = workflow.RetroTarget{Kind: workflow.TargetDocs, Name: "AGENTS.md"}      //nolint:gochecknoglobals
	curatorSkillGroup  = workflow.GroupID{Target: curatorSkillTarget, Key: "guide-misses-tests"} //nolint:gochecknoglobals
	curatorDocsGroup   = workflow.GroupID{Target: curatorDocsTarget, Key: "no-test-command"}     //nolint:gochecknoglobals
	curatorReviewGroup = workflow.GroupID{Target: curatorSkillTarget, Key: "guide-skips-review"} //nolint:gochecknoglobals
)

var _ = Describe("the Curator", func() {
	var (
		h     *harness
		clock time.Time
	)

	BeforeEach(func() {
		h = newHarness()
		h.installed[workflow.Curator] = []string{"o/r"}

		// One fixed clock per spec, just ahead of the emulator's wall-clock stamps, so a
		// test's events never land after it: nothing advances it while the test runs.
		clock = time.Now().Add(time.Hour)
		h.deps.Now = func() time.Time { return clock }
	})

	ctx := context.Background()

	// dashboard creates the repository's dashboard and returns its number.
	dashboard := func(state workflow.DashboardState) int {
		GinkgoHelper()

		body, err := workflow.RenderDashboard(workflow.BuildDashboard(nil, state, clock))
		Expect(err).NotTo(HaveOccurred())

		number, err := emulator.CreateIssueAs(ctx, "o/r", tokenFor(appLogin(workflow.Implementer)),
			nil, workflow.DashboardTitle, body)
		Expect(err).NotTo(HaveOccurred())

		return number
	}

	// seedRetro posts a retro finding on number, authored by the Implementer's App.
	seedRetro := func(number int, finding workflow.RetroFinding) {
		GinkgoHelper()

		body, err := workflow.RenderRetro(workflow.RetroMarker(number, workflow.Implementer, "sha-a"),
			[]workflow.RetroFinding{finding})
		Expect(err).NotTo(HaveOccurred())
		h.comment(number, appLogin(workflow.Implementer), body)
	}

	finding := func(group workflow.GroupID, claim string) workflow.RetroFinding {
		return workflow.RetroFinding{
			Target: group.Target, Group: group.Key, Severity: workflow.SeverityHigh,
			Claim: claim, Evidence: "the session searched and found none",
		}
	}

	// ready makes group ready from one high-severity report on a fresh issue.
	ready := func(group workflow.GroupID) {
		seedRetro(h.createAs("o", nil, "An issue"), finding(group, "the guide never mentions tests"))
	}

	// runCuratorRun runs the Curator on number.
	runCuratorRun := func(number int) (int, error) {
		h.writeToken(workflow.Curator, "repo-token\n")

		return Run(ctx, h.deps, workflow.Curator, testRef(number), testAgent(), testRetro(), 3)
	}

	// dashboardBody reads a dashboard's body back.
	dashboardBody := func(number int) string {
		GinkgoHelper()

		var issue struct {
			Body string `json:"body"`
		}

		path := fmt.Sprintf("/repos/o/r/issues/%d", number)
		Expect(emulator.Do(ctx, "GET", path, ghtest.AdminToken, nil, &issue)).To(Succeed())

		return issue.Body
	}

	// filedIn returns the issues of repo whose body starts with a promotion marker for group.
	filedIn := func(repo string, group workflow.GroupID) []ghtest.Issue {
		GinkgoHelper()

		issues, err := emulator.Issues(ctx, repo)
		Expect(err).NotTo(HaveOccurred())

		var out []ghtest.Issue

		for _, issue := range issues {
			found, ok, err := workflow.ParsePromoted(issue.Body)
			Expect(err).NotTo(HaveOccurred())

			if ok && found == group {
				out = append(out, issue)
			}
		}

		return out
	}

	It("reads the log and dashboard, records proposals and files a ready group, with no retro", func() {
		number := dashboard(workflow.DashboardState{})

		ready(curatorDocsGroup)

		out := `{"version":1,"merges":[],"links":[],"issues":[{"group":"` + curatorDocsGroup.String() +
			`","title":"Add a test command","body":"Say how to run tests."}]}`
		h.roleWrites(workflow.Curator, number, out)

		Expect(runCuratorRun(number)).To(BeZero())

		filed := filedIn("o/r", curatorDocsGroup)
		Expect(filed).To(HaveLen(1), "did not file the ready group")
		Expect(filed[0].Body).To(ContainSubstring("## Lesson"))
		Expect(filed[0].Body).To(ContainSubstring("Say how to run tests."))

		body := dashboardBody(number)
		Expect(body).To(ContainSubstring("## Promoted (1)"))
		Expect(body).To(ContainSubstring("promoted by curator"))

		state, err := workflow.ParseDashboard(body)
		Expect(err).NotTo(HaveOccurred())
		Expect(state.Curator.LastRun).NotTo(BeZero(), "the run did not record that it happened")
		Expect(state.Curator.Claim).To(BeZero(), "the run did not clear its claim")

		// The Curator holds no Contents permission, so it clones nothing and resolves no toolchain
		// from the repository; and it runs no retro.
		Expect(h.runner.find("git clone")).To(BeEmpty(), "the Curator cloned the repository")
		Expect(h.runner.find("print-dev-env")).To(BeEmpty(), "the Curator resolved a dev environment")
		Expect(h.runner.find("--session")).To(BeEmpty(), "the Curator ran a retro")
		Expect(h.opencodeCalls()).To(HaveLen(1))

		prompt := joined(h.runCalls()[0])
		Expect(prompt).To(ContainSubstring("the session searched and found none"), "the log's evidence is missing")
		Expect(prompt).To(ContainSubstring("Write the JSON to " + h.rolePaths(workflow.Curator, number).out))
	})

	It("shows a merge proposal and a cross-target link under Proposed merges", func() {
		number := dashboard(workflow.DashboardState{})

		ready(curatorSkillGroup)
		ready(curatorReviewGroup)
		ready(curatorDocsGroup)

		out := `{"version":1,` +
			`"merges":[{"id":"m1","groups":["` + curatorSkillGroup.String() + `","` + curatorReviewGroup.String() +
			`"],"reason":"one guide problem"}],` +
			`"links":[{"id":"l1","groups":["` + curatorSkillGroup.String() + `","` + curatorDocsGroup.String() +
			`"],"reason":"the docs and the guide disagree"}],` +
			`"issues":[]}`
		h.roleWrites(workflow.Curator, number, out)

		Expect(runCuratorRun(number)).To(BeZero())

		body := dashboardBody(number)
		Expect(body).To(ContainSubstring("## Proposed merges (1)"))
		Expect(body).To(ContainSubstring("`m1`"))
		Expect(body).To(ContainSubstring("one guide problem"))
		Expect(body).To(ContainSubstring("### Links (1)"))
		Expect(body).To(ContainSubstring("`l1`"))
		Expect(body).To(ContainSubstring("the docs and the guide disagree"))
	})

	It("never proposes a rejected pair again", func() {
		number := dashboard(workflow.DashboardState{Rejected: []workflow.DashboardRejection{{
			ID: "old", Groups: []workflow.GroupID{curatorSkillGroup, curatorReviewGroup},
			By: "o", At: clock,
		}}})
		ready(curatorSkillGroup)
		ready(curatorReviewGroup)

		out := `{"version":1,"merges":[{"id":"new","groups":["` + curatorSkillGroup.String() + `","` +
			curatorReviewGroup.String() + `"],"reason":"one guide problem"}],"links":[],"issues":[]}`
		h.roleWrites(workflow.Curator, number, out)

		Expect(runCuratorRun(number)).To(BeZero())
		Expect(dashboardBody(number)).To(ContainSubstring("## Proposed merges (0)"))
	})

	It("keeps a private draft's lesson and evidence apart, and promotes no skill group", func() {
		h.asPrivate()

		number := dashboard(workflow.DashboardState{})

		ready(curatorDocsGroup)
		ready(curatorSkillGroup)

		out := `{"version":1,"merges":[],"links":[],"issues":[` +
			`{"group":"` + curatorDocsGroup.String() + `","title":"Add a test command",` +
			`"lesson":"Say how to run tests.","private_evidence":"a private repository detail"},` +
			`{"group":"` + curatorSkillGroup.String() + `","title":"Fix the guide","lesson":"Mention tests."}]}`
		h.roleWrites(workflow.Curator, number, out)

		Expect(runCuratorRun(number)).To(BeZero())

		filed := filedIn("o/r", curatorDocsGroup)
		Expect(filed).To(HaveLen(1))
		Expect(filed[0].Body).To(ContainSubstring("## Lesson"))
		Expect(filed[0].Body).To(ContainSubstring("## Private evidence"))
		Expect(filed[0].Body).To(ContainSubstring("a private repository detail"))

		Expect(filedIn("o/r", curatorSkillGroup)).To(BeEmpty(),
			"promoted a private repository's skill group without a writer")
	})

	It("files a public skill group in the factory repo", func() {
		factory := h.factoryRepo()
		h.deps.Settings.FactoryRepo = factory
		h.installed[workflow.Curator] = append(h.installed[workflow.Curator], factory)

		number := dashboard(workflow.DashboardState{})

		ready(curatorSkillGroup)

		out := `{"version":1,"merges":[],"links":[],"issues":[{"group":"` + curatorSkillGroup.String() +
			`","title":"Fix the guide","body":"Mention tests."}]}`
		h.roleWrites(workflow.Curator, number, out)

		Expect(runCuratorRun(number)).To(BeZero())

		Expect(filedIn("o/r", curatorSkillGroup)).To(BeEmpty(), "filed a public skill group in the repository")
		filed := filedIn(factory, curatorSkillGroup)
		Expect(filed).To(HaveLen(1), "did not file the group in the factory repo")
		Expect(dashboardBody(number)).To(ContainSubstring(fmt.Sprintf("/issues/%d", filed[0].Number)))
	})

	It("applies the same output twice without filing a second issue", func() {
		number := dashboard(workflow.DashboardState{})

		ready(curatorDocsGroup)

		out := `{"version":1,"merges":[],"links":[],"issues":[{"group":"` + curatorDocsGroup.String() +
			`","title":"Add a test command","body":"Say how to run tests."}]}`
		h.roleWrites(workflow.Curator, number, out)

		Expect(runCuratorRun(number)).To(BeZero())
		Expect(runCuratorRun(number)).To(BeZero())

		Expect(filedIn("o/r", curatorDocsGroup)).To(HaveLen(1), "filed the group twice")
	})

	It("files the drafts it can when one fails, and reports the failure", func() {
		// No factory repository is configured, so a public skill group has nowhere to go.
		number := dashboard(workflow.DashboardState{})

		ready(curatorSkillGroup)
		ready(curatorDocsGroup)

		out := `{"version":1,"merges":[],"links":[],"issues":[` +
			`{"group":"` + curatorSkillGroup.String() + `","title":"Fix the guide","body":"Mention tests."},` +
			`{"group":"` + curatorDocsGroup.String() + `","title":"Add a test command","body":"Say how to run tests."}]}`
		h.roleWrites(workflow.Curator, number, out)

		code, err := runCuratorRun(number)
		Expect(err).To(MatchError(ContainSubstring("factory repository")))
		Expect(code).NotTo(BeZero())

		Expect(filedIn("o/r", curatorSkillGroup)).To(BeEmpty(), "filed a group with nowhere to go")
		Expect(filedIn("o/r", curatorDocsGroup)).To(HaveLen(1), "one failed draft stopped the rest")
	})

	It("fails on an invalid output without commenting on the dashboard", func() {
		number := dashboard(workflow.DashboardState{})

		ready(curatorDocsGroup)

		before := dashboardBody(number)

		h.roleWrites(workflow.Curator, number, `{"version":1,"merges":[],"links":[],"issues":[{"group":"`+
			curatorDocsGroup.String()+`","title":"t","body":"b","lesson":"l"}]}`)

		code, err := runCuratorRun(number)
		Expect(err).To(HaveOccurred())
		Expect(code).NotTo(BeZero())
		Expect(dashboardBody(number)).To(Equal(before), "changed the dashboard for a rejected file")
		Expect(h.gh.Bodies("POST", fmt.Sprintf("/repos/o/r/issues/%d/comments", number))).To(BeEmpty(),
			"commented on the dashboard")
	})
})

var _ = Describe("the poll's Curator trigger", func() {
	var (
		h     *harness
		clock time.Time
	)

	BeforeEach(func() {
		h = newHarness()
		h.installed[workflow.Curator] = []string{"o/r"}

		// One fixed clock per spec, just ahead of the emulator's wall-clock stamps, so a
		// test's events never land after it: nothing advances it while the test runs.
		clock = time.Now().Add(time.Hour)
		h.deps.Now = func() time.Time { return clock }
	})

	ctx := context.Background()

	poll := func() (int, error) {
		return Poll(ctx, h.deps, pollConfig(workflow.Implementer, workflow.Curator))
	}

	// dashboards lists o/r's dashboards.
	dashboards := func() []ghtest.Issue {
		GinkgoHelper()

		issues, err := emulator.Issues(ctx, "o/r")
		Expect(err).NotTo(HaveOccurred())

		var out []ghtest.Issue

		for _, issue := range issues {
			if issue.Title == workflow.DashboardTitle {
				out = append(out, issue)
			}
		}

		return out
	}

	// curatorEnqueues are the dagu enqueues of a Curator run.
	curatorEnqueues := func() []string {
		GinkgoHelper()

		out := make([]string, 0, len(h.runner.find("enqueue", "factory-curator")))

		for _, c := range h.runner.find("enqueue", "factory-curator") {
			out = append(out, joined(c))
		}

		return out
	}

	It("queues one run for a retro that added findings, at most once a day", func() {
		number := h.createAs("o", nil, "An issue")
		body, err := workflow.RenderRetro(workflow.RetroMarker(number, workflow.Implementer, "sha-a"),
			[]workflow.RetroFinding{{
				Target: curatorDocsTarget, Group: curatorDocsGroup.Key, Severity: workflow.SeverityHigh,
				Claim: "no test command", Evidence: "searched and found none",
			}})
		Expect(err).NotTo(HaveOccurred())
		h.comment(number, appLogin(workflow.Implementer), body)

		Expect(poll()).Error().NotTo(HaveOccurred())

		dashboard := dashboards()
		Expect(dashboard).To(HaveLen(1))

		Expect(curatorEnqueues()).To(HaveLen(1))
		Expect(curatorEnqueues()[0]).To(ContainSubstring("ISSUE=" + fmt.Sprint(dashboard[0].Number)))

		Expect(poll()).Error().NotTo(HaveOccurred())
		Expect(curatorEnqueues()).To(HaveLen(1), "queued a second run before the first finished")

		h.deps.Now = func() time.Time { return clock.Add(25 * time.Hour) }

		Expect(poll()).Error().NotTo(HaveOccurred())
		Expect(curatorEnqueues()).To(HaveLen(2), "did not queue the next day's run")
	})

	It("queues no run for retro reports an outsider wrote", func() {
		number := h.createAs("o", nil, "An issue")
		body, err := workflow.RenderRetro(workflow.RetroMarker(number, workflow.Implementer, "sha-a"),
			[]workflow.RetroFinding{{
				Target: curatorDocsTarget, Group: curatorDocsGroup.Key, Severity: workflow.SeverityHigh,
				Claim: "an outsider's claim", Evidence: "searched and found none",
			}})
		Expect(err).NotTo(HaveOccurred())
		h.comment(number, "factory", body)

		Expect(poll()).Error().NotTo(HaveOccurred())

		Expect(curatorEnqueues()).To(BeEmpty(), "queued a run for an outsider's report")

		dashboard := dashboards()
		Expect(dashboard).To(HaveLen(1))
		Expect(dashboard[0].Body).To(ContainSubstring("## Ready to promote (0)"))
		Expect(dashboard[0].Body).NotTo(ContainSubstring("an outsider's claim"))
	})

	It("queues a run for a writer's curate, with no findings", func() {
		Expect(poll()).Error().NotTo(HaveOccurred())

		dashboard := dashboards()
		Expect(dashboard).To(HaveLen(1))
		Expect(curatorEnqueues()).To(BeEmpty(), "queued a run with nothing to curate")

		h.comment(dashboard[0].Number, "o", "curate")
		Expect(poll()).Error().NotTo(HaveOccurred())

		Expect(curatorEnqueues()).To(HaveLen(1))
		Expect(curatorEnqueues()[0]).To(ContainSubstring("ISSUE=" + fmt.Sprint(dashboard[0].Number)))

		Expect(poll()).Error().NotTo(HaveOccurred())
		Expect(curatorEnqueues()).To(HaveLen(1), "a curate bypassed the once-a-day limit")
	})

	It("queues nothing when the Curator DAG is already busy", func() {
		number := h.createAs("o", nil, "An issue")
		body, err := workflow.RenderRetro(workflow.RetroMarker(number, workflow.Implementer, "sha-a"),
			[]workflow.RetroFinding{{
				Target: curatorDocsTarget, Group: curatorDocsGroup.Key, Severity: workflow.SeverityHigh,
				Claim: "no test command", Evidence: "searched and found none",
			}})
		Expect(err).NotTo(HaveOccurred())
		h.comment(number, appLogin(workflow.Implementer), body)

		h.runner.wrap(func(c proc.Cmd, base func(proc.Cmd) (string, error)) (string, error) {
			if len(c.Argv) > 1 && c.Argv[1] == "history" {
				return `[{"name":"factory-curator","dagRunId":"r1","status":"queued"}]`, nil
			}

			return base(c)
		})

		Expect(poll()).Error().NotTo(HaveOccurred())
		Expect(curatorEnqueues()).To(BeEmpty(), "queued a second run while the Curator DAG was busy")
	})

	It("queues nothing when the Curator App is not installed", func() {
		delete(h.installed, workflow.Curator)

		number := h.createAs("o", nil, "An issue")
		body, err := workflow.RenderRetro(workflow.RetroMarker(number, workflow.Implementer, "sha-a"),
			[]workflow.RetroFinding{{
				Target: curatorDocsTarget, Group: curatorDocsGroup.Key, Severity: workflow.SeverityHigh,
				Claim: "no test command", Evidence: "searched and found none",
			}})
		Expect(err).NotTo(HaveOccurred())
		h.comment(number, appLogin(workflow.Implementer), body)

		Expect(poll()).Error().NotTo(HaveOccurred())
		Expect(curatorEnqueues()).To(BeEmpty())
	})

	It("never queues a run on a repository whose dashboard body cannot be read", func() {
		_, err := emulator.CreateIssueAs(ctx, "o/r", tokenFor(appLogin(workflow.Implementer)),
			nil, workflow.DashboardTitle, "hand-written")
		Expect(err).NotTo(HaveOccurred())

		Expect(poll()).Error().NotTo(HaveOccurred())
		Expect(curatorEnqueues()).To(BeEmpty())
		Expect(h.logs.String()).To(ContainSubstring("cannot redraw the retro dashboard"))
	})

	It("queues a curator run when only the Curator App is installed", func() {
		h.installed = map[workflow.Role][]string{workflow.Curator: {"o/r"}}

		number := h.createAs("o", nil, "An issue")
		body, err := workflow.RenderRetro(workflow.RetroMarker(number, workflow.Implementer, "sha-a"),
			[]workflow.RetroFinding{{
				Target: curatorDocsTarget, Group: curatorDocsGroup.Key, Severity: workflow.SeverityHigh,
				Claim: "no test command", Evidence: "searched and found none",
			}})
		Expect(err).NotTo(HaveOccurred())
		h.comment(number, appLogin(workflow.Implementer), body)

		Expect(Poll(ctx, h.deps, pollConfig(workflow.Curator))).Error().NotTo(HaveOccurred())
		Expect(curatorEnqueues()).To(HaveLen(1))
	})
})
