package foreman

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("the package's source", func() {
	// literals lists every string the package's non-test files define, comments excluded: a comment may
	// name a state without spelling one.
	literals := func() map[string][]string {
		GinkgoHelper()

		entries, err := os.ReadDir(".")
		Expect(err).NotTo(HaveOccurred())

		out := make(map[string][]string, len(entries))

		for _, entry := range entries {
			name := entry.Name()
			if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
				continue
			}

			raw, err := os.ReadFile(filepath.Join(".", name))
			Expect(err).NotTo(HaveOccurred())

			file, err := parser.ParseFile(token.NewFileSet(), name, raw, 0)
			Expect(err).NotTo(HaveOccurred())

			ast.Inspect(file, func(n ast.Node) bool {
				lit, ok := n.(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					return true
				}

				value, err := strconv.Unquote(lit.Value)
				Expect(err).NotTo(HaveOccurred())

				out[name] = append(out[name], value)

				return true
			})
		}

		Expect(out).NotTo(BeEmpty(), "found no sources to read")

		return out
	}

	It("names the closed state with the workflow package's constant", func() {
		for name, values := range literals() {
			Expect(values).NotTo(ContainElement("closed"),
				name+" spells the closed state itself; use workflow.Closed")
		}
	})
})
