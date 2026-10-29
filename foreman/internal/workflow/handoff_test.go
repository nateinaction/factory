package workflow_test

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/nateinaction/factory/foreman/internal/workflow"
)

var _ = Describe("the handoff label table", func() {
	DescribeTable("gives every agent role a queued and a running label",
		func(role workflow.Role, queued, running workflow.State) {
			got, ok := workflow.Queued(role)
			Expect(ok).To(BeTrue())
			Expect(got).To(Equal(queued))

			owner, ok := workflow.RoleOf(running)
			Expect(ok).To(BeTrue())
			Expect(owner).To(Equal(role))
		},
		Entry(nil, workflow.Planner, workflow.Plan, workflow.Planning),
		Entry(nil, workflow.Researcher, workflow.Research, workflow.Researching),
		Entry(nil, workflow.Implementer, workflow.Implement, workflow.Implementing),
		Entry(nil, workflow.Reviewer, workflow.Review, workflow.Reviewing),
	)

	It("gives the Curator no queue", func() {
		_, ok := workflow.Queued(workflow.Curator)
		Expect(ok).To(BeFalse())
	})

	DescribeTable("names the agent role that owns a state",
		func(state string, role workflow.Role) {
			got, ok := workflow.RoleOf(workflow.State(state))
			Expect(ok).To(BeTrue())
			Expect(got).To(Equal(role))
		},
		Entry(nil, "factory:plan", workflow.Planner),
		Entry(nil, "factory:planning", workflow.Planner),
		Entry(nil, "factory:research", workflow.Researcher),
		Entry(nil, "factory:researching", workflow.Researcher),
		Entry(nil, "factory:implement", workflow.Implementer),
		Entry(nil, "factory:implementing", workflow.Implementer),
		Entry(nil, "factory:review", workflow.Reviewer),
		Entry(nil, "factory:reviewing", workflow.Reviewer),
	)

	DescribeTable("parses only the known states",
		func(name string, ok bool) {
			_, err := workflow.ParseState(name)
			if ok {
				Expect(err).NotTo(HaveOccurred())
			} else {
				Expect(err).To(HaveOccurred())
			}
		},
		Entry(nil, "factory:plan", true),
		Entry(nil, "factory:paused", true),
		Entry(nil, "factory:done", true),
		Entry(nil, "closed", true),
		Entry(nil, "", false),
		Entry(nil, "plan", false),
		Entry(nil, "factory:nope", false),
		Entry(nil, "factory:planning ", false),
	)

	It("lists every factory label foreman creates, and only factory: labels", func() {
		Expect(workflow.FactoryLabels()).To(ConsistOf(
			"factory:plan", "factory:planning", "factory:research", "factory:researching",
			"factory:implement", "factory:implementing", "factory:review", "factory:reviewing",
			"factory:waiting", "factory:done", "factory:failed", "factory:paused",
		))
	})

	It("gives a closed ticket no label", func() {
		Expect(workflow.Closed).To(Equal(workflow.State("closed")))
		Expect(workflow.FactoryLabels()).NotTo(ContainElement("closed"))
	})
})

var _ = Describe("transition markers", func() {
	transition := workflow.Transition{
		Role: workflow.Implementer, RunID: "run-1",
		From: workflow.Implement, To: workflow.Implementing, SHA: "abc123",
	}

	It("renders the agent role, run id, from-state, to-state and commit SHA", func() {
		body, err := workflow.RenderTransition(transition)
		Expect(err).NotTo(HaveOccurred())
		Expect(body).To(Equal(
			`<!-- factory:handoff role=implementer run=run-1 from=factory:implement ` +
				`to=factory:implementing sha=abc123 -->`))
	})

	It("round-trips through its own parser", func() {
		body, err := workflow.RenderTransition(transition)
		Expect(err).NotTo(HaveOccurred())

		got, found, err := workflow.ParseTransition(body)
		Expect(err).NotTo(HaveOccurred())
		Expect(found).To(BeTrue())
		Expect(got).To(Equal(transition))
	})

	It("parses a marker at the top of a longer comment", func() {
		body, err := workflow.RenderTransition(transition)
		Expect(err).NotTo(HaveOccurred())

		got, found, err := workflow.ParseTransition(body + "\n\nThe agent finished.")
		Expect(err).NotTo(HaveOccurred())
		Expect(found).To(BeTrue())
		Expect(got).To(Equal(transition))
	})

	It("is not a marker when the comment does not start with one", func() {
		_, found, err := workflow.ParseTransition("A human comment\n<!-- factory:handoff -->")
		Expect(err).NotTo(HaveOccurred())
		Expect(found).To(BeFalse())
	})

	// marker wraps a fields string as a handoff marker.
	marker := func(fields string) string { return "<!-- factory:handoff " + fields + " -->" }

	DescribeTable("fails closed on a malformed marker",
		func(body string) {
			_, found, err := workflow.ParseTransition(body)
			Expect(found).To(BeTrue())
			Expect(err).To(HaveOccurred())
		},
		Entry("not closed", "<!-- factory:handoff role=implementer"),
		Entry("no fields", "<!-- factory:handoff  -->"),
		Entry("unknown role", marker(
			"role=janitor run=r from=factory:implement to=factory:implementing sha=a")),
		Entry("unknown state", marker(
			"role=implementer run=r from=factory:nope to=factory:implementing sha=a")),
		Entry("missing run", marker(
			"role=implementer from=factory:implement to=factory:implementing sha=a")),
		Entry("missing from", marker(
			"role=implementer run=r to=factory:implementing sha=a")),
		Entry("missing to", marker(
			"role=implementer run=r from=factory:implement sha=a")),
		Entry("missing sha", marker(
			"role=implementer run=r from=factory:implement to=factory:implementing")),
		Entry("repeated key", marker(
			"role=implementer role=reviewer run=r from=factory:implement to=factory:implementing sha=a")),
		Entry("unknown key", marker(
			"role=implementer run=r from=factory:implement to=factory:implementing sha=a extra=1")),
		Entry("unquoted value", marker(
			`role=implementer run=r from=factory:implement to=factory:implementing sha="a b"`)),
	)

	// renderable is a transition for mutating one field.
	renderable := func() workflow.Transition {
		return workflow.Transition{
			Role: workflow.Implementer, RunID: "r", From: workflow.Implement, To: workflow.Implementing,
		}
	}

	DescribeTable("refuses to render a transition with an unstorable value",
		func(mutate func(*workflow.Transition)) {
			t := renderable()
			mutate(&t)

			_, err := workflow.RenderTransition(t)
			Expect(err).To(HaveOccurred())
		},
		Entry("unknown role", func(t *workflow.Transition) { t.Role = "janitor" }),
		Entry("unknown from", func(t *workflow.Transition) { t.From = "factory:nope" }),
		Entry("unknown to", func(t *workflow.Transition) { t.To = "factory:nope" }),
		Entry("empty run", func(t *workflow.Transition) { t.RunID = "" }),
		Entry("run with a space", func(t *workflow.Transition) { t.RunID = "a b" }),
		Entry("run with a newline", func(t *workflow.Transition) { t.RunID = "a\nb" }),
	)
})

var _ = Describe("the latest transition", func() {
	marker := func(role workflow.Role, run string, to workflow.State) string {
		body, err := workflow.RenderTransition(workflow.Transition{
			Role: role, RunID: run, From: workflow.Implement, To: to,
		})
		Expect(err).NotTo(HaveOccurred())

		return body
	}

	It("is the last marker in API order, ignoring comments that are not markers", func() {
		comments := []workflow.Item{
			{Body: marker(workflow.Implementer, "r1", workflow.Implementing)},
			{Body: "a human comment"},
			{Body: marker(workflow.Implementer, "r2", workflow.Review) + "\n\nopened"},
		}

		got, found, err := workflow.LatestMarker(comments)
		Expect(err).NotTo(HaveOccurred())
		Expect(found).To(BeTrue())
		Expect(got.Transition.RunID).To(Equal("r2"))
		Expect(got.Transition.To).To(Equal(workflow.Review))
	})

	It("finds none when no comment is a marker", func() {
		_, found, err := workflow.LatestMarker([]workflow.Item{{Body: "hello"}})
		Expect(err).NotTo(HaveOccurred())
		Expect(found).To(BeFalse())
	})

	It("fails closed on a malformed latest marker", func() {
		_, _, err := workflow.LatestMarker([]workflow.Item{{Body: "<!-- factory:handoff nope -->"}})
		Expect(err).To(HaveOccurred())
	})
})

var _ = Describe("recovery", func() {
	transition := workflow.Transition{
		Role: workflow.Implementer, RunID: "r", From: workflow.Implement, To: workflow.Implementing,
	}

	It("re-applies the state the latest marker names when the issue does not carry it", func() {
		issue := workflow.Issue{Labels: []string{"factory:implement"}}

		state, repair := workflow.Repair(issue, transition, true)
		Expect(repair).To(BeTrue())
		Expect(state).To(Equal(workflow.Implementing))
	})

	It("does nothing when the issue already carries that state", func() {
		issue := workflow.Issue{Labels: []string{"factory:implement", "factory:implementing"}}

		_, repair := workflow.Repair(issue, transition, true)
		Expect(repair).To(BeFalse())
	})

	It("does nothing without a marker or on a paused issue", func() {
		_, repair := workflow.Repair(workflow.Issue{}, transition, false)
		Expect(repair).To(BeFalse())

		paused := workflow.Issue{Labels: []string{"factory:paused"}}
		_, repair = workflow.Repair(paused, transition, true)
		Expect(repair).To(BeFalse())
	})

	It("does not apply a label for a closed ticket", func() {
		closed := workflow.Transition{
			Role: workflow.Researcher, RunID: "r", From: workflow.Researching, To: workflow.Closed,
		}

		_, repair := workflow.Repair(workflow.Issue{}, closed, true)
		Expect(repair).To(BeFalse())
	})
})

var _ = Describe("routing a new issue", func() {
	DescribeTable("sends a ticket to its first queue",
		func(labels []string, state workflow.State) {
			got, ok := workflow.Route(workflow.Issue{Labels: labels})
			Expect(ok).To(BeTrue())
			Expect(got).To(Equal(state))
		},
		Entry("research", []string{"wayfinder:research"}, workflow.Research),
		Entry("grilling", []string{"wayfinder:grilling"}, workflow.Plan),
		Entry("prototype", []string{"prototype"}, workflow.Plan),
		Entry("wayfinder prototype", []string{"wayfinder:prototype"}, workflow.Plan),
		Entry("task", []string{"task"}, workflow.Plan),
		Entry("anything else", []string{"bug"}, workflow.Plan),
		Entry("unlabelled", nil, workflow.Plan),
	)

	DescribeTable("never routes",
		func(issue workflow.Issue) {
			_, ok := workflow.Route(issue)
			Expect(ok).To(BeFalse())
		},
		Entry("a wayfinder map", workflow.Issue{Labels: []string{"wayfinder:map"}}),
		Entry("the retro dashboard", workflow.Issue{Title: workflow.DashboardTitle}),
		Entry("an issue already carrying a factory label", workflow.Issue{Labels: []string{"factory:implement"}}),
		Entry("a paused issue", workflow.Issue{Labels: []string{"factory:paused"}}),
		Entry("a pull request", workflow.Issue{IsPullRequest: true}),
	)

	It("recognises a factory App as a bot identity", func() {
		Expect(workflow.BotAuthored("nateinaction-factory-implementer[bot]")).To(BeTrue())
		Expect(workflow.BotAuthored("nateinaction")).To(BeFalse())
	})
})

var _ = Describe("writers", func() {
	DescribeTable("grants write access only to the permissions that have it",
		func(permission string, writer bool) {
			Expect(workflow.WriterPermission(permission)).To(Equal(writer))
		},
		Entry(nil, "admin", true),
		Entry(nil, "maintain", true),
		Entry(nil, "write", true),
		Entry(nil, "push", true),
		Entry(nil, "read", false),
		Entry(nil, "triage", false),
		Entry(nil, "none", false),
		Entry(nil, "", false),
		Entry(nil, "WRITE", false),
	)
})

var _ = Describe("the Blocked by fallback", func() {
	DescribeTable("reads the blocking issue numbers",
		func(body string, want []int) {
			Expect(workflow.BlockedBy(body)).To(Equal(want))
		},
		Entry(nil, "Blocked by: #12, #13", []int{12, 13}),
		Entry(nil, "# Title\n\nBlocked by: #7\n\nBody", []int{7}),
		Entry(nil, "Blocked by: #7 and #7", []int{7}),
		Entry(nil, "No blockers here", nil),
		Entry(nil, "Blocked by: nothing", nil),
		Entry(nil, "Blocked by: #notanumber", nil),
	)
})
