package foreman

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/nateinaction/factory/foreman/internal/config"
	"github.com/nateinaction/factory/foreman/internal/workflow"
)

// moduleSources lists every non-test Go file in the module, whose root is two levels up.
func moduleSources() []string {
	var files []string

	Expect(filepath.WalkDir(filepath.Join("..", ".."), func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		if !d.IsDir() && strings.HasSuffix(path, ".go") && !strings.HasSuffix(path, "_test.go") {
			files = append(files, path)
		}

		return nil
	})).To(Succeed())
	Expect(files).To(ContainElement(filepath.Join("..", "..", "internal", "github", "github.go")), "walked the wrong tree")

	return files
}

// stringLiterals returns every string literal in the Go file at path.
func stringLiterals(path string) []string {
	file, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
	Expect(err).NotTo(HaveOccurred())

	var out []string

	ast.Inspect(file, func(n ast.Node) bool {
		lit, ok := n.(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			return true
		}

		s, err := strconv.Unquote(lit.Value)
		Expect(err).NotTo(HaveOccurred(), path+": "+lit.Value)

		out = append(out, s)

		return true
	})

	return out
}

// A human stays in the merge path: foreman has no code path that can merge.
var _ = Describe("the merge path", func() {
	It("is named by no string literal in foreman's source", func() {
		for _, path := range moduleSources() {
			for _, s := range stringLiterals(path) {
				Expect(s).NotTo(Or(
					ContainSubstring("/merge"), ContainSubstring("auto_merge"),
					WithTransform(strings.ToLower, ContainSubstring("enableautomerge")),
				), path)
			}
		}
	})

	DescribeTable("is called by no flow",
		func(flow func(h *harness)) {
			h := newHarness()
			h.withConfig()
			flow(h)

			for _, r := range h.gh.Calls() {
				Expect(r.Path).NotTo(ContainSubstring("merge"))
				Expect(r.Method).NotTo(Equal(http.MethodPut))
			}
		},
		Entry("poll", func(h *harness) {
			h.queue(map[string][]map[string]any{workflow.LabelReview: {{"number": 5}}})
			h.acceptWrites()
			cfg, err := config.Load(h.deps.Settings.ConfigFile)
			Expect(err).NotTo(HaveOccurred())
			_, err = Poll(context.Background(), h.deps, cfg)
			Expect(err).NotTo(HaveOccurred())
		}),
		Entry("start", func(h *harness) {
			h.acceptWrites()
			_, err := Start(context.Background(), h.deps, testConfig(), workflow.Reviewer, testRef(5))
			Expect(err).NotTo(HaveOccurred())
		}),
		Entry("run implementer", func(h *harness) {
			h.implementerGH(nil, nil)
			h.writeToken("implementer", "t")
			h.roleWrites(workflow.Implementer, 5, firstRunOutput)
			_, err := Run(context.Background(), h.deps, workflow.Implementer, testRef(5), testAgent(), testRetro(), 3)
			Expect(err).NotTo(HaveOccurred())
		}),
		Entry("run reviewer", func(h *harness) {
			h.reviewerGH(nil)
			h.roleWrites(workflow.Reviewer, 5, cleanReview)
			_, err := Run(context.Background(), h.deps, workflow.Reviewer, testRef(5), testAgent(), testRetro(), 3)
			Expect(err).NotTo(HaveOccurred())
		}),
	)
})
