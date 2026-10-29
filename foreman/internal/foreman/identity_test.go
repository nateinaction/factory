package foreman

import (
	"context"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/nateinaction/factory/foreman/internal/workflow"
)

// TestStartMintsOnlyTheOwningAppsTokens pins: no App ever falls back to another App's token.
func TestStartMintsOnlyTheOwningAppsTokens(t *testing.T) {
	for _, role := range []workflow.Role{workflow.Implementer, workflow.Reviewer} {
		h := newHarness(t)
		h.acceptWrites("5")
		var seen string
		var mode os.FileMode
		h.sudoExit(0, &seen, &mode)
		if _, err := Start(context.Background(), h.deps, role, "o/r", 5); err != nil {
			t.Fatal(err)
		}
		mints := h.runner.find("app-token", " token ")
		if len(mints) != 2 {
			t.Fatalf("%s: minted %d tokens", role, len(mints))
		}
		for _, c := range mints {
			if appOf(c) != string(role) || !slices.Equal(c.Argv[len(c.Argv)-2:], []string{"--repo", "o/r"}) {
				t.Errorf("%s: mint %v", role, c.Argv)
			}
		}
		for _, r := range h.gh.Calls() {
			if got := r.Header.Get("Authorization"); got != "Bearer minted-token-"+string(role) {
				t.Errorf("%s: %s %s used %q", role, r.Method, r.Path, got)
			}
		}
		if seen != "minted-token-"+string(role) {
			t.Errorf("%s: token file %q", role, seen)
		}
	}
}

// TestStartFailsARunWhoseAppCredentialIsMissing pins: the run fails to the failed label, posted by an App still installed.
func TestStartFailsARunWhoseAppCredentialIsMissing(t *testing.T) {
	h := newHarness(t)
	h.acceptWrites("5")
	h.failMint(workflow.Implementer, false)
	code, err := Start(context.Background(), h.deps, workflow.Implementer, "o/r", 5)
	if err != nil || code != 1 {
		t.Fatalf("%d %v", code, err)
	}
	if len(h.runner.find("sudo")) != 0 {
		t.Error("ran the agent without its credential")
	}
	labels, comments := h.outcome()
	if len(labels) != 1 || !strings.Contains(labels[0], workflow.LabelFailed) || !strings.Contains(comments[0], "credential") {
		t.Errorf("%v %v", labels, comments)
	}
	for _, r := range h.gh.Calls() {
		if got := r.Header.Get("Authorization"); got != "Bearer minted-token-reviewer" {
			t.Errorf("%s %s used %q", r.Method, r.Path, got)
		}
	}
}

// TestStartWithNoAppLeftReportsTheFailure pins: with no App able to post, the run still fails and writes nothing.
func TestStartWithNoAppLeftReportsTheFailure(t *testing.T) {
	h := newHarness(t)
	h.failMint(workflow.Implementer, false)
	h.failMint(workflow.Reviewer, false)
	code, err := Start(context.Background(), h.deps, workflow.Implementer, "o/r", 5)
	if err == nil || code != 1 {
		t.Fatalf("%d %v", code, err)
	}
	if len(h.gh.Calls()) != 0 {
		t.Errorf("wrote without a token: %v", h.gh.Summary())
	}
}
