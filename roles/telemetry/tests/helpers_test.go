package tests_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"go.yaml.in/yaml/v3"
)

// groupVars is the repo's real group_vars file, which a spec reads for what must not be set there.
const groupVars = "../../../group_vars/factory.yml"

func readFile(path ...string) string {
	GinkgoHelper()
	b, err := os.ReadFile(filepath.Join(path...))
	Expect(err).NotTo(HaveOccurred())
	return string(b)
}

// yamlDoc parses a YAML file into generic maps and slices.
func yamlDoc(path ...string) any {
	GinkgoHelper()
	var doc any
	Expect(yaml.Unmarshal([]byte(readFile(path...)), &doc)).To(Succeed())
	return doc
}

// jsonDoc parses a JSON file into generic maps and slices.
func jsonDoc(path ...string) any {
	GinkgoHelper()
	var doc any
	Expect(json.Unmarshal([]byte(readFile(path...)), &doc)).To(Succeed())
	return doc
}

// at walks a parsed document by map keys (string) and list indexes (int), failing the spec at the
// first step that is not there.
func at(doc any, steps ...any) any {
	GinkgoHelper()
	for _, step := range steps {
		switch s := step.(type) {
		case string:
			m, ok := doc.(map[string]any)
			Expect(ok).To(BeTrue(), "no map before key %q", s)
			doc, ok = m[s]
			Expect(ok).To(BeTrue(), "no key %q", s)
		case int:
			l, ok := doc.([]any)
			Expect(ok).To(BeTrue(), "no list before index %d", s)
			Expect(s).To(BeNumerically("<", len(l)), "no index %d", s)
			doc = l[s]
		default:
			Fail("a step is a string or an int")
		}
	}
	return doc
}

// list is a parsed list, failing the spec if doc is not one.
func list(doc any) []any {
	GinkgoHelper()
	l, ok := doc.([]any)
	Expect(ok).To(BeTrue(), "not a list: %v", doc)
	return l
}

// text is a parsed string, failing the spec if doc is not one.
func text(doc any) string {
	GinkgoHelper()
	s, ok := doc.(string)
	Expect(ok).To(BeTrue(), "not a string: %v", doc)
	return s
}

func hasLine(content, line string) bool {
	for _, l := range strings.Split(content, "\n") {
		if l == line {
			return true
		}
	}
	return false
}

// decodeYAML parses YAML text into out.
func decodeYAML(content string, out any) {
	GinkgoHelper()
	Expect(yaml.Unmarshal([]byte(content), out)).To(Succeed())
}
