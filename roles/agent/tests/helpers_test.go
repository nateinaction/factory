package tests_test

import (
	"os"
	"path/filepath"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"go.yaml.in/yaml/v3"

	"github.com/nateinaction/factory/roletest"
)

// tree is a parsed YAML or JSON document. JSON is YAML, so one parser reads every rendered file.
type tree = map[string]any

// render runs the agent role's render playbook with the extra vars and returns the directory it wrote.
func render(vars map[string]any) string {
	GinkgoHelper()
	return roletest.Render("render.yml", roletest.Options{Vars: vars}).Dir
}

func readText(path string) string {
	GinkgoHelper()
	b, err := os.ReadFile(path)
	Expect(err).NotTo(HaveOccurred())
	return string(b)
}

// parse reads a YAML or JSON file into a tree.
func parse(path string) tree {
	GinkgoHelper()
	var t tree
	Expect(yaml.Unmarshal([]byte(readText(path)), &t)).To(Succeed(), path)
	return t
}

// parseNode reads a YAML or JSON file keeping the order of its keys, for rules where order decides.
func parseNode(path string) *yaml.Node {
	GinkgoHelper()
	var doc yaml.Node
	Expect(yaml.Unmarshal([]byte(readText(path)), &doc)).To(Succeed(), path)
	Expect(doc.Content).To(HaveLen(1), path)
	return doc.Content[0]
}

// at follows keys down through nested maps and fails the spec where one is missing.
func at(t any, keys ...string) any {
	GinkgoHelper()
	cur := t
	for _, k := range keys {
		m, ok := cur.(tree)
		Expect(ok).To(BeTrue(), "%v is not a map at %q", cur, k)
		cur, ok = m[k]
		Expect(ok).To(BeTrue(), "no key %q", k)
	}
	return cur
}

func treeAt(t any, keys ...string) tree {
	GinkgoHelper()
	m, ok := at(t, keys...).(tree)
	Expect(ok).To(BeTrue(), "not a map at %v", keys)
	return m
}

func listAt(t any, keys ...string) []any {
	GinkgoHelper()
	l, ok := at(t, keys...).([]any)
	Expect(ok).To(BeTrue(), "not a list at %v", keys)
	return l
}

// strs converts a list of scalars to strings.
func strs(l []any) []string {
	GinkgoHelper()
	out := make([]string, 0, len(l))
	for _, v := range l {
		s, ok := v.(string)
		Expect(ok).To(BeTrue(), "%v is not a string", v)
		out = append(out, s)
	}
	return out
}

func has(t tree, key string) bool {
	_, ok := t[key]
	return ok
}

// childOf returns the value node of key in a mapping node, or nil.
func childOf(n *yaml.Node, key string) *yaml.Node {
	for i := 0; i+1 < len(n.Content); i += 2 {
		if n.Content[i].Value == key {
			return n.Content[i+1]
		}
	}
	return nil
}

func nodeAt(n *yaml.Node, keys ...string) *yaml.Node {
	GinkgoHelper()
	for _, k := range keys {
		n = childOf(n, k)
		Expect(n).NotTo(BeNil(), "no key %q", k)
	}
	return n
}

// files lists every file under dir, so a spec can scan a whole render.
func files(dir string) []string {
	GinkgoHelper()
	var out []string
	err := filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			out = append(out, p)
		}
		return nil
	})
	Expect(err).NotTo(HaveOccurred())
	return out
}
