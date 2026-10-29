package foreman

import (
	"context"
	"errors"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/nateinaction/factory/foreman/internal/proc"
	"github.com/nateinaction/factory/foreman/internal/workflow"
)

// sudoExit makes the factory half exit with code, after checking the token file is in place.
func (h *harness) sudoExit(code int, tokenSeen *string, mode *os.FileMode) {
	base := h.runner.fn
	h.runner.fn = func(c proc.Cmd) (string, error) {
		if c.Argv[0] != "sudo" {
			return base(c)
		}
		raw, err := os.ReadFile(h.deps.Settings.TokenPath(workflow.Role(c.Argv[6])))
		if err != nil {
			h.t.Errorf("token file not in place: %v", err)
		}
		*tokenSeen = string(raw)
		if st, err := os.Stat(h.deps.Settings.TokenPath(workflow.Role(c.Argv[6]))); err == nil {
			*mode = st.Mode().Perm()
		}
		if code != 0 {
			return "", &proc.ExitError{Program: "sudo", Code: code}
		}
		return "", nil
	}
}

// outcome returns the labels added and comments posted on issue 5, in order.
func (h *harness) outcome() (labels, comments []string) {
	return h.gh.Bodies("POST", "/repos/o/r/issues/5/labels"), h.gh.Bodies("POST", "/repos/o/r/issues/5/comments")
}

// TestStartImplementerSuccessQueuesReview pins: start implementer success queues review.
func TestStartImplementerSuccessQueuesReview(t *testing.T) {
	h := newHarness(t)
	h.acceptWrites("5")
	var seen string
	var mode os.FileMode
	h.sudoExit(0, &seen, &mode)
	code, err := Start(context.Background(), h.deps, workflow.Implementer, "o/r", 5)
	if err != nil || code != 0 {
		t.Fatalf("%d %v", code, err)
	}
	sudo := h.runner.find("sudo")
	if len(sudo) != 1 || !slices.Equal(sudo[0].Argv, []string{
		"sudo", "-n", "-u", "factory", "/usr/local/bin/foreman", "run", "implementer", "o/r", "5",
	}) {
		t.Errorf("sudo %v", sudo)
	}
	if seen != "minted-token-implementer" || mode != 0o640 {
		t.Errorf("token file %q mode %v", seen, mode)
	}
	if _, err := os.Stat(h.deps.Settings.TokenPath(workflow.Implementer)); !os.IsNotExist(err) {
		t.Errorf("token file left behind: %v", err)
	}
	labels, comments := h.outcome()
	if len(labels) != 1 || !strings.Contains(labels[0], workflow.LabelReviewReady) ||
		!strings.Contains(comments[0], "The agent finished; see the linked pull request. Queued for review.") {
		t.Errorf("%v %v", labels, comments)
	}
	if got := h.gh.Summary(); !slices.Contains(got, "DELETE /repos/o/r/issues/5/labels/agent-working") {
		t.Errorf("did not release the claim: %v", got)
	}
	if n := len(h.runner.find("github-app-token")); n != 2 {
		t.Errorf("minted %d tokens, want a fresh one after the run", n)
	}
	h.leakFree("minted-token")
}

// TestStartImplementerFailureMarksFailed pins: start implementer failure marks failed.
func TestStartImplementerFailureMarksFailed(t *testing.T) {
	h := newHarness(t)
	h.acceptWrites("5")
	var seen string
	var mode os.FileMode
	h.sudoExit(1, &seen, &mode)
	code, err := Start(context.Background(), h.deps, workflow.Implementer, "o/r", 5)
	if err != nil || code != 1 {
		t.Fatalf("%d %v", code, err)
	}
	labels, comments := h.outcome()
	if !strings.Contains(labels[0], workflow.LabelFailed) || !strings.Contains(comments[0], "The agent failed. Check the dagu run log, then re-apply `ready-for-agent` to retry.") {
		t.Errorf("%v %v", labels, comments)
	}
	if _, err := os.Stat(h.deps.Settings.TokenPath(workflow.Implementer)); !os.IsNotExist(err) {
		t.Errorf("token file left behind: %v", err)
	}
}

// TestStartTreatsAFactoryHalfThatCannotRunAsFailed pins: start treats a factory half that cannot run as failed.
func TestStartTreatsAFactoryHalfThatCannotRunAsFailed(t *testing.T) {
	h := newHarness(t)
	h.acceptWrites("5")
	base := h.runner.fn
	h.runner.fn = func(c proc.Cmd) (string, error) {
		if c.Argv[0] == "sudo" {
			return "", errors.New("sudo missing")
		}
		return base(c)
	}
	code, err := Start(context.Background(), h.deps, workflow.Implementer, "o/r", 5)
	if err != nil || code != 1 {
		t.Fatalf("%d %v", code, err)
	}
	labels, _ := h.outcome()
	if !strings.Contains(labels[0], workflow.LabelFailed) || !strings.Contains(h.errb.String(), "sudo missing") {
		t.Errorf("%v %q", labels, h.errb.String())
	}
}

// TestStartReviewerRoutesByExitCode pins: start reviewer routes by exit code.
func TestStartReviewerRoutesByExitCode(t *testing.T) {
	cases := []struct {
		code     int
		wantLbl  string
		wantText string
		wantExit int
	}{
		{workflow.ReviewClean, workflow.LabelDone, "Review found no high or medium issues; see the pull request.", 0},
		{workflow.ReviewFindings, workflow.LabelReady, "Review found high or medium issues, posted on the pull request. Sent back to the agent.", 0},
		{workflow.ReviewExhausted, workflow.LabelFailed, "Review still found high or medium issues after 3 rounds. A person needs to look at the pull request.", 1},
		{2, workflow.LabelFailed, "The review failed. Check the dagu run log, then re-apply `ready-for-agent-review` to retry.", 1},
	}
	for _, tc := range cases {
		h := newHarness(t)
		h.acceptWrites("5")
		var seen string
		var mode os.FileMode
		h.sudoExit(tc.code, &seen, &mode)
		code, err := Start(context.Background(), h.deps, workflow.Reviewer, "o/r", 5)
		if err != nil || code != tc.wantExit {
			t.Fatalf("code %d: %d %v", tc.code, code, err)
		}
		labels, comments := h.outcome()
		if !strings.Contains(labels[0], tc.wantLbl) || !strings.Contains(comments[0], tc.wantText) {
			t.Errorf("code %d: %v %v", tc.code, labels, comments)
		}
		if got := h.gh.Summary(); !slices.Contains(got, "DELETE /repos/o/r/issues/5/labels/agent-reviewing") {
			t.Errorf("code %d did not release the claim: %v", tc.code, got)
		}
		if sudo := h.runner.find("sudo"); !strings.Contains(joined(sudo[0]), "run reviewer o/r 5") {
			t.Errorf("sudo %v", sudo)
		}
	}
}

// TestStartRefusesRolesWithoutBehaviour pins: start refuses roles without behaviour.
func TestStartRefusesRolesWithoutBehaviour(t *testing.T) {
	for _, role := range []workflow.Role{workflow.Planner, workflow.Researcher, workflow.Curator} {
		h := newHarness(t)
		if code, err := Start(context.Background(), h.deps, role, "o/r", 5); err == nil || code == 0 {
			t.Errorf("%s: %d %v", role, code, err)
		}
		if len(h.runner.cmds) != 0 || len(h.gh.Calls()) != 0 {
			t.Errorf("%s did something before refusing", role)
		}
	}
}
