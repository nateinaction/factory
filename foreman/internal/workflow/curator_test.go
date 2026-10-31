package workflow_test

import (
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/nateinaction/factory/foreman/internal/workflow"
)

// curator groups the curator specs work with.
var (
	skillTarget = workflow.RetroTarget{Kind: workflow.TargetSkill, Name: "implement"} //nolint:gochecknoglobals
	docsTarget  = workflow.RetroTarget{Kind: workflow.TargetDocs, Name: "AGENTS.md"}  //nolint:gochecknoglobals

	guide  = workflow.GroupID{Target: skillTarget, Key: "guide-misses-tests"} //nolint:gochecknoglobals
	review = workflow.GroupID{Target: skillTarget, Key: "guide-skips-review"} //nolint:gochecknoglobals
	agents = workflow.GroupID{Target: docsTarget, Key: "no-test-command"}     //nolint:gochecknoglobals
)

var _ = Describe("curator-output.json", func() {
	// doc is an output file from its three raw lists.
	doc := func(merges, links, issues string) string {
		return `{"version":1,"merges":[` + merges + `],"links":[` + links + `],"issues":[` + issues + `]}`
	}

	// proposal is one merge or link entry.
	proposal := func(id string, a, b workflow.GroupID) string {
		return `{"id":"` + id + `","groups":["` + a.String() + `","` + b.String() + `"],"reason":"r"}`
	}

	// draft is one issue entry for agents with the given extra fields.
	draft := func(fields string) string {
		return `{"group":"` + agents.String() + `",` + fields + `}`
	}

	It("reads merge proposals within one target, links across targets and issue drafts", func() {
		out, err := workflow.ParseCuratorOutput(doc(
			`{"id":"m1","groups":["`+guide.String()+`","`+review.String()+`"],"reason":"one guide problem"}`,
			`{"id":"l1","groups":["`+guide.String()+`","`+agents.String()+`"],"reason":"the docs disagree"}`,
			draft(`"title":"Add a test command","body":"Say how to run tests."`),
		))
		Expect(err).NotTo(HaveOccurred())

		Expect(out.Merges).To(HaveLen(1))
		Expect(out.Merges[0].ID).To(Equal("m1"))
		Expect(out.Merges[0].Groups).To(Equal([]workflow.GroupID{guide, review}))
		Expect(out.Merges[0].Reason).To(Equal("one guide problem"))

		Expect(out.Links).To(HaveLen(1))
		Expect(out.Links[0].Groups).To(Equal([]workflow.GroupID{guide, agents}))

		Expect(out.Issues).To(HaveLen(1))
		Expect(out.Issues[0].Group).To(Equal(agents))
		Expect(out.Issues[0].Title).To(Equal("Add a test command"))
		Expect(out.Issues[0].Body).To(Equal("Say how to run tests."))
	})

	It("reads a private draft with the lesson and the evidence apart", func() {
		out, err := workflow.ParseCuratorOutput(doc("", "",
			draft(`"title":"Add a test command","lesson":"Say how to run tests.",`+
				`"private_evidence":"the session showed a secret"`)))
		Expect(err).NotTo(HaveOccurred())

		Expect(out.Issues[0].Lesson).To(Equal("Say how to run tests."))
		Expect(out.Issues[0].PrivateEvidence).To(Equal("the session showed a secret"))
	})

	It("refuses what the Curator may not propose, and names every problem", func() {
		for _, text := range []string{
			// a merge across targets
			doc(proposal("m1", guide, agents), "", ""),
			// a link within one target
			`{"version":1,"merges":[],"links":[` + proposal("l1", guide, review) + `],"issues":[]}`,
			// a merge naming one group twice
			doc(proposal("m1", guide, guide), "", ""),
			// a repeated proposal id
			`{"version":1,"merges":[` + proposal("p1", guide, review) + `],` +
				`"links":[` + proposal("p1", guide, agents) + `],"issues":[]}`,
			// no version
			`{"merges":[],"links":[],"issues":[]}`,
			// a repeated member
			`{"version":1,"version":1,"merges":[],"links":[],"issues":[]}`,
			// trailing data
			`{"version":1,"merges":[],"links":[],"issues":[]}{}`,
			// an unknown field
			`{"version":1,"merges":[],"links":[],"issues":[],"splits":[]}`,
			// a factory marker in the model's text
			doc("", "", draft(`"title":"t","body":"<!-- factory:handoff -->"`)),
			// a draft with both a public body and a private lesson
			doc("", "", draft(`"title":"t","body":"b","lesson":"l"`)),
			// evidence without a lesson
			doc("", "", draft(`"title":"t","body":"b","private_evidence":"e"`)),
			// two drafts for one group
			doc("", "", draft(`"title":"t","body":"b"`)+`,`+draft(`"title":"u","body":"c"`)),
			// a multi-line title
			doc("", "", draft(`"title":"t\nu","body":"b"`)),
		} {
			_, err := workflow.ParseCuratorOutput(text)
			Expect(err).To(HaveOccurred(), text)
		}
	})

	It("returns an output error naming its file", func() {
		_, err := workflow.ParseCuratorOutput("")
		Expect(err).To(BeAssignableToTypeOf(&workflow.OutputError{}))
		Expect(err.Error()).To(ContainSubstring(workflow.CuratorOutputFile))
	})
})

var _ = Describe("planning a Curator run", func() {
	now := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)

	report := func(issue int, group workflow.GroupID, severity workflow.Severity) workflow.DashboardReport {
		return workflow.DashboardReport{
			Issue: issue, Role: workflow.Implementer, Key: "sha-a", At: now,
			Target: group.Target, Group: group.Key, Severity: severity,
			Claim: "one claim", Evidence: "the session showed it",
		}
	}

	// readyReports make guide and agents ready, and leave review watching.
	readyReports := func() []workflow.DashboardReport {
		return []workflow.DashboardReport{
			report(12, guide, workflow.SeverityHigh),
			report(13, agents, workflow.SeverityHigh),
			report(14, review, workflow.SeverityLow),
		}
	}

	proposal := func(id string, groups ...workflow.GroupID) workflow.GroupRelation {
		return workflow.GroupRelation{ID: id, Groups: groups, Reason: "one problem"}
	}

	It("records merge proposals and links in the dashboard state", func() {
		out := workflow.CuratorOutput{
			Merges: []workflow.GroupRelation{proposal("m1", guide, review)},
			Links: []workflow.GroupRelation{
				{ID: "l1", Groups: []workflow.GroupID{guide, agents}, Reason: "the docs disagree"},
			},
		}

		plan := workflow.PlanCurator(out, readyReports(), workflow.DashboardState{}, false)

		Expect(plan.State.Proposals).To(HaveLen(1))
		Expect(plan.State.Proposals[0].ID).To(Equal("m1"))
		Expect(plan.State.Proposals[0].Groups).To(Equal([]workflow.GroupID{guide, review}))
		Expect(plan.State.Links).To(HaveLen(1))
		Expect(plan.State.Links[0].ID).To(Equal("l1"))
	})

	It("never proposes a rejected pair again, whatever its id", func() {
		state := workflow.DashboardState{Rejected: []workflow.DashboardRejection{{
			ID: "old", Groups: []workflow.GroupID{guide, review}, By: "o", At: now,
		}}}

		plan := workflow.PlanCurator(workflow.CuratorOutput{
			Merges: []workflow.GroupRelation{proposal("new", guide, review)},
		}, readyReports(), state, false)

		Expect(plan.State.Proposals).To(BeEmpty())
	})

	It("never touches a group a writer merged", func() {
		state := workflow.DashboardState{Actions: []workflow.DashboardAction{{
			Kind: workflow.ActionMerge, Groups: []workflow.GroupID{guide, review}, By: "o", At: now,
		}}}

		plan := workflow.PlanCurator(workflow.CuratorOutput{
			Merges: []workflow.GroupRelation{proposal("m1", guide, agents)},
			Links:  []workflow.GroupRelation{{ID: "l1", Groups: []workflow.GroupID{review, agents}, Reason: "r"}},
		}, readyReports(), state, false)

		Expect(plan.State.Proposals).To(BeEmpty())
		Expect(plan.State.Links).To(BeEmpty())
	})

	It("promotes a ready docs group in a private repository, and no skill group", func() {
		out := workflow.CuratorOutput{Issues: []workflow.CuratorIssueDraft{
			{Group: agents, Title: "Add a test command", Body: "Say how to run tests."},
			{Group: guide, Title: "Fix the guide", Body: "Mention tests."},
		}}

		plan := workflow.PlanCurator(out, readyReports(), workflow.DashboardState{}, true)

		Expect(plan.Issues).To(HaveLen(1))
		Expect(plan.Issues[0].Group).To(Equal(agents))
	})

	It("promotes a ready public skill group, but never a watching one", func() {
		out := workflow.CuratorOutput{Issues: []workflow.CuratorIssueDraft{
			{Group: guide, Title: "Fix the guide", Body: "Mention tests."},
			{Group: review, Title: "Too early", Body: "Not ready."},
		}}

		plan := workflow.PlanCurator(out, readyReports(), workflow.DashboardState{}, false)

		Expect(plan.Issues).To(HaveLen(1))
		Expect(plan.Issues[0].Group).To(Equal(guide))
	})

	It("keeps a proposal and link the dashboard already shows, without repeating it", func() {
		state := workflow.DashboardState{
			Proposals: []workflow.GroupRelation{{ID: "m1", Groups: []workflow.GroupID{guide, review}, Reason: "old"}},
			Links:     []workflow.GroupRelation{{ID: "l1", Groups: []workflow.GroupID{guide, agents}, Reason: "old"}},
		}

		plan := workflow.PlanCurator(workflow.CuratorOutput{
			Merges: []workflow.GroupRelation{proposal("m1", guide, review)},
			Links:  []workflow.GroupRelation{{ID: "l1", Groups: []workflow.GroupID{guide, agents}, Reason: "the docs disagree"}},
		}, readyReports(), state, false)

		Expect(plan.State.Proposals).To(HaveLen(1))
		Expect(plan.State.Proposals[0].Reason).To(Equal("one problem"))
		Expect(plan.State.Links).To(HaveLen(1))
		Expect(plan.State.Links[0].Reason).To(Equal("the docs disagree"))
	})
})

var _ = Describe("when the Curator is due", func() {
	now := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
	ttl := 40 * time.Minute

	reportAt := func(at time.Time) []workflow.DashboardReport {
		return []workflow.DashboardReport{{At: at, Target: skillTarget, Group: guide.Key}}
	}

	It("starts on a retro that added findings", func() {
		Expect(workflow.CuratorDue(reportAt(now.Add(-time.Hour)), workflow.DashboardState{}, now, ttl)).
			To(BeTrue())
	})

	It("does not start when nothing new arrived", func() {
		Expect(workflow.CuratorDue(nil, workflow.DashboardState{}, now, ttl)).To(BeFalse())

		state := workflow.DashboardState{Curator: workflow.DashboardCurator{LastRun: now.Add(-time.Hour)}}
		Expect(workflow.CuratorDue(reportAt(now.Add(-2*time.Hour)), state, now, ttl)).To(BeFalse())
	})

	It("starts on a writer's curate, even with no new report", func() {
		state := workflow.DashboardState{Actions: []workflow.DashboardAction{{
			Kind: workflow.ActionCurate, By: "o", At: now.Add(-time.Minute),
		}}}

		Expect(workflow.CuratorDue(nil, state, now, ttl)).To(BeTrue())
	})

	It("refuses a second curate on the same day as the last run", func() {
		state := workflow.DashboardState{
			Curator: workflow.DashboardCurator{LastRun: now.Add(-time.Hour)},
			Actions: []workflow.DashboardAction{
				{Kind: workflow.ActionCurate, By: "o", At: now.Add(-2 * time.Hour)},
				{Kind: workflow.ActionCurate, By: "o", At: now.Add(-time.Minute)},
			},
		}

		Expect(workflow.CuratorDue(nil, state, now, ttl)).To(BeFalse())

		// Once the day is over, the same request starts a run.
		Expect(workflow.CuratorDue(nil, state, now.Add(workflow.CuratorDay), ttl)).To(BeTrue())
	})

	It("starts when a writer's merge makes a group ready, with no new report", func() {
		state := workflow.DashboardState{
			Actions: []workflow.DashboardAction{{
				Kind: workflow.ActionMerge, Groups: []workflow.GroupID{guide, review}, By: "o",
				At: now.Add(-time.Minute),
			}},
			Curator: workflow.DashboardCurator{LastRun: now.Add(-25 * time.Hour)},
		}

		reports := []workflow.DashboardReport{
			{
				Issue: 12, At: now.Add(-2 * time.Hour), Target: guide.Target, Group: review.Key,
				Severity: workflow.SeverityMedium,
			},
			{
				Issue: 13, At: now.Add(-2 * time.Hour), Target: guide.Target, Group: guide.Key,
				Severity: workflow.SeverityMedium,
			},
		}

		Expect(workflow.CuratorDue(reports, state, now, ttl)).To(BeTrue())
	})

	It("runs at most once a day and one at a time", func() {
		state := workflow.DashboardState{Curator: workflow.DashboardCurator{LastRun: now.Add(-23 * time.Hour)}}
		Expect(workflow.CuratorDue(reportAt(now.Add(-time.Hour)), state, now, ttl)).To(BeFalse())

		state = workflow.DashboardState{Curator: workflow.DashboardCurator{LastRun: now.Add(-25 * time.Hour)}}
		Expect(workflow.CuratorDue(reportAt(now.Add(-time.Hour)), state, now, ttl)).To(BeTrue())

		claimed := workflow.DashboardState{Curator: workflow.DashboardCurator{Claim: now.Add(-time.Minute)}}
		Expect(workflow.CuratorDue(reportAt(now.Add(-time.Hour)), claimed, now, ttl)).To(BeFalse())

		stale := workflow.DashboardState{Curator: workflow.DashboardCurator{Claim: now.Add(-time.Hour)}}
		Expect(workflow.CuratorDue(reportAt(now.Add(-2*time.Hour)), stale, now, ttl)).To(BeTrue())
	})
})

var _ = Describe("a promoted issue a Curator drafts", func() {
	It("keeps a private draft's lesson and evidence in separate sections", func() {
		title, body, err := workflow.RenderCuratorDraft(agents, workflow.CuratorIssueDraft{
			Group: agents, Title: "Add a test command",
			Lesson: "Say how to run tests.", PrivateEvidence: "a private repository detail",
		}, true)
		Expect(err).NotTo(HaveOccurred())

		Expect(title).To(Equal("Add a test command"))
		Expect(body).To(HavePrefix(workflow.PromotionMarker(agents)))
		Expect(body).To(ContainSubstring("## Lesson"))
		Expect(body).To(ContainSubstring("Say how to run tests."))
		Expect(body).To(ContainSubstring("## Private evidence"))
		Expect(body).To(ContainSubstring("a private repository detail"))

		lesson, ok := workflow.LessonOf(body)
		Expect(ok).To(BeTrue())
		Expect(lesson).To(ContainSubstring("Say how to run tests."))
		Expect(lesson).NotTo(ContainSubstring("private repository detail"))
	})

	It("writes a public draft's body with no private section", func() {
		_, body, err := workflow.RenderCuratorDraft(guide, workflow.CuratorIssueDraft{
			Group: guide, Title: "Fix the guide", Body: "Mention tests.",
		}, false)
		Expect(err).NotTo(HaveOccurred())

		Expect(body).To(HavePrefix(workflow.PromotionMarker(guide)))
		Expect(body).To(ContainSubstring("Mention tests."))
		Expect(body).NotTo(ContainSubstring("## Private evidence"))
	})
})

var _ = Describe("the Curator's brief", func() {
	It("carries the retro log with evidence, the writer actions and the rejections", func() {
		now := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
		reports := []workflow.DashboardReport{{
			Issue: 12, Role: workflow.Implementer, Key: "sha-a", At: now,
			Target: skillTarget, Group: guide.Key, Severity: workflow.SeverityHigh,
			Claim: "the guide never mentions tests", Evidence: "searched and found none",
		}}
		state := workflow.DashboardState{
			Actions: []workflow.DashboardAction{{
				Kind: workflow.ActionMerge, Groups: []workflow.GroupID{guide, review}, By: "o", At: now,
			}},
			Rejected: []workflow.DashboardRejection{{
				ID: "p1", Groups: []workflow.GroupID{guide, review}, By: "o", At: now,
			}},
		}

		brief := workflow.CuratorBrief(
			workflow.IssueRef{Repo: mustRepo("o/r"), Number: 7}, true, reports, workflow.BuildDashboard(reports, state, now))

		Expect(brief).To(ContainSubstring("o/r"))
		Expect(brief).To(ContainSubstring("#7"))
		Expect(brief).To(ContainSubstring("private"))
		Expect(brief).To(ContainSubstring("searched and found none"))
		Expect(brief).To(ContainSubstring("merge `" + guide.String() + "` and `" + review.String() + "`"))
		Expect(brief).To(ContainSubstring("`p1`"))
		Expect(strings.Count(brief, guide.String())).To(BeNumerically(">", 1))
	})
})
