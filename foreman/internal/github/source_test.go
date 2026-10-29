package github_test

import (
	"os"
	"path/filepath"
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

	It("sends every request from one function", func() {
		sent := 0

		for _, src := range sources() {
			sent += strings.Count(src, ".hc.Do(")
		}

		Expect(sent).To(Equal(1),
			"a second request path repeats the close, the size cap and the status check")
	})

	It("builds every repository path from the one builder", func() {
		for name, src := range sources() {
			Expect(src).NotTo(ContainSubstring(`"/repos`),
				name+" builds an API path by hand; repoPath escapes every part for the whole client")
		}
	})
})
