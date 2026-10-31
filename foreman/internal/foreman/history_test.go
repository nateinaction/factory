package foreman

import (
	"context"
	"strconv"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/nateinaction/factory/foreman/internal/workflow"
)

// historyDay is the day the history specs' timelines happen on.
func historyDay() time.Time { return time.Date(2026, 10, 28, 0, 0, 0, 0, time.UTC) }

// at is the clock time on historyDay in RFC 3339.
func at(hour, minute int) string {
	return historyDay().Add(time.Duration(hour)*time.Hour + time.Duration(minute)*time.Minute).Format(time.RFC3339)
}

// labelEvent is a timeline event that adds or removes a label.
func labelEvent(event, label, when string) map[string]any {
	return map[string]any{"event": event, "label": map[string]string{"name": label}, "created_at": when}
}

// handoff is a handoff marker as the factory's App for role posts it.
func handoff(role workflow.Role, from, to workflow.State) string {
	text, err := workflow.RenderTransition(workflow.Transition{Role: role, RunID: "run-1", From: from, To: to})
	Expect(err).NotTo(HaveOccurred())

	return text
}

var _ = Describe("History", func() {
	var h *harness

	BeforeEach(func() {
		h = newHarness()
		h.deps.Now = func() time.Time { return historyDay().Add(15*time.Hour + 30*time.Minute) }
		// The emulator keeps no timeline, so an issue with no seeded one has no label events.
		h.gh.JSON("GET /repos/o/r/issues/{number}/timeline", []map[string]any{})
	})

	repo := func() workflow.Repo {
		GinkgoHelper()

		r, err := workflow.ParseRepo("o/r")
		Expect(err).NotTo(HaveOccurred())

		return r
	}

	// reworked seeds an issue that went plan, implement, review, rework and done.
	reworked := func() int {
		GinkgoHelper()

		n := h.createAs("o", nil, "Add x")
		bot := func(role workflow.Role, from, to workflow.State) {
			h.comment(n, appLogin(role), handoff(role, from, to))
		}
		bot(workflow.Planner, workflow.Plan, workflow.Planning)
		bot(workflow.Planner, workflow.Planning, workflow.Waiting)
		h.comment(n, "o", "go ahead")
		bot(workflow.Planner, workflow.Waiting, workflow.Implement)
		bot(workflow.Implementer, workflow.Implement, workflow.Implementing)
		bot(workflow.Implementer, workflow.Implementing, workflow.Review)
		bot(workflow.Reviewer, workflow.Review, workflow.Reviewing)
		bot(workflow.Reviewer, workflow.Reviewing, workflow.Implement)
		bot(workflow.Implementer, workflow.Implement, workflow.Implementing)
		bot(workflow.Implementer, workflow.Implementing, workflow.Review)
		bot(workflow.Reviewer, workflow.Review, workflow.Reviewing)
		bot(workflow.Reviewer, workflow.Reviewing, workflow.Done)

		h.gh.JSON("GET /repos/o/r/issues/"+strconv.Itoa(n)+"/timeline", []map[string]any{
			labelEvent("labeled", workflow.LabelPlan, at(10, 0)),
			labelEvent("labeled", "bug", at(10, 1)),
			labelEvent("unlabeled", workflow.LabelPlan, at(10, 10)),
			labelEvent("labeled", workflow.LabelPlanning, at(10, 10)),
			labelEvent("unlabeled", workflow.LabelPlanning, at(10, 15)),
			labelEvent("labeled", workflow.LabelWaiting, at(10, 15)),
			labelEvent("unlabeled", workflow.LabelWaiting, at(12, 15)),
			labelEvent("labeled", workflow.LabelImplement, at(12, 15)),
			labelEvent("unlabeled", workflow.LabelImplement, at(12, 20)),
			labelEvent("labeled", workflow.LabelImplementing, at(12, 20)),
			labelEvent("unlabeled", workflow.LabelImplementing, at(13, 20)),
			labelEvent("labeled", workflow.LabelReview, at(13, 20)),
			labelEvent("unlabeled", workflow.LabelReview, at(13, 30)),
			labelEvent("labeled", workflow.LabelReviewing, at(13, 30)),
			labelEvent("unlabeled", workflow.LabelReviewing, at(13, 40)),
			labelEvent("labeled", workflow.LabelImplement, at(13, 40)),
			labelEvent("unlabeled", workflow.LabelImplement, at(13, 45)),
			labelEvent("labeled", workflow.LabelImplementing, at(13, 45)),
			labelEvent("unlabeled", workflow.LabelImplementing, at(14, 15)),
			labelEvent("labeled", workflow.LabelReview, at(14, 15)),
			labelEvent("unlabeled", workflow.LabelReview, at(14, 20)),
			labelEvent("labeled", workflow.LabelReviewing, at(14, 20)),
			labelEvent("unlabeled", workflow.LabelReviewing, at(14, 30)),
			labelEvent("labeled", workflow.LabelDone, at(14, 30)),
		})

		return n
	}

	It("reports each state's duration, the human gate wait, the rework count and the outcome", func() {
		n := reworked()

		code, err := History(context.Background(), h.deps, testConfig(), repo())
		Expect(err).NotTo(HaveOccurred())
		Expect(code).To(BeZero())

		out := h.out.String()
		Expect(out).To(MatchRegexp(`(?m)^#` + strconv.Itoa(n) + `\s+done\s+1\s+2h0m0s\s+`))

		for _, want := range []string{
			"plan 10m0s", "planning 5m0s", "waiting 2h0m0s", "implement 10m0s", "implementing 1h30m0s",
			"review 15m0s", "reviewing 20m0s", "done 1h0m0s",
		} {
			Expect(out).To(ContainSubstring(want))
		}
	})

	It("counts only factory markers and factory labels", func() {
		n := h.createAs("o", nil, "Add x")
		// A person and another App cannot forge a handoff.
		h.comment(n, "o", handoff(workflow.Reviewer, workflow.Reviewing, workflow.Implement))
		h.comment(n, "factory", handoff(workflow.Reviewer, workflow.Reviewing, workflow.Implement))
		h.comment(n, appLogin(workflow.Implementer), "Looks like a marker but is just a note")
		h.comment(n, appLogin(workflow.Implementer), handoff(workflow.Implementer, workflow.Implementing, workflow.Review))
		h.gh.JSON("GET /repos/o/r/issues/"+strconv.Itoa(n)+"/timeline", []map[string]any{
			labelEvent("labeled", "bug", at(10, 0)),
			labelEvent("labeled", "wayfinder:research", at(10, 0)),
			labelEvent("labeled", workflow.LabelReview, at(11, 0)),
			labelEvent("unlabeled", workflow.LabelReview, at(11, 30)),
		})

		_, err := History(context.Background(), h.deps, testConfig(), repo())
		Expect(err).NotTo(HaveOccurred())

		out := h.out.String()
		Expect(out).To(MatchRegexp(`(?m)^#` + strconv.Itoa(n) + `\s+review\s+0\s+0s\s+review 30m0s\s*$`))
		Expect(out).NotTo(ContainSubstring("bug"))
		Expect(out).NotTo(ContainSubstring("wayfinder"))
	})

	It("leaves out an issue the factory never handled", func() {
		n := h.createAs("o", []string{"bug"}, "Not ours")
		h.gh.JSON("GET /repos/o/r/issues/"+strconv.Itoa(n)+"/timeline", []map[string]any{
			labelEvent("labeled", "bug", at(10, 0)),
		})

		_, err := History(context.Background(), h.deps, testConfig(), repo())
		Expect(err).NotTo(HaveOccurred())
		Expect(h.out.String()).NotTo(ContainSubstring("#" + strconv.Itoa(n)))
	})

	It("only reads GitHub, with a read-only token", func() {
		reworked()

		_, err := History(context.Background(), h.deps, testConfig(), repo())
		Expect(err).NotTo(HaveOccurred())

		for _, call := range h.gh.Calls() {
			Expect(call.Method).To(Equal("GET"), call.Method+" "+call.Path)
			Expect(call.Header.Get("Authorization")).To(ContainSubstring("read-only-token-"))
		}
	})

	It("stops at the issue bound and says so", func() {
		for range HistoryIssueLimit + 1 {
			h.createAs("o", nil, "Add x")
		}

		_, err := History(context.Background(), h.deps, testConfig(), repo())
		Expect(err).NotTo(HaveOccurred())
		Expect(h.out.String()).To(ContainSubstring("Stopped at the bound: read the newest 50 issues"))
	})

	It("does not claim a bound it did not reach", func() {
		reworked()

		_, err := History(context.Background(), h.deps, testConfig(), repo())
		Expect(err).NotTo(HaveOccurred())
		Expect(h.out.String()).NotTo(ContainSubstring("Stopped at the bound"))
	})

	It("fails when no factory App is installed on the repository", func() {
		h.installed = map[workflow.Role][]string{}

		code, err := History(context.Background(), h.deps, testConfig(), repo())
		Expect(code).To(Equal(1))
		Expect(err).To(MatchError(ContainSubstring("not an assigned repository")))
		Expect(strings.TrimSpace(h.out.String())).To(BeEmpty())
	})
})
