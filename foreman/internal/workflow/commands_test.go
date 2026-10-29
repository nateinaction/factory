package workflow_test

import (
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/nateinaction/factory/foreman/internal/workflow"
)

var _ = Describe("a dashboard command", func() {
	// group is "skill:implement/guide-misses-tests", the form the dashboard shows.
	group := workflow.GroupID{
		Target: workflow.RetroTarget{Kind: workflow.TargetSkill, Name: "implement"},
		Key:    "guide-misses-tests",
	}
	other := workflow.GroupID{Target: group.Target, Key: "guide-skips-review"}

	It("reads every command a writer may type", func() {
		cmd, ok, err := workflow.ParseCommand("promote " + group.String())
		Expect(err).NotTo(HaveOccurred())
		Expect(ok).To(BeTrue())
		Expect(cmd.Kind).To(Equal(workflow.CommandPromote))
		Expect(cmd.Groups).To(Equal([]workflow.GroupID{group}))

		cmd, ok, err = workflow.ParseCommand("merge " + group.String() + " " + other.String())
		Expect(err).NotTo(HaveOccurred())
		Expect(ok).To(BeTrue())
		Expect(cmd.Groups).To(Equal([]workflow.GroupID{group, other}))

		cmd, ok, err = workflow.ParseCommand("ignore " + group.String())
		Expect(err).NotTo(HaveOccurred())
		Expect(ok).To(BeTrue())
		Expect(cmd.Kind).To(Equal(workflow.CommandIgnore))

		cmd, ok, err = workflow.ParseCommand("reject p1")
		Expect(err).NotTo(HaveOccurred())
		Expect(ok).To(BeTrue())
		Expect(cmd.Proposal).To(Equal("p1"))

		cmd, ok, err = workflow.ParseCommand("publish")
		Expect(err).NotTo(HaveOccurred())
		Expect(ok).To(BeTrue())
		Expect(cmd.Kind).To(Equal(workflow.CommandPublish))

		cmd, ok, err = workflow.ParseCommand("curate")
		Expect(err).NotTo(HaveOccurred())
		Expect(ok).To(BeTrue())
		Expect(cmd.Kind).To(Equal(workflow.CommandCurate))
	})

	It("reads a command on the first non-blank line, and leaves prose alone", func() {
		cmd, ok, err := workflow.ParseCommand("\n\n  Promote " + group.String() + "\n\nbecause it repeats.\n")
		Expect(err).NotTo(HaveOccurred())
		Expect(ok).To(BeTrue())
		Expect(cmd.Kind).To(Equal(workflow.CommandPromote))

		_, ok, err = workflow.ParseCommand("This is ready to build.")
		Expect(err).NotTo(HaveOccurred())
		Expect(ok).To(BeFalse())

		_, ok, err = workflow.ParseCommand("")
		Expect(err).NotTo(HaveOccurred())
		Expect(ok).To(BeFalse())
	})

	It("reports a command word with no usable argument, and guesses at nothing", func() {
		for _, text := range []string{
			"promote", "promote nope", "merge " + group.String(),
			"reject", "reject not an id", "publish now", "curate now", "promote " + group.String() + " please",
		} {
			_, ok, err := workflow.ParseCommand(text)
			Expect(err).To(HaveOccurred(), text)
			Expect(ok).To(BeTrue(), text)
		}
	})

	It("reads a document group, whose target carries a slash of its own", func() {
		doc := workflow.GroupID{
			Target: workflow.RetroTarget{Kind: workflow.TargetDocs, Name: "docs/agents/triage.md"}, Key: "no-lab",
		}

		parsed, err := workflow.ParseGroupID(doc.String())
		Expect(err).NotTo(HaveOccurred())
		Expect(parsed).To(Equal(doc))
	})
})

var _ = Describe("writer commands change the dashboard", func() {
	now := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)

	skillTarget := workflow.RetroTarget{Kind: workflow.TargetSkill, Name: "implement"}

	group := workflow.GroupID{Target: skillTarget, Key: "guide-misses-tests"}
	other := workflow.GroupID{Target: skillTarget, Key: "guide-skips-review"}

	report := func(issue int, group workflow.GroupID, claim string) workflow.DashboardReport {
		return workflow.DashboardReport{
			Issue: issue, Role: workflow.Implementer, Key: "sha-a", At: now, Severity: workflow.SeverityMedium,
			Target: group.Target, Group: group.Key, Claim: claim, Evidence: "the session showed it",
		}
	}

	// merged is the action a writer's merge of two groups leaves.
	merged := workflow.DashboardAction{
		Kind: workflow.ActionMerge, Groups: []workflow.GroupID{group, other},
		By: "o", At: now.Add(time.Hour), URL: "https://github.com/o/r/issues/9#issuecomment-1",
	}

	It("counts a merged group's reports as one group's", func() {
		reports := []workflow.DashboardReport{
			report(12, other, "the guide skips review"),
			report(13, other, "the guide skips review too"),
		}

		view := workflow.BuildDashboard(reports, workflow.DashboardState{
			Actions: []workflow.DashboardAction{merged},
		}, now)

		Expect(view.Promoted).To(HaveLen(1))
		Expect(view.Promoted[0].ID()).To(Equal(group))
		Expect(view.Promoted[0].Reports).To(HaveLen(2), "folded the merged group's reports away")

		found, ok := workflow.FindGroup(reports, workflow.DashboardState{
			Actions: []workflow.DashboardAction{merged},
		}, other)
		Expect(ok).To(BeTrue(), "the merged group is still found by the key it was merged away")
		Expect(found.ID).To(Equal(group))
	})

	It("never shows a proposal a writer rejected, even under a new id", func() {
		state := workflow.DashboardState{
			Rejected: []workflow.DashboardRejection{{
				ID: "p1", Groups: []workflow.GroupID{group, other}, By: "o", At: now.Add(-time.Hour),
			}},
			Proposals: []workflow.GroupRelation{
				{ID: "p1", Groups: []workflow.GroupID{group, other}, Reason: "same problem"},
				{ID: "p2", Groups: []workflow.GroupID{other, group}, Reason: "same problem again"},
				{ID: "p3", Groups: []workflow.GroupID{group, {Target: skillTarget, Key: "third"}}, Reason: "another problem"},
			},
		}

		shown := workflow.BuildDashboard(nil, state, now).Proposals
		Expect(shown).To(HaveLen(1), "showed a rejected pair again")
		Expect(shown[0].ID).To(Equal("p3"))

		state.Rejected = nil
		Expect(workflow.BuildDashboard(nil, state, now).Proposals).To(HaveLen(3))
	})

	It("records a rejection and drops the proposal", func() {
		state := workflow.DashboardState{Proposals: []workflow.GroupRelation{
			{ID: "p1", Groups: []workflow.GroupID{group, other}, Reason: "same problem"},
			{ID: "p2", Groups: []workflow.GroupID{group}, Reason: "another problem"},
		}}

		rejected := state.WithRejection(workflow.DashboardRejection{
			ID: "p1", Groups: []workflow.GroupID{group, other}, By: "o", At: now,
		})

		Expect(rejected.Rejected).To(HaveLen(1))
		Expect(rejected.Proposals).To(HaveLen(1))
		Expect(rejected.Proposals[0].ID).To(Equal("p2"))
	})

	It("marks a handled comment once", func() {
		state := workflow.DashboardState{}

		state, ok := state.WithHandled(9)
		Expect(ok).To(BeTrue())
		Expect(state.HasHandled(9)).To(BeTrue())

		state, ok = state.WithHandled(9)
		Expect(ok).To(BeFalse())
		Expect(state.Handled).To(Equal([]int64{9}))
	})

	It("round-trips rejections, publications and handled comments", func() {
		state := workflow.DashboardState{
			Rejected: []workflow.DashboardRejection{{
				ID: "p1", Groups: []workflow.GroupID{group, other}, By: "o", At: now,
				URL: "https://github.com/o/r/issues/9#issuecomment-1",
			}},
			Published: []workflow.DashboardPublication{{
				Group: group, By: "o", At: now, URL: "https://github.com/nateinaction/factory/issues/2",
			}},
			Handled: []int64{7, 8},
		}

		rendered, err := workflow.RenderDashboard(workflow.BuildDashboard(nil, state, now))
		Expect(err).NotTo(HaveOccurred())

		parsed, err := workflow.ParseDashboard(rendered)
		Expect(err).NotTo(HaveOccurred())
		Expect(parsed).To(Equal(state))
	})

	It("shows a published lesson beside the promotion it came from", func() {
		state := workflow.DashboardState{
			Actions: []workflow.DashboardAction{{
				Kind: workflow.ActionPromote, Groups: []workflow.GroupID{group}, By: "o", At: now.Add(-time.Hour),
			}},
			Published: []workflow.DashboardPublication{{
				Group: group, By: "o", At: now, URL: "https://github.com/nateinaction/factory/issues/2",
			}},
		}

		rendered, err := workflow.RenderDashboard(workflow.BuildDashboard(nil, state, now))
		Expect(err).NotTo(HaveOccurred())
		Expect(rendered).To(ContainSubstring("published by o"))
		Expect(rendered).To(ContainSubstring("https://github.com/nateinaction/factory/issues/2"))
	})
})

var _ = Describe("promotion", func() {
	now := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)

	skill := workflow.GroupID{
		Target: workflow.RetroTarget{Kind: workflow.TargetSkill, Name: "implement"}, Key: "guide-misses-tests",
	}
	docs := workflow.GroupID{Target: workflow.RetroTarget{Kind: workflow.TargetDocs, Name: "AGENTS.md"}, Key: "no-tests"}

	reports := []workflow.DashboardReport{{
		Issue: 12, Role: workflow.Implementer, Key: "sha-a", At: now, Severity: workflow.SeverityHigh,
		Target: skill.Target, Group: skill.Key, Claim: "the guide never mentions tests",
		Evidence: "the session ran the suite and nothing said how",
	}}

	It("files a document group in the repository it is about", func() {
		repo, err := workflow.PromotionRepo(docs, mustRepo("o/r"), false, mustRepo("nateinaction/factory"))
		Expect(err).NotTo(HaveOccurred())
		Expect(repo.String()).To(Equal("o/r"))
	})

	It("files a public skill group in the factory repo", func() {
		repo, err := workflow.PromotionRepo(skill, mustRepo("o/r"), false, mustRepo("nateinaction/factory"))
		Expect(err).NotTo(HaveOccurred())
		Expect(repo.String()).To(Equal("nateinaction/factory"))

		_, err = workflow.PromotionRepo(skill, mustRepo("o/r"), false, workflow.Repo{})
		Expect(err).To(HaveOccurred(), "promoted to a factory repo that is not configured")
	})

	It("keeps a private repository's group there", func() {
		repo, err := workflow.PromotionRepo(skill, mustRepo("o/r"), true, mustRepo("nateinaction/factory"))
		Expect(err).NotTo(HaveOccurred())
		Expect(repo.String()).To(Equal("o/r"))
	})

	It("keys the issue to its group and draws the lesson apart from the evidence", func() {
		title, body, err := workflow.RenderPromotion(skill, reports, true)
		Expect(err).NotTo(HaveOccurred())
		Expect(title).To(Equal("Retro: " + skill.String()))
		Expect(body).To(HavePrefix(workflow.PromotionMarker(skill)))
		Expect(body).To(ContainSubstring("## Lesson"))
		Expect(body).To(ContainSubstring("## Private evidence"))
		Expect(body).To(ContainSubstring("the guide never mentions tests"))
		Expect(body).To(ContainSubstring("the session ran the suite"))

		group, ok, err := workflow.ParsePromoted(body)
		Expect(err).NotTo(HaveOccurred())
		Expect(ok).To(BeTrue())
		Expect(group).To(Equal(skill))
	})

	It("publishes only the lesson, keyed to its group", func() {
		_, promoted, err := workflow.RenderPromotion(skill, reports, true)
		Expect(err).NotTo(HaveOccurred())

		lesson, ok := workflow.LessonOf(promoted)
		Expect(ok).To(BeTrue())
		Expect(lesson).To(ContainSubstring("the guide never mentions tests"))
		Expect(lesson).NotTo(ContainSubstring("the session ran the suite"))

		title, body, err := workflow.RenderPublished(skill, lesson, "o")
		Expect(err).NotTo(HaveOccurred())
		Expect(title).To(Equal("Retro: " + skill.String()))
		Expect(body).To(ContainSubstring("the guide never mentions tests"))
		Expect(body).NotTo(ContainSubstring("the session ran the suite"))
		Expect(body).NotTo(ContainSubstring("## Private evidence"))

		group, ok, err := workflow.ParsePublished(body)
		Expect(err).NotTo(HaveOccurred())
		Expect(ok).To(BeTrue())
		Expect(group).To(Equal(skill))

		_, ok = workflow.LessonOf("no lesson here")
		Expect(ok).To(BeFalse())
	})

	It("reads a marker strictly", func() {
		_, ok, err := workflow.ParsePromoted("A body with no marker")
		Expect(err).NotTo(HaveOccurred())
		Expect(ok).To(BeFalse())

		_, ok, err = workflow.ParsePromoted("<!-- factory:promoted group=not-a-group -->")
		Expect(err).To(HaveOccurred())
		Expect(ok).To(BeTrue())

		_, ok, err = workflow.ParsePublished(workflow.PromotionMarker(skill))
		Expect(err).NotTo(HaveOccurred())
		Expect(ok).To(BeFalse())
	})

	It("shows the command that promotes a ready group", func() {
		rendered, err := workflow.RenderDashboard(workflow.BuildDashboard(reports,
			workflow.DashboardState{}, now))
		Expect(err).NotTo(HaveOccurred())
		Expect(rendered).To(ContainSubstring("promote " + skill.String()))
		Expect(strings.Count(rendered, "promote "+skill.String())).To(BeNumerically(">=", 1))
	})
})
