package workflow_test

import (
	"encoding/json"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/nateinaction/factory/foreman/internal/workflow"
)

var _ = Describe("the retro dashboard", func() {
	// now is the clock every spec's dashboard is drawn at.
	now := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)

	// skillGroup is the target and group key most specs report on.
	skillTarget := workflow.RetroTarget{Kind: workflow.TargetSkill, Name: "implement"}

	const groupKey = "guide-misses-tests"

	// report is one retro finding with where it came from.
	report := func(issue int, role workflow.Role, key string, severity workflow.Severity) workflow.DashboardReport {
		return workflow.DashboardReport{
			Issue: issue, Role: role, Key: key, At: now,
			Target: skillTarget, Group: groupKey, Severity: severity,
			Claim: "the guide never mentions tests",
		}
	}

	// groupIDs lists the groups of a section, in the order drawn.
	groupIDs := func(groups []workflow.DashboardGroup) []string {
		out := make([]string, 0, len(groups))

		for _, g := range groups {
			out = append(out, g.ID.String())
		}

		return out
	}

	Describe("readiness", func() {
		It("is ready when two different issues report the group", func() {
			view := workflow.BuildDashboard([]workflow.DashboardReport{
				report(12, workflow.Implementer, "sha-a", workflow.SeverityMedium),
				report(13, workflow.Reviewer, "sha-b", workflow.SeverityLow),
			}, workflow.DashboardState{}, now)

			Expect(groupIDs(view.Ready)).To(ConsistOf("skill:implement/guide-misses-tests"))
			Expect(view.Watching).To(BeEmpty())
		})

		It("counts issues, not agent roles: two roles on one issue count once", func() {
			view := workflow.BuildDashboard([]workflow.DashboardReport{
				report(12, workflow.Implementer, "sha-a", workflow.SeverityMedium),
				report(12, workflow.Reviewer, "sha-b", workflow.SeverityMedium),
			}, workflow.DashboardState{}, now)

			Expect(view.Ready).To(BeEmpty())
			Expect(groupIDs(view.Watching)).To(ConsistOf("skill:implement/guide-misses-tests"))
		})

		It("is ready after one high-severity report", func() {
			view := workflow.BuildDashboard([]workflow.DashboardReport{
				report(12, workflow.Implementer, "sha-a", workflow.SeverityHigh),
			}, workflow.DashboardState{}, now)

			Expect(groupIDs(view.Ready)).To(ConsistOf("skill:implement/guide-misses-tests"))
		})

		It("keeps one group per target and key, so equal keys on different targets stay apart", func() {
			docsTarget := workflow.RetroTarget{Kind: workflow.TargetDocs, Name: "AGENTS.md"}

			docs := report(13, workflow.Reviewer, "sha-b", workflow.SeverityHigh)
			docs.Target = docsTarget

			view := workflow.BuildDashboard([]workflow.DashboardReport{
				report(12, workflow.Implementer, "sha-a", workflow.SeverityHigh), docs,
			}, workflow.DashboardState{}, now)

			Expect(groupIDs(view.Ready)).To(ConsistOf(
				"docs:AGENTS.md/guide-misses-tests", "skill:implement/guide-misses-tests",
			))
		})
	})

	Describe("watching", func() {
		It("drops a group after 60 days with no repeat", func() {
			reports := []workflow.DashboardReport{
				report(12, workflow.Implementer, "sha-a", workflow.SeverityMedium),
			}

			fresh := workflow.BuildDashboard(reports, workflow.DashboardState{}, now.Add(59*24*time.Hour))
			Expect(groupIDs(fresh.Watching)).To(HaveLen(1))

			expired := workflow.BuildDashboard(reports, workflow.DashboardState{},
				now.Add(workflow.WatchingWindow+time.Minute))
			Expect(expired.Watching).To(BeEmpty())
		})

		It("keeps a group another report refreshed", func() {
			reports := make([]workflow.DashboardReport, 0, 2)
			reports = append(reports, report(12, workflow.Implementer, "sha-a", workflow.SeverityMedium))

			repeat := report(12, workflow.Implementer, "sha-c", workflow.SeverityLow)
			repeat.At = now.Add(50 * 24 * time.Hour)

			view := workflow.BuildDashboard(append(reports, repeat), workflow.DashboardState{},
				now.Add(workflow.WatchingWindow+time.Hour))
			Expect(groupIDs(view.Watching)).To(HaveLen(1))
		})

		It("never drops a ready group, however old", func() {
			view := workflow.BuildDashboard([]workflow.DashboardReport{
				report(12, workflow.Implementer, "sha-a", workflow.SeverityMedium),
				report(13, workflow.Reviewer, "sha-b", workflow.SeverityLow),
			}, workflow.DashboardState{}, now.Add(10*workflow.WatchingWindow))

			Expect(groupIDs(view.Ready)).To(HaveLen(1))
		})
	})

	Describe("writer actions and Curator proposals", func() {
		// action records a writer's promote or ignore, and the proposal a Curator might file.
		promote := workflow.DashboardAction{
			Kind: workflow.ActionPromote,
			Groups: []workflow.GroupID{{
				Target: skillTarget, Key: groupKey,
			}},
			By: "o", At: now.Add(-time.Hour), URL: "https://github.com/o/r/issues/99",
		}

		It("moves a promoted group out of the ready section", func() {
			older := report(12, workflow.Implementer, "sha-a", workflow.SeverityHigh)
			older.At = now.Add(-2 * time.Hour)

			view := workflow.BuildDashboard([]workflow.DashboardReport{older},
				workflow.DashboardState{Actions: []workflow.DashboardAction{promote}}, now)

			Expect(view.Ready).To(BeEmpty())
			Expect(view.Promoted).To(HaveLen(1))
			Expect(view.Promoted[0].Action.By).To(Equal("o"))
		})

		It("ignores a group instead of showing it", func() {
			ignored := promote
			ignored.Kind = workflow.ActionIgnore

			older := report(12, workflow.Implementer, "sha-a", workflow.SeverityHigh)
			older.At = now.Add(-2 * time.Hour)

			view := workflow.BuildDashboard([]workflow.DashboardReport{older},
				workflow.DashboardState{Actions: []workflow.DashboardAction{ignored}}, now)

			Expect(view.Ready).To(BeEmpty())
			Expect(view.Ignored).To(HaveLen(1))
			Expect(view.Promoted).To(BeEmpty())
		})

		It("regresses a promoted group a later report mentions", func() {
			view := workflow.BuildDashboard([]workflow.DashboardReport{
				report(14, workflow.Reviewer, "sha-d", workflow.SeverityHigh),
			}, workflow.DashboardState{Actions: []workflow.DashboardAction{promote}}, now)

			Expect(view.Promoted).To(BeEmpty())
			Expect(view.Regressed).To(HaveLen(1))
			Expect(view.Regressed[0].Action.By).To(Equal("o"))
		})

		It("keeps a promoted group promoted when every report predates the promotion", func() {
			late := promote
			late.At = now.Add(time.Hour)

			view := workflow.BuildDashboard([]workflow.DashboardReport{
				report(12, workflow.Implementer, "sha-a", workflow.SeverityHigh),
			}, workflow.DashboardState{Actions: []workflow.DashboardAction{late}}, now)

			Expect(view.Promoted).To(HaveLen(1))
			Expect(view.Regressed).To(BeEmpty())
		})

		It("shows a Curator proposal with its id, groups and reason", func() {
			state := workflow.DashboardState{Proposals: []workflow.GroupRelation{{
				ID: "p1",
				Groups: []workflow.GroupID{
					{Target: skillTarget, Key: groupKey},
					{Target: skillTarget, Key: "guide-skips-review"},
				},
				Reason: "one guide problem, split in two",
			}}}

			rendered, err := workflow.RenderDashboard(workflow.BuildDashboard(nil, state, now))
			Expect(err).NotTo(HaveOccurred())
			Expect(rendered).To(And(
				ContainSubstring("## Proposed merges (1)"),
				ContainSubstring("`p1`"),
				ContainSubstring("one guide problem, split in two"),
			))
		})
	})

	Describe("the body", func() {
		// view is a dashboard with one ready and one watching group.
		view := func() workflow.DashboardView {
			return workflow.BuildDashboard([]workflow.DashboardReport{
				report(12, workflow.Implementer, "sha-a", workflow.SeverityHigh),
				report(13, workflow.Reviewer, "sha-b", workflow.SeverityMedium),
				func() workflow.DashboardReport {
					watching := report(20, workflow.Implementer, "sha-c", workflow.SeverityLow)
					watching.Group = "slow-review"

					return watching
				}(),
			}, workflow.DashboardState{}, now)
		}

		It("shows every section, each with its count", func() {
			rendered, err := workflow.RenderDashboard(view())
			Expect(err).NotTo(HaveOccurred())

			Expect(rendered).To(HavePrefix(workflow.DashboardMarker))
			Expect(rendered).To(ContainSubstring("## Ready to promote (1)"))
			Expect(rendered).To(ContainSubstring("## Proposed merges (0)"))
			Expect(rendered).To(ContainSubstring("## Watching (1)"))
			Expect(rendered).To(ContainSubstring("## Promoted (0)"))
			Expect(rendered).To(ContainSubstring("## Ignored (0)"))
			Expect(rendered).To(ContainSubstring("## Regressed (0)"))
		})

		It("collapses Watching", func() {
			rendered, err := workflow.RenderDashboard(view())
			Expect(err).NotTo(HaveOccurred())
			Expect(rendered).To(ContainSubstring("<details>"))
			Expect(rendered).To(ContainSubstring("<summary>1 group is not ready yet</summary>"))
		})

		It("shows the group's count, how the reports vary, and each report's agent role and run", func() {
			rendered, err := workflow.RenderDashboard(view())
			Expect(err).NotTo(HaveOccurred())

			Expect(rendered).To(ContainSubstring("2 reports from 2 issues"))
			Expect(rendered).To(ContainSubstring("1 high, 1 medium"))
			Expect(rendered).To(ContainSubstring("implementer run `sha-a` on #12"))
			Expect(rendered).To(ContainSubstring("reviewer run `sha-b` on #13"))
		})

		It("round-trips the state writers and the Curator leave behind", func() {
			state := workflow.DashboardState{
				Actions: []workflow.DashboardAction{{
					Kind: workflow.ActionPromote,
					Groups: []workflow.GroupID{{
						Target: skillTarget, Key: groupKey,
					}},
					By: "o", At: now.Add(-time.Hour), URL: "https://github.com/o/r/issues/99",
				}, {
					Kind: workflow.ActionCurate, By: "o", At: now.Add(-30 * time.Minute),
					URL: "https://github.com/o/r/issues/99#issuecomment-2",
				}},
				Proposals: []workflow.GroupRelation{{
					ID: "p1",
					Groups: []workflow.GroupID{
						{Target: skillTarget, Key: groupKey},
						{Target: skillTarget, Key: "guide-skips-review"},
					},
					Reason: "same guide problem",
				}},
				Links: []workflow.GroupRelation{{
					ID: "l1",
					Groups: []workflow.GroupID{
						{Target: skillTarget, Key: groupKey},
						{Target: workflow.RetroTarget{Kind: workflow.TargetDocs, Name: "AGENTS.md"}, Key: "no-lab"},
					},
					Reason: "the docs and the guide disagree",
				}},
				Curator: workflow.DashboardCurator{
					LastRun: now.Add(-2 * time.Hour), Claim: now.Add(-time.Minute),
				},
			}

			rendered, err := workflow.RenderDashboard(workflow.BuildDashboard(nil, state, now))
			Expect(err).NotTo(HaveOccurred())

			parsed, err := workflow.ParseDashboard(rendered)
			Expect(err).NotTo(HaveOccurred())
			Expect(parsed).To(Equal(state))
		})

		It("round-trips a full state exactly, with every optional part", func() {
			state := workflow.DashboardState{
				Actions: []workflow.DashboardAction{{
					Kind: workflow.ActionMerge,
					Groups: []workflow.GroupID{
						{Target: skillTarget, Key: groupKey},
						{Target: skillTarget, Key: "guide-skips-review"},
					},
					By: "o", At: now.Add(-time.Hour).Add(123 * time.Nanosecond),
				}},
				Rejected: []workflow.DashboardRejection{{
					ID: "p9", By: "o", At: now.Add(-3 * time.Hour), URL: "https://github.com/o/r/issues/1",
					Groups: []workflow.GroupID{
						{Target: skillTarget, Key: groupKey},
						{Target: skillTarget, Key: "guide-skips-review"},
					},
				}},
				Published: []workflow.DashboardPublication{{
					Group: workflow.GroupID{Target: skillTarget, Key: groupKey},
					By:    "o", At: now.Add(-4 * time.Hour), URL: "https://github.com/o/r/issues/2",
				}},
				Handled: []int64{7, 3},
			}

			raw, err := workflow.RenderDashboard(workflow.BuildDashboard(nil, state, now))
			Expect(err).NotTo(HaveOccurred())

			parsed, err := workflow.ParseDashboard(raw)
			Expect(err).NotTo(HaveOccurred())
			Expect(parsed).To(Equal(state))

			again, err := workflow.RenderDashboard(workflow.BuildDashboard(nil, parsed, now))
			Expect(err).NotTo(HaveOccurred())
			Expect(again).To(Equal(raw))
		})

		const (
			groupG = `{"target":"skill:x","group":"g"}`
			groupH = `{"target":"skill:x","group":"h"}`
		)

		DescribeTable("fails closed on any malformed field of the state block",
			func(fields string) {
				_, err := workflow.ParseDashboard(
					workflow.DashboardMarker + "\n\n<!-- factory:dashboard-json\n{" + fields + "}\n-->")
				Expect(err).To(HaveOccurred(), fields)
			},
			Entry("no version", `"actions":[],"proposals":[]`),
			Entry("a future version", `"version":2,"actions":[],"proposals":[]`),
			Entry("an unknown field", `"version":1,"actions":[],"proposals":[],"extra":1`),
			Entry("trailing data", `"version":1,"actions":[],"proposals":[]}{`),
			Entry("an unknown action kind", `"version":1,"proposals":[],"actions":[`+
				`{"kind":"burn","groups":[],"by":"o","at":"2026-10-01T10:00:00Z"}]`),
			Entry("an action with the wrong group count", `"version":1,"proposals":[],"actions":[`+
				`{"kind":"promote","groups":[],"by":"o","at":"2026-10-01T10:00:00Z"}]`),
			Entry("an action with no date", `"version":1,"proposals":[],"actions":[`+
				`{"kind":"curate","groups":[],"by":"o"}]`),
			Entry("an action with a malformed date", `"version":1,"proposals":[],"actions":[`+
				`{"kind":"curate","groups":[],"by":"o","at":"yesterday"}]`),
			Entry("an action with no author", `"version":1,"proposals":[],"actions":[`+
				`{"kind":"curate","groups":[],"at":"2026-10-01T10:00:00Z"}]`),
			Entry("an action link over several lines", `"version":1,"proposals":[],"actions":[`+
				`{"kind":"curate","groups":[],"by":"o","at":"2026-10-01T10:00:00Z","url":"a\nb"}]`),
			Entry("a group with a bad target", `"version":1,"proposals":[],"actions":[`+
				`{"kind":"ignore","groups":[{"target":"skill:../x","group":"g"}],"by":"o","at":"2026-10-01T10:00:00Z"}]`),
			Entry("a group with a bad key", `"version":1,"proposals":[],"actions":[`+
				`{"kind":"ignore","groups":[{"target":"skill:x","group":"G G"}],"by":"o","at":"2026-10-01T10:00:00Z"}]`),
			Entry("a group that is null", `"version":1,"proposals":[],"actions":[`+
				`{"kind":"ignore","groups":[null],"by":"o","at":"2026-10-01T10:00:00Z"}]`),
			Entry("a proposal of one group", `"version":1,"actions":[],"proposals":[`+
				`{"id":"p","groups":[`+groupG+`],"reason":"r"}]`),
			Entry("a proposal with a bad id", `"version":1,"actions":[],"proposals":[`+
				`{"id":"P!","groups":[`+groupG+`,`+groupH+`],"reason":"r"}]`),
			Entry("a proposal with an empty reason", `"version":1,"actions":[],"proposals":[`+
				`{"id":"p","groups":[`+groupG+`,`+groupH+`],"reason":" "}]`),
			Entry("a link within one target", `"version":1,"actions":[],"proposals":[],"links":[`+
				`{"id":"l","groups":[`+groupG+`,`+groupH+`],"reason":"r"}]`),
			Entry("a rejection with no date", `"version":1,"actions":[],"proposals":[],"rejected":[`+
				`{"id":"p","groups":[`+groupG+`,`+groupH+`],"by":"o"}]`),
			Entry("a publication with no group", `"version":1,"actions":[],"proposals":[],"published":[`+
				`{"by":"o","at":"2026-10-01T10:00:00Z"}]`),
			Entry("a malformed curator date", `"version":1,"actions":[],"proposals":[],`+
				`"curator":{"last_run":"soon"}`),
			Entry("a handled id of zero", `"version":1,"actions":[],"proposals":[],"handled":[0]`),
			Entry("a repeated handled id", `"version":1,"actions":[],"proposals":[],"handled":[4,4]`),
		)

		It("is deterministic: the same log and state render the same body", func() {
			first, err := workflow.RenderDashboard(view())
			Expect(err).NotTo(HaveOccurred())

			second, err := workflow.RenderDashboard(view())
			Expect(err).NotTo(HaveOccurred())
			Expect(second).To(Equal(first))
		})

		It("accepts an empty body as an empty state, and rejects a body it cannot read whole", func() {
			parsed, err := workflow.ParseDashboard("")
			Expect(err).NotTo(HaveOccurred())
			Expect(parsed).To(Equal(workflow.DashboardState{}))

			_, err = workflow.ParseDashboard("a body a person wrote, with no state block")
			Expect(err).To(HaveOccurred())

			_, err = workflow.ParseDashboard(workflow.DashboardMarker + "\n\n<!-- factory:dashboard-json\n{\"version\":1,")
			Expect(err).To(HaveOccurred())
		})

		It("counts every report in the log, not only the ones it shows", func() {
			reports := make([]workflow.DashboardReport, 0, workflow.MaxGroupReports+3)

			for i := range workflow.MaxGroupReports + 3 {
				reports = append(reports, report(30+i, workflow.Implementer, "sha-"+strings.Repeat("a", i+1),
					workflow.SeverityLow))
			}

			view := workflow.BuildDashboard(reports, workflow.DashboardState{}, now)
			Expect(view.Ready).To(HaveLen(1))

			rendered, err := workflow.RenderDashboard(view)
			Expect(err).NotTo(HaveOccurred())
			Expect(rendered).To(ContainSubstring("8 reports from 8 issues"))
			Expect(strings.Count(rendered, "implementer run `")).To(Equal(workflow.MaxGroupReports))
		})
	})
})

var _ = Describe("a retro comment's date", func() {
	It("reads updated_at, falling back to created_at", func() {
		var item workflow.Item

		Expect(json.Unmarshal([]byte(`{"id":1,"body":"b","created_at":"2026-10-01T10:00:00Z",`+
			`"updated_at":"2026-10-02T10:00:00Z"}`), &item)).To(Succeed())
		Expect(item.UpdatedAt).To(Equal(time.Date(2026, 10, 2, 10, 0, 0, 0, time.UTC)))

		Expect(json.Unmarshal([]byte(`{"id":1,"body":"b","created_at":"2026-10-01T10:00:00Z"}`),
			&item)).To(Succeed())
		Expect(item.UpdatedAt).To(Equal(time.Date(2026, 10, 1, 10, 0, 0, 0, time.UTC)))
	})
})
