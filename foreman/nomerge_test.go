package foreman

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// TestNoCodePathNamesTheMergeEndpoint reads foreman's own source: no string literal
// may name a merge endpoint, so no code path can call it. A human stays in the merge path.
func TestNoCodePathNamesTheMergeEndpoint(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(token.NewFileSet(), name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(file, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			s, err := strconv.Unquote(lit.Value)
			if err != nil {
				t.Errorf("%s: unquote %s: %v", name, lit.Value, err)
				return true
			}
			if strings.Contains(s, "/merge") || strings.Contains(s, "auto_merge") || strings.Contains(strings.ToLower(s), "enableautomerge") {
				t.Errorf("%s: string literal %q names a merge endpoint", name, s)
			}
			return true
		})
	}
}

// TestNoRunMakesAMergeCall drives every flow against a fake and checks that no request merges.
func TestNoRunMakesAMergeCall(t *testing.T) {
	flows := map[string]func(*harness){
		"poll": func(h *harness) {
			h.queue(map[string][]map[string]any{LabelReviewReady: {{"number": 5}}})
			h.acceptWrites("5")
			_ = Main(context.Background(), []string{"poll"}, h.deps)
		},
		"start": func(h *harness) {
			h.acceptWrites("5")
			_ = Main(context.Background(), []string{"start", "reviewer", "o/r", "5"}, h.deps)
		},
		"run implementer": func(h *harness) {
			h.implementerGH(nil, nil)
			writeToken(t, h)
			_ = Main(context.Background(), []string{"run", "implementer", "o/r", "5"}, h.deps)
		},
		"run reviewer": func(h *harness) {
			h.reviewerGH(nil)
			h.opencodeWrites("code-review", filepath.Join(h.deps.Settings.WorkRoot, "review-5.json"), cleanReview)
			_ = Main(context.Background(), []string{"run", "reviewer", "o/r", "5"}, h.deps)
		},
	}
	for name, flow := range flows {
		h := newHarness(t)
		h.withConfig()
		flow(h)
		for _, r := range h.gh.calls() {
			if strings.Contains(r.Path, "merge") || r.Method == "PUT" {
				t.Errorf("%s: %s %s", name, r.Method, r.Path)
			}
		}
	}
}

// writeToken leaves a token for the factory half.
func writeToken(t *testing.T, h *harness) {
	t.Helper()
	if err := os.WriteFile(h.deps.Settings.TokenPath(Implementer), []byte("t"), 0o640); err != nil {
		t.Fatal(err)
	}
}
