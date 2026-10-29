package github_test

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

var _ = Describe("the client's source", func() {
	// sources reads the package's non-test files, keyed by file name. The shape of the package is what
	// these specs judge, so they read it rather than call it.
	sources := func() map[string]string {
		GinkgoHelper()

		entries, err := os.ReadDir(".")
		Expect(err).NotTo(HaveOccurred())

		out := make(map[string]string, len(entries))

		for _, entry := range entries {
			name := entry.Name()
			if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
				continue
			}

			raw, err := os.ReadFile(filepath.Join(".", name))
			Expect(err).NotTo(HaveOccurred())

			out[name] = string(raw)
		}

		Expect(out).NotTo(BeEmpty(), "found no sources to read")

		return out
	}

	// literals lists every string a file defines, comments excluded: a comment may name a path or a
	// state without building one.
	literals := func(src string) []string {
		GinkgoHelper()

		file, err := parser.ParseFile(token.NewFileSet(), "source.go", src, 0)
		Expect(err).NotTo(HaveOccurred())

		var out []string

		ast.Inspect(file, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}

			value, err := strconv.Unquote(lit.Value)
			Expect(err).NotTo(HaveOccurred())

			out = append(out, value)

			return true
		})

		return out
	}

	It("sends every request from one function", func() {
		sent := 0

		for _, src := range sources() {
			sent += strings.Count(src, ".hc.Do(")
		}

		Expect(sent).To(Equal(1),
			"a second request path repeats the close, the size cap and the status check")
	})

	It("names the closed state with the workflow package's constant", func() {
		for name, src := range sources() {
			Expect(literals(src)).NotTo(ContainElement("closed"),
				name+" spells the closed state itself; use workflow.Closed")
		}
	})

	It("builds every repository path from the one builder", func() {
		for name, src := range sources() {
			for _, literal := range literals(src) {
				Expect(literal).NotTo(ContainSubstring("/repos"),
					name+" builds an API path by hand; repoPath escapes every part for the whole client")
			}
		}
	})
})
