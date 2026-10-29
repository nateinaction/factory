package foreman

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/nateinaction/factory/foreman/internal/ghtest"
	"github.com/nateinaction/factory/foreman/internal/workflow"
)

// dashboardIssue is the part of an emulator issue the dashboard specs read back.
type dashboardIssue struct {
	Number int    `json:"number"`
	Title  string `json:"title"`
	Body   string `json:"body"`
	State  string `json:"state"`
	User   struct {
		Login string `json:"login"`
	} `json:"user"`
}

var _ = Describe("the retro dashboard", func() {
	var h *harness

	BeforeEach(func() {
		h = newHarness()
	})

	// poll polls the Implementer's and the Reviewer's queues.
	poll := func() (int, error) {
		return Poll(context.Background(), h.deps, pollConfig(workflow.Implementer, workflow.Reviewer))
	}

	// issues lists every issue of o/r, open or closed.
	issues := func() []dashboardIssue {
		GinkgoHelper()

		var out []dashboardIssue

		err := emulator.Do(context.Background(), http.MethodGet,
			"/repos/o/r/issues?state=all&per_page=100", ghtest.AdminToken, nil, &out)
		Expect(err).NotTo(HaveOccurred())

		return out
	}

	// dashboards lists the issues titled like the dashboard.
	dashboards := func() []dashboardIssue {
		GinkgoHelper()

		var out []dashboardIssue

		for _, issue := range issues() {
			if issue.Title == workflow.DashboardTitle {
				out = append(out, issue)
			}
		}

		return out
	}

	// theDashboard is the repository's one dashboard.
	theDashboard := func() dashboardIssue {
		GinkgoHelper()

		found := dashboards()
		Expect(found).To(HaveLen(1), "want exactly one dashboard")

		return found[0]
	}

	// patches are the body writes foreman made to the dashboard.
	patches := func() []string {
		GinkgoHelper()

		return h.gh.Bodies("PATCH", fmt.Sprintf("/repos/o/r/issues/%d", theDashboard().Number))
	}

	// seedRetro posts a retro comment on number as the agent role's App.
	seedRetro := func(number int, role workflow.Role, key string, findings ...workflow.RetroFinding) {
		GinkgoHelper()

		body, err := workflow.RenderRetro(workflow.RetroMarker(number, role, key), findings)
		Expect(err).NotTo(HaveOccurred())
		h.comment(number, appLogin(role), body)
	}

	// openAs opens an issue titled title with body as login, returning its number.
	openAs := func(login, title, body string) int {
		GinkgoHelper()

		number, err := emulator.CreateIssueAs(context.Background(), "o/r", tokenFor(login), nil, title, body)
		Expect(err).NotTo(HaveOccurred())

		return number
	}

	// dashboardBody is a readable dashboard body with no reports.
	dashboardBody := func() string {
		GinkgoHelper()

		body, err := workflow.RenderDashboard(
			workflow.BuildDashboard(nil, workflow.DashboardState{}, time.Now()))
		Expect(err).NotTo(HaveOccurred())

		return body
	}

	// finding is one retro finding about the Implementer's skill.
	finding := func(severity workflow.Severity, claim string) workflow.RetroFinding {
		return workflow.RetroFinding{
			Target:   workflow.RetroTarget{Kind: workflow.TargetSkill, Name: "implement"},
			Group:    "guide-misses-tests",
			Severity: severity,
			Claim:    claim,
			Evidence: "the session showed it",
		}
	}

	It("creates exactly one dashboard issue per repository, idempotently", func() {
		Expect(poll()).Error().NotTo(HaveOccurred())

		dashboard := theDashboard()
		Expect(dashboard.State).To(Equal("open"))
		Expect(dashboard.Body).To(HavePrefix(workflow.DashboardMarker))
		Expect(dashboard.Body).To(ContainSubstring("## Ready to promote (0)"))

		Expect(poll()).Error().NotTo(HaveOccurred())
		Expect(dashboards()).To(HaveLen(1), "created a second dashboard")
	})

	It("ignores a Retro dashboard an outsider opened and draws its own", func() {
		body := dashboardBody()
		outsider := openAs("factory", workflow.DashboardTitle, body)

		Expect(poll()).Error().NotTo(HaveOccurred())

		theirs, err := emulator.Issue(context.Background(), "o/r", outsider)
		Expect(err).NotTo(HaveOccurred())
		Expect(theirs.Body).To(Equal(body), "an outsider's dashboard was rewritten")

		factory := make([]dashboardIssue, 0, 1)

		for _, issue := range dashboards() {
			if workflow.FactoryAuthored(issue.User.Login) {
				factory = append(factory, issue)
			}
		}

		Expect(factory).To(HaveLen(1), "the factory's own dashboard")
		Expect(factory[0].Number).NotTo(Equal(outsider))
		Expect(factory[0].Body).To(ContainSubstring("## Ready to promote (0)"))
	})

	It("leaves retro reports an outsider wrote off the dashboard", func() {
		issue := h.createAs("o", nil, "An issue")

		body, err := workflow.RenderRetro(
			workflow.RetroMarker(issue, workflow.Implementer, "sha-a"),
			[]workflow.RetroFinding{finding(workflow.SeverityHigh, "an outsider's claim")})
		Expect(err).NotTo(HaveOccurred())
		h.comment(issue, "factory", body)

		Expect(poll()).Error().NotTo(HaveOccurred())

		drawn := theDashboard().Body
		Expect(drawn).To(ContainSubstring("## Ready to promote (0)"))
		Expect(drawn).NotTo(ContainSubstring("an outsider's claim"))
	})

	It("redraws the body from the retro log, and writes only when it changes", func() {
		first := h.createAs("o", nil, "First")
		second := h.createAs("o", nil, "Second")

		seedRetro(first, workflow.Implementer, "sha-a", finding(workflow.SeverityMedium, "no tests"))
		seedRetro(second, workflow.Reviewer, "sha-b", finding(workflow.SeverityLow, "no tests either"))

		Expect(poll()).Error().NotTo(HaveOccurred())

		body := theDashboard().Body
		Expect(body).To(ContainSubstring("## Ready to promote (1)"))
		Expect(body).To(ContainSubstring("2 reports from 2 issues"))
		Expect(body).To(ContainSubstring("implementer run `sha-a` on #"))
		Expect(body).To(ContainSubstring("reviewer run `sha-b` on #"))
		Expect(patches()).To(BeEmpty(), "wrote a body that had not changed")

		Expect(poll()).Error().NotTo(HaveOccurred())
		Expect(patches()).To(BeEmpty(), "wrote the same body again")

		third := h.createAs("o", nil, "Third")
		seedRetro(third, workflow.Implementer, "sha-c", finding(workflow.SeverityMedium, "still no tests"))

		Expect(poll()).Error().NotTo(HaveOccurred())
		Expect(patches()).To(HaveLen(1), "did not write the changed body")
		Expect(theDashboard().Body).To(ContainSubstring("3 reports from 3 issues"))
	})

	It("never routes the dashboard and never comments on it", func() {
		issue := h.createAs("o", nil, "An issue")
		seedRetro(issue, workflow.Implementer, "sha-a", finding(workflow.SeverityHigh, "no tests"))

		Expect(poll()).Error().NotTo(HaveOccurred())

		number := theDashboard().Number
		Expect(h.factoryLabels(number)).To(BeEmpty(), "routed the dashboard")
		Expect(h.gh.Bodies("POST", fmt.Sprintf("/repos/o/r/issues/%d/comments", number))).To(BeEmpty(),
			"commented on the dashboard")
	})

	It("counts issues, not agent roles, for readiness", func() {
		issue := h.createAs("o", nil, "An issue")

		seedRetro(issue, workflow.Implementer, "sha-a", finding(workflow.SeverityMedium, "no tests"))
		seedRetro(issue, workflow.Reviewer, "sha-b", finding(workflow.SeverityMedium, "no tests"))

		Expect(poll()).Error().NotTo(HaveOccurred())

		body := theDashboard().Body
		Expect(body).To(ContainSubstring("## Ready to promote (0)"))
		Expect(body).To(ContainSubstring("## Watching (1)"))
	})

	It("readies a group after one high-severity report", func() {
		issue := h.createAs("o", nil, "An issue")
		seedRetro(issue, workflow.Implementer, "sha-a", finding(workflow.SeverityHigh, "no tests"))

		Expect(poll()).Error().NotTo(HaveOccurred())

		body := theDashboard().Body
		Expect(body).To(ContainSubstring("## Ready to promote (1)"))
		Expect(body).To(ContainSubstring("## Watching (0)"))
	})

	It("drops a Watching group after 60 days with no repeat", func() {
		issue := h.createAs("o", nil, "An issue")
		seedRetro(issue, workflow.Implementer, "sha-a", finding(workflow.SeverityMedium, "no tests"))

		h.deps.Now = func() time.Time { return time.Now().Add(59 * 24 * time.Hour) }

		Expect(poll()).Error().NotTo(HaveOccurred())
		Expect(theDashboard().Body).To(ContainSubstring("## Watching (1)"))

		h.deps.Now = func() time.Time { return time.Now().Add(61 * 24 * time.Hour) }

		Expect(poll()).Error().NotTo(HaveOccurred())
		Expect(theDashboard().Body).To(ContainSubstring("## Watching (0)"))
		Expect(patches()).To(HaveLen(1), "kept the expired group")
	})

	It("shows a promoted group a later report mentions as Regressed", func() {
		group := workflow.GroupID{
			Target: workflow.RetroTarget{Kind: workflow.TargetSkill, Name: "implement"},
			Key:    "guide-misses-tests",
		}
		action := workflow.DashboardAction{
			Kind:   workflow.ActionPromote,
			Groups: []workflow.GroupID{group},
			By:     "o",
			At:     time.Now().Add(-time.Hour),
			URL:    "https://github.com/o/r/issues/2",
		}

		body, err := workflow.RenderDashboard(
			workflow.BuildDashboard(nil, workflow.DashboardState{Actions: []workflow.DashboardAction{action}}, time.Now()))
		Expect(err).NotTo(HaveOccurred())

		_, err = emulator.CreateIssueAs(context.Background(), "o/r", tokenFor(appLogin(workflow.Implementer)),
			nil, workflow.DashboardTitle, body)
		Expect(err).NotTo(HaveOccurred())

		issue := h.createAs("o", nil, "An issue")
		seedRetro(issue, workflow.Reviewer, "sha-b", finding(workflow.SeverityHigh, "came back"))

		Expect(poll()).Error().NotTo(HaveOccurred())

		drawn := theDashboard().Body
		Expect(drawn).To(ContainSubstring("## Regressed (1)"))
		Expect(drawn).To(ContainSubstring("promoted by o"))
	})

	It("leaves a dashboard body it cannot read alone", func() {
		openAs(appLogin(workflow.Implementer), workflow.DashboardTitle,
			"a body a person rewrote by hand")

		Expect(poll()).Error().NotTo(HaveOccurred())
		Expect(theDashboard().Body).To(Equal("a body a person rewrote by hand"))
		Expect(h.logs.String()).To(ContainSubstring("cannot redraw the retro dashboard"))
		Expect(strings.Join(patches(), "")).To(BeEmpty())
	})
})
