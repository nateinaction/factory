package foreman

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/nateinaction/factory/foreman/internal/config"
	"github.com/nateinaction/factory/foreman/internal/ghtest"
	"github.com/nateinaction/factory/foreman/internal/workflow"
)

// minimize is the answer the harness gives foreman's GraphQL calls: whether the App may minimize a
// comment, which comments it asked about, and which it minimized.
type minimize struct {
	mu        sync.Mutex
	can       bool
	queried   []string
	minimized []string
}

// minimizedComments are the comment nodes foreman minimized.
func (m *minimize) minimizedComments() []string {
	m.mu.Lock()
	defer m.mu.Unlock()

	return append([]string(nil), m.minimized...)
}

// graphql answers foreman's GraphQL calls with a live viewerCanMinimize of can.
func (h *harness) graphql(can bool) *minimize {
	state := &minimize{can: can}

	h.gh.Handle("POST /graphql", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Query     string `json:"query"`
			Variables struct {
				ID string `json:"id"`
			} `json:"variables"`
		}

		body, err := io.ReadAll(r.Body)
		Expect(err).NotTo(HaveOccurred())
		Expect(json.Unmarshal(body, &in)).To(Succeed())

		state.mu.Lock()
		defer state.mu.Unlock()

		if strings.Contains(in.Query, "minimizeComment") {
			state.minimized = append(state.minimized, in.Variables.ID)

			ghtest.WriteJSON(w, map[string]any{"data": map[string]any{
				"minimizeComment": map[string]any{"minimizedComment": map[string]any{"isMinimized": true}},
			}})

			return
		}

		state.queried = append(state.queried, in.Variables.ID)

		ghtest.WriteJSON(w, map[string]any{"data": map[string]any{
			"node": map[string]any{"viewerCanMinimize": state.can},
		}})
	})

	return state
}

// asPrivate makes foreman read o/r as a private repository.
func (h *harness) asPrivate() {
	h.gh.Handle("GET /repos/o/r", func(w http.ResponseWriter, _ *http.Request) {
		ghtest.WriteJSON(w, map[string]any{"private": true, "default_branch": "main"})
	})
}

// factoryRepo creates a repository to promote to, assigns it to every agent role, and returns it.
func (h *harness) factoryRepo() string {
	const repo = "o/factory"

	Expect(emulator.EnsureRepo(context.Background(), repo)).To(Succeed())
	DeferCleanup(func() {
		Expect(emulator.DeleteRepo(context.Background(), repo)).To(Succeed())
	})

	for role, repos := range h.installed {
		h.installed[role] = append(repos, repo)
	}

	return repo
}

// promoteConfig is a poll of o/r and the factory repo with the factory repo configured.
func promoteConfig(factory string) config.Config {
	cfg := pollConfig(workflow.Implementer, workflow.Reviewer)
	cfg.FactoryRepo = factory

	return cfg
}

// dashboardCommands is the flow of a writer's commands on the retro dashboard.
var _ = Describe("the dashboard's writer commands", func() {
	var (
		h     *harness
		clock time.Time
	)

	BeforeEach(func() {
		h = newHarness()

		// One fixed clock per spec, just ahead of the emulator's wall-clock stamps, so a
		// test's events never land after it: nothing advances it while the test runs.
		clock = time.Now().Add(time.Hour)
		h.deps.Now = func() time.Time { return clock }
	})

	ctx := context.Background()

	// poll polls o/r and, when it is configured, the factory repo.
	poll := func(cfg config.Config) (int, error) {
		return Poll(ctx, h.deps, cfg)
	}

	// skillGroup is the group most specs report on.
	skillGroup := workflow.GroupID{
		Target: workflow.RetroTarget{Kind: workflow.TargetSkill, Name: "implement"},
		Key:    "guide-misses-tests",
	}

	// docsGroup is a group about the repository's own agent docs.
	docsGroup := workflow.GroupID{
		Target: workflow.RetroTarget{Kind: workflow.TargetDocs, Name: "AGENTS.md"},
		Key:    "no-tests",
	}

	// finding is one retro finding about group.
	finding := func(group workflow.GroupID, severity workflow.Severity, claim string) workflow.RetroFinding {
		return workflow.RetroFinding{
			Target: group.Target, Group: group.Key, Severity: severity, Claim: claim,
			Evidence: "the session showed it",
		}
	}

	// seeded reports a group from a new issue authored by a factory App, and returns the issue.
	seeded := func(group workflow.GroupID, severity workflow.Severity, claim string) int {
		number := h.createAs("o", nil, "An issue")
		body, err := workflow.RenderRetro(workflow.RetroMarker(number, workflow.Implementer, "sha-a"),
			[]workflow.RetroFinding{finding(group, severity, claim)})
		Expect(err).NotTo(HaveOccurred())
		h.comment(number, appLogin(workflow.Implementer), body)

		return number
	}

	// theDashboardOf is a repository's one dashboard.
	theDashboardOf := func(repo string) dashboardIssue {
		GinkgoHelper()

		var out []dashboardIssue

		err := emulator.Do(ctx, http.MethodGet, "/repos/"+repo+"/issues?state=all&per_page=100",
			ghtest.AdminToken, nil, &out)
		Expect(err).NotTo(HaveOccurred())

		var found []dashboardIssue

		for _, issue := range out {
			if issue.Title == workflow.DashboardTitle {
				found = append(found, issue)
			}
		}

		Expect(found).To(HaveLen(1), "want exactly one dashboard in "+repo)

		return found[0]
	}

	// issuesOf lists a repository's open issues, oldest first.
	issuesOf := func(repo string) []ghtest.Issue {
		GinkgoHelper()

		out, err := emulator.Issues(ctx, repo)
		Expect(err).NotTo(HaveOccurred())

		return out
	}

	// ready reports a group from two issues, ready to promote, and returns its dashboard.
	ready := func(group workflow.GroupID, cfg config.Config) dashboardIssue {
		seeded(group, workflow.SeverityMedium, "the guide never mentions tests")
		seeded(group, workflow.SeverityMedium, "the guide never mentions tests either")

		Expect(poll(cfg)).Error().NotTo(HaveOccurred())

		return theDashboardOf("o/r")
	}

	// promotedIn are the issues in repo a group was promoted as.
	promotedIn := func(repo string, group workflow.GroupID) []ghtest.Issue {
		GinkgoHelper()

		var out []ghtest.Issue

		for _, issue := range issuesOf(repo) {
			found, ok, err := workflow.ParsePromoted(issue.Body)
			Expect(err).NotTo(HaveOccurred())

			if ok && found == group {
				out = append(out, issue)
			}
		}

		return out
	}

	// publishedIn are the issues in repo a group's lesson was published as.
	publishedIn := func(repo string, group workflow.GroupID) []ghtest.Issue {
		GinkgoHelper()

		var out []ghtest.Issue

		for _, issue := range issuesOf(repo) {
			found, ok, err := workflow.ParsePublished(issue.Body)
			Expect(err).NotTo(HaveOccurred())

			if ok && found == group {
				out = append(out, issue)
			}
		}

		return out
	}

	It("files a ready document group in the repository it is about, once", func() {
		graphql := h.graphql(true)

		dashboard := ready(docsGroup, pollConfig(workflow.Implementer, workflow.Reviewer))

		h.comment(dashboard.Number, "o", "promote "+docsGroup.String())
		Expect(poll(pollConfig(workflow.Implementer, workflow.Reviewer))).Error().NotTo(HaveOccurred())

		filed := promotedIn("o/r", docsGroup)
		Expect(filed).To(HaveLen(1), "did not file the promoted issue")
		Expect(filed[0].Body).To(ContainSubstring("## Lesson"))
		Expect(filed[0].Body).To(ContainSubstring("the guide never mentions tests"))

		body := theDashboardOf("o/r").Body
		Expect(body).To(ContainSubstring("## Promoted (1)"))
		Expect(body).To(ContainSubstring("promoted by o"))

		Expect(graphql.minimizedComments()).To(HaveLen(1), "did not minimize the command comment")

		// The comment is skipped on later polls: the group is filed once and the comment settled once.
		Expect(poll(pollConfig(workflow.Implementer, workflow.Reviewer))).Error().NotTo(HaveOccurred())
		Expect(promotedIn("o/r", docsGroup)).To(HaveLen(1), "filed the group twice")
		Expect(graphql.minimizedComments()).To(HaveLen(1), "settled the command comment twice")
	})

	It("files a public skill group in the factory repo", func() {
		h.graphql(true)
		factory := h.factoryRepo()
		cfg := promoteConfig(factory)
		dashboard := ready(skillGroup, cfg)

		h.comment(dashboard.Number, "o", "promote "+skillGroup.String())
		Expect(poll(cfg)).Error().NotTo(HaveOccurred())

		filed := promotedIn(factory, skillGroup)
		Expect(filed).To(HaveLen(1), "did not file the group in the factory repo")
		Expect(promotedIn("o/r", skillGroup)).To(BeEmpty(), "filed a public skill group in the repository")
		Expect(theDashboardOf("o/r").Body).To(ContainSubstring("## Promoted (1)"))
	})

	It("keeps a private repository's group there, lesson apart from evidence", func() {
		h.graphql(true)
		h.asPrivate()

		factory := h.factoryRepo()
		cfg := promoteConfig(factory)
		dashboard := ready(skillGroup, cfg)

		h.comment(dashboard.Number, "o", "promote "+skillGroup.String())
		Expect(poll(cfg)).Error().NotTo(HaveOccurred())

		filed := promotedIn("o/r", skillGroup)
		Expect(filed).To(HaveLen(1), "moved a private repository's group out")
		Expect(filed[0].Body).To(ContainSubstring("## Private evidence"))
		Expect(promotedIn(factory, skillGroup)).To(BeEmpty(), "filed a private group in the factory repo")

		// Only a writer's publish copies the lesson out, without the private evidence.
		h.comment(filed[0].Number, "o", "publish")
		Expect(poll(cfg)).Error().NotTo(HaveOccurred())

		Expect(promotedIn(factory, skillGroup)).To(BeEmpty(), "a publish is not a promotion")

		published := publishedIn(factory, skillGroup)
		Expect(published).To(HaveLen(1), "did not publish the lesson")
		Expect(published[0].Body).To(ContainSubstring("the guide never mentions tests"))
		Expect(published[0].Body).NotTo(ContainSubstring("the session showed it"), "leaked private evidence")
		Expect(published[0].Body).NotTo(ContainSubstring("## Private evidence"))

		// A repeat publishes nothing new.
		Expect(poll(cfg)).Error().NotTo(HaveOccurred())
		Expect(publishedIn(factory, skillGroup)).To(HaveLen(1), "published the lesson twice")
		Expect(theDashboardOf("o/r").Body).To(ContainSubstring("published by o"))
	})

	// seedPromoted files a promoted issue for group in o/r directly, with body when it is non-empty
	// and the lesson a promotion would render otherwise, and returns its number.
	seedPromoted := func(group workflow.GroupID, body string) int {
		GinkgoHelper()

		if body == "" {
			reports := []workflow.DashboardReport{{
				Role: workflow.Implementer, Key: "sha-a", Issue: 1, Severity: workflow.SeverityMedium,
				Claim: "the guide never mentions tests", Evidence: "the session showed it",
			}}

			_, rendered, err := workflow.RenderPromotion(group, reports, true)
			Expect(err).NotTo(HaveOccurred())

			body = rendered
		}

		number := h.createAs("o", nil, "Retro: "+group.String())
		h.setBody(number, body)

		return number
	}

	// expectNothingPublished polls after a publish command and expects the lesson stayed where it was.
	expectNothingPublished := func(factory string, cfg config.Config, group workflow.GroupID, why string) {
		GinkgoHelper()

		Expect(poll(cfg)).Error().NotTo(HaveOccurred())
		Expect(publishedIn(factory, group)).To(BeEmpty(), why)
		Expect(theDashboardOf("o/r").Body).NotTo(ContainSubstring("published by"), why)
	}

	Describe("a publish that fails closed", func() {
		var (
			factory string
			cfg     config.Config
		)

		BeforeEach(func() {
			h.graphql(true)
			factory = h.factoryRepo()
			cfg = promoteConfig(factory)
		})

		It("ignores a non-writer's publish of a private promoted issue", func() {
			h.asPrivate()
			ready(skillGroup, cfg)
			promoted := seedPromoted(skillGroup, "")

			h.comment(promoted, "factory", "publish")
			expectNothingPublished(factory, cfg, skillGroup, "a non-writer published a private lesson")
			Expect(h.logs.String()).To(ContainSubstring("a non-writer's dashboard command"))
		})

		It("does not publish when the repository's visibility cannot be read", func() {
			h.gh.Status("GET /repos/o/r", http.StatusInternalServerError)
			ready(skillGroup, cfg)
			promoted := seedPromoted(skillGroup, "")

			h.comment(promoted, "o", "publish")
			expectNothingPublished(factory, cfg, skillGroup, "published without knowing the repository is private")
			Expect(h.logs.String()).To(ContainSubstring("cannot read the repository's visibility; not publishing"))
		})

		It("does not publish out of a public repository", func() {
			ready(skillGroup, cfg)
			promoted := seedPromoted(skillGroup, "")

			h.comment(promoted, "o", "publish")
			expectNothingPublished(factory, cfg, skillGroup, "published a public repository's lesson")
			Expect(h.logs.String()).To(ContainSubstring("publish only copies a private repository's lesson out"))
		})

		It("does not publish a promoted issue that has no lesson", func() {
			h.asPrivate()
			ready(skillGroup, cfg)
			promoted := seedPromoted(skillGroup, workflow.PromotionMarker(skillGroup)+"\n\nNo lesson here.\n")

			h.comment(promoted, "o", "publish")
			expectNothingPublished(factory, cfg, skillGroup, "published an issue with no lesson")
			Expect(h.logs.String()).To(ContainSubstring("a promoted issue has no lesson to publish"))
		})
	})

	It("takes no action for a non-writer's command, and minimizes it", func() {
		graphql := h.graphql(true)

		dashboard := ready(skillGroup, pollConfig(workflow.Implementer, workflow.Reviewer))

		h.comment(dashboard.Number, "factory", "promote "+skillGroup.String())
		Expect(poll(pollConfig(workflow.Implementer, workflow.Reviewer))).Error().NotTo(HaveOccurred())

		Expect(promotedIn("o/r", skillGroup)).To(BeEmpty(), "a non-writer promoted a group")
		Expect(theDashboardOf("o/r").Body).To(ContainSubstring("## Promoted (0)"))
		Expect(graphql.minimizedComments()).To(HaveLen(1), "did not minimize the non-writer's command")
		Expect(h.logs.String()).To(ContainSubstring("a non-writer's dashboard command"))
		Expect(h.securityEvents()).To(ConsistOf(And(
			HaveKeyWithValue("event", "dashboard_command_refused"),
			HaveKeyWithValue("outcome", "unauthorized_author"),
			HaveKeyWithValue("repository", "o/r"),
			HaveKeyWithValue("issue", strconv.Itoa(dashboard.Number)),
		)))
	})

	It("fails closed on a command whose author's permission it cannot read, and records it", func() {
		h.graphql(true)

		dashboard := ready(skillGroup, pollConfig(workflow.Implementer, workflow.Reviewer))

		h.gh.Status("GET /repos/o/r/collaborators/o/permission", http.StatusInternalServerError)

		h.comment(dashboard.Number, "o", "promote "+skillGroup.String())
		Expect(poll(pollConfig(workflow.Implementer, workflow.Reviewer))).Error().NotTo(HaveOccurred())

		Expect(promotedIn("o/r", skillGroup)).To(BeEmpty(), "acted on an unreadable permission")
		Expect(h.securityEvents()).To(ConsistOf(And(
			HaveKeyWithValue("event", "dashboard_command_refused"),
			HaveKeyWithValue("outcome", "permission_unreadable"),
			HaveKeyWithValue("repository", "o/r"),
			HaveKeyWithValue("issue", strconv.Itoa(dashboard.Number)),
		)))
	})

	It("deletes a command when the App cannot minimize it", func() {
		h.graphql(false)

		dashboard := ready(docsGroup, pollConfig(workflow.Implementer, workflow.Reviewer))

		h.comment(dashboard.Number, "o", "promote "+docsGroup.String())
		Expect(poll(pollConfig(workflow.Implementer, workflow.Reviewer))).Error().NotTo(HaveOccurred())

		Expect(h.gh.Summary()).To(ContainElement(
			ContainSubstring("DELETE /repos/o/r/issues/comments/")), "did not delete the command")
		Expect(promotedIn("o/r", docsGroup)).To(HaveLen(1), "deleted the command without acting on it")
	})

	It("ignores a group", func() {
		h.graphql(true)

		dashboard := ready(skillGroup, pollConfig(workflow.Implementer, workflow.Reviewer))

		h.comment(dashboard.Number, "o", "ignore "+skillGroup.String())
		Expect(poll(pollConfig(workflow.Implementer, workflow.Reviewer))).Error().NotTo(HaveOccurred())

		body := theDashboardOf("o/r").Body
		Expect(body).To(ContainSubstring("## Ignored (1)"))
		Expect(body).To(ContainSubstring("ignored by o"))
		Expect(body).To(ContainSubstring("## Ready to promote (0)"))
	})

	It("merges two groups into the first, counting both groups' reports", func() {
		h.graphql(true)

		other := workflow.GroupID{Target: docsGroup.Target, Key: "no-test-command"}
		seeded(docsGroup, workflow.SeverityMedium, "the guide never mentions tests")
		seeded(other, workflow.SeverityMedium, "the guide never mentions tests either")

		cfg := pollConfig(workflow.Implementer, workflow.Reviewer)
		Expect(poll(cfg)).Error().NotTo(HaveOccurred())

		dashboard := theDashboardOf("o/r")
		h.comment(dashboard.Number, "o", "merge "+docsGroup.String()+" "+other.String())
		Expect(poll(cfg)).Error().NotTo(HaveOccurred())

		body := theDashboardOf("o/r").Body
		Expect(body).To(ContainSubstring("`" + docsGroup.String() + "` merged by o"))
		Expect(body).NotTo(ContainSubstring("`" + other.String() + "`"))

		// The merged group is one group: promoting it files both reports' lesson as one issue.
		h.comment(dashboard.Number, "o", "promote "+docsGroup.String())
		Expect(poll(cfg)).Error().NotTo(HaveOccurred())

		filed := promotedIn("o/r", docsGroup)
		Expect(filed).To(HaveLen(1))
		Expect(filed[0].Body).To(ContainSubstring("the guide never mentions tests"))
		Expect(filed[0].Body).To(ContainSubstring("the guide never mentions tests either"))
	})

	It("refuses a merge across targets, which is a link", func() {
		h.graphql(true)

		seeded(skillGroup, workflow.SeverityHigh, "the guide never mentions tests")
		seeded(docsGroup, workflow.SeverityHigh, "the docs never mention tests")

		cfg := pollConfig(workflow.Implementer, workflow.Reviewer)
		Expect(poll(cfg)).Error().NotTo(HaveOccurred())

		dashboard := theDashboardOf("o/r")
		h.comment(dashboard.Number, "o", "merge "+skillGroup.String()+" "+docsGroup.String())
		Expect(poll(cfg)).Error().NotTo(HaveOccurred())

		body := theDashboardOf("o/r").Body
		Expect(body).NotTo(ContainSubstring("merged by o"))

		state, err := workflow.ParseDashboard(body)
		Expect(err).NotTo(HaveOccurred())
		Expect(state.Actions).To(BeEmpty(), "recorded a merge across targets")
	})

	It("takes a Curator proposal with a writer's merge", func() {
		h.graphql(true)

		proposal := workflow.GroupRelation{
			ID:     "p1",
			Groups: []workflow.GroupID{skillGroup, {Target: skillGroup.Target, Key: "guide-skips-review"}},
			Reason: "one guide problem, split in two",
		}

		body, err := workflow.RenderDashboard(workflow.BuildDashboard(nil,
			workflow.DashboardState{Proposals: []workflow.GroupRelation{proposal}}, clock))
		Expect(err).NotTo(HaveOccurred())

		_, err = emulator.CreateIssueAs(ctx, "o/r", tokenFor(appLogin(workflow.Implementer)),
			nil, workflow.DashboardTitle, body)
		Expect(err).NotTo(HaveOccurred())

		dashboard := theDashboardOf("o/r")
		h.comment(dashboard.Number, "o", "merge "+skillGroup.String()+" "+proposal.Groups[1].String())
		Expect(poll(pollConfig(workflow.Implementer, workflow.Reviewer))).Error().NotTo(HaveOccurred())

		drawn := theDashboardOf("o/r").Body
		Expect(drawn).To(ContainSubstring("## Proposed merges (0)"), "kept a proposal a writer merged")
		Expect(drawn).To(ContainSubstring("merged by o"))
	})

	It("refuses a Curator proposal for good", func() {
		h.graphql(true)

		proposal := workflow.GroupRelation{
			ID:     "p1",
			Groups: []workflow.GroupID{skillGroup, {Target: skillGroup.Target, Key: "guide-skips-review"}},
			Reason: "one guide problem, split in two",
		}

		body, err := workflow.RenderDashboard(workflow.BuildDashboard(nil,
			workflow.DashboardState{Proposals: []workflow.GroupRelation{proposal}}, clock))
		Expect(err).NotTo(HaveOccurred())

		_, err = emulator.CreateIssueAs(ctx, "o/r", tokenFor(appLogin(workflow.Implementer)),
			nil, workflow.DashboardTitle, body)
		Expect(err).NotTo(HaveOccurred())

		dashboard := theDashboardOf("o/r")
		h.comment(dashboard.Number, "o", "reject p1")
		Expect(poll(pollConfig(workflow.Implementer, workflow.Reviewer))).Error().NotTo(HaveOccurred())

		drawn := theDashboardOf("o/r").Body
		Expect(drawn).To(ContainSubstring("## Proposed merges (0)"), "kept a rejected proposal")

		state, err := workflow.ParseDashboard(drawn)
		Expect(err).NotTo(HaveOccurred())
		Expect(state.Rejected).To(HaveLen(1))
		Expect(state.Rejected[0].ID).To(Equal("p1"))
	})

	It("does not report a command applied when it cannot be recorded as handled", func() {
		r := &commandRun{
			d: h.deps, cfg: pollConfig(workflow.Implementer, workflow.Reviewer),
			c: h.deps.client("minted-token-implementer"), perms: newPermissions(),
			repo: testRepo("o/r"), role: workflow.Implementer,
		}
		comment := workflow.Item{
			ID: 42, Author: "stranger", Body: "promote " + skillGroup.String(),
		}
		state := workflow.DashboardState{Handled: []int64{comment.ID}}

		next, done := r.onComment(ctx, state, nil, 1, comment)
		Expect(done).To(BeFalse(), "settled a command it could not record")
		Expect(next.Handled).To(ConsistOf(comment.ID))
	})

	It("leaves a promoted issue inert until a writer comments", func() {
		h.graphql(true)

		dashboard := ready(docsGroup, pollConfig(workflow.Implementer, workflow.Reviewer))

		h.comment(dashboard.Number, "o", "promote "+docsGroup.String())
		Expect(poll(pollConfig(workflow.Implementer, workflow.Reviewer))).Error().NotTo(HaveOccurred())

		filed := promotedIn("o/r", docsGroup)
		Expect(filed).To(HaveLen(1))
		Expect(h.factoryLabels(filed[0].Number)).To(BeEmpty(), "routed a promoted issue nobody commented on")

		h.comment(filed[0].Number, "o", "Let's do it")
		Expect(poll(pollConfig(workflow.Implementer, workflow.Reviewer))).Error().NotTo(HaveOccurred())
		Expect(h.factoryLabels(filed[0].Number)).To(ContainElement(workflow.LabelPlan),
			"a writer's comment did not route the promoted issue")
	})
})
