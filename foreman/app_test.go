package foreman

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// withConfig writes a valid config file and points the harness at it.
func (h *harness) withConfig() {
	h.t.Helper()
	path := filepath.Join(h.dir, "config.json")
	if err := os.WriteFile(path, []byte(`{"roles":["implementer","reviewer"],"model":"prov/model"}`), 0o600); err != nil {
		h.t.Fatal(err)
	}
	h.deps.Settings.ConfigFile = path
}

// TestMainRejectsBadInvocationsWithUsageExit pins: main rejects bad invocations with usage exit.
func TestMainRejectsBadInvocationsWithUsageExit(t *testing.T) {
	bad := [][]string{
		nil,
		{"frobnicate"},
		{"poll", "extra"},
		{"start"},
		{"start", "implementer", "o/r"},
		{"start", "implementer", "o/r", "5", "extra"},
		{"run", "implementer", "o/r"},
		{"start", "implementer", "not-a-repo", "5"},
		{"start", "implementer", "o/r", "five"},
		{"run", "implementer", "o/r", "5; rm -rf /"},
	}
	for _, args := range bad {
		h := newHarness(t)
		h.withConfig()
		if code := Main(context.Background(), args, h.deps); code != ExitUsage {
			t.Errorf("%v: exit %d", args, code)
		}
		if len(h.runner.cmds) != 0 || len(h.gh.calls()) != 0 {
			t.Errorf("%v: did work on a bad invocation", args)
		}
	}
}

// TestMainUnknownAgentRoleFailsClosed pins: main unknown agent role fails closed.
func TestMainUnknownAgentRoleFailsClosed(t *testing.T) {
	for _, verb := range []string{"start", "run"} {
		h := newHarness(t)
		h.withConfig()
		code := Main(context.Background(), []string{verb, "janitor", "o/r", "5"}, h.deps)
		if code == 0 {
			t.Errorf("%s accepted an unknown role", verb)
		}
		if !strings.Contains(h.errb.String(), `unknown agent role "janitor"`) {
			t.Errorf("%s stderr %q", verb, h.errb.String())
		}
		if len(h.runner.cmds) != 0 || len(h.gh.calls()) != 0 {
			t.Errorf("%s did work for an unknown role", verb)
		}
	}
}

// TestMainKnownRoleWithoutBehaviourExitsNonZero pins: main known role without behaviour exits non zero.
func TestMainKnownRoleWithoutBehaviourExitsNonZero(t *testing.T) {
	h := newHarness(t)
	h.withConfig()
	if code := Main(context.Background(), []string{"start", "planner", "o/r", "5"}, h.deps); code == 0 {
		t.Error("exit 0")
	}
}

// TestMainPollUsesTheConfiguredAgentRoles pins: main poll polls where the configured Apps are installed.
func TestMainPollUsesTheConfiguredAgentRoles(t *testing.T) {
	h := newHarness(t)
	h.withConfig()
	h.queue(nil)
	if code := Main(context.Background(), []string{"poll"}, h.deps); code != 0 {
		t.Fatalf("exit %d: %s", code, h.errb.String())
	}
	if got := h.gh.calls()[0].Path; got != "/repos/o/r/issues" {
		t.Errorf("path %q", got)
	}
}

// TestMainRunReturnsTheReviewVerdictAsExitCode pins: main run returns the review verdict as exit code.
func TestMainRunReturnsTheReviewVerdictAsExitCode(t *testing.T) {
	h := newHarness(t)
	h.withConfig()
	h.reviewerGH(nil)
	h.opencodeWrites("code-review", filepath.Join(h.deps.Settings.WorkRoot, "review-5.json"), blockingReview)
	if code := Main(context.Background(), []string{"run", "reviewer", "o/r", "5"}, h.deps); code != ReviewFindings {
		t.Fatalf("exit %d: %s", code, h.errb.String())
	}
	if oc := h.runner.find("opencode run --model prov/model"); len(oc) != 1 {
		t.Errorf("model from config not used: %v", h.runner.cmds)
	}
}

// TestMainStartRunsTheWholeImplementerHandoff pins: main start runs the whole implementer handoff.
func TestMainStartRunsTheWholeImplementerHandoff(t *testing.T) {
	h := newHarness(t)
	h.withConfig()
	h.acceptWrites("5")
	if code := Main(context.Background(), []string{"start", "implementer", "o/r", "5"}, h.deps); code != 0 {
		t.Fatalf("exit %d: %s", code, h.errb.String())
	}
}

// TestMainBadConfigFailsClosed pins: main bad config fails closed.
func TestMainBadConfigFailsClosed(t *testing.T) {
	for _, cfg := range []string{``, `{}`, `{"roles":["janitor"],"model":"m"}`, `{"roles":[],"model":"m"}`, `{"roles":["implementer"]}`, `{"model":"m"}`, `[`} {
		h := newHarness(t)
		path := filepath.Join(h.dir, "config.json")
		if err := os.WriteFile(path, []byte(cfg), 0o600); err != nil {
			t.Fatal(err)
		}
		h.deps.Settings.ConfigFile = path
		if code := Main(context.Background(), []string{"poll"}, h.deps); code == 0 {
			t.Errorf("accepted config %q", cfg)
		}
	}
	h := newHarness(t)
	h.deps.Settings.ConfigFile = filepath.Join(h.dir, "missing.json")
	if code := Main(context.Background(), []string{"poll"}, h.deps); code == 0 {
		t.Error("accepted a missing config")
	}
}

// TestMainErrorsNeverCarryTheToken pins: main errors never carry the token.
func TestMainErrorsNeverCarryTheToken(t *testing.T) {
	h := newHarness(t)
	h.withConfig()
	h.gh.status("GET /repos/o/r/issues", 500)
	if code := Main(context.Background(), []string{"poll"}, h.deps); code != 1 {
		t.Fatalf("exit %d", code)
	}
	if strings.Contains(h.errb.String(), "minted-token") || !strings.Contains(h.errb.String(), "foreman: ") {
		t.Errorf("stderr %q", h.errb.String())
	}
}

// TestDefaultSettingsKeepTheConfigLocations pins: default settings keep the config locations.
func TestDefaultSettingsKeepTheConfigLocations(t *testing.T) {
	s := DefaultSettings("/home/factory", "")
	if s.ConfigFile != "/etc/factory-agent/config.json" || s.KeyFile != "/home/factory/.config/factory-agent/openrouter-key" ||
		s.WorkRoot != "/home/factory/work" || s.TokenPath(Reviewer) != "/run/factory/gh-token-reviewer" || s.API != "https://api.github.com" {
		t.Errorf("%+v", s)
	}
	if DefaultSettings("/h", "/x.json").ConfigFile != "/x.json" {
		t.Error("override ignored")
	}
}
