package workflows_test

import (
	"fmt"
	"os"
	"path/filepath"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"go.yaml.in/yaml/v3"
)

// topLevelPermissions parses a workflow and returns its top-level permissions block. A workflow
// with no block, or one that is not a mapping (`write-all`), is an error: only an explicit mapping
// can be checked.
func topLevelPermissions(workflow []byte) (map[string]string, error) {
	var doc struct {
		Permissions any `yaml:"permissions"`
	}
	if err := yaml.Unmarshal(workflow, &doc); err != nil {
		return nil, fmt.Errorf("parse workflow: %w", err)
	}
	block, ok := doc.Permissions.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("top-level permissions is %T, want a mapping", doc.Permissions)
	}
	granted := make(map[string]string, len(block))
	for scope, level := range block {
		granted[scope] = fmt.Sprint(level)
	}
	return granted, nil
}

var _ = Describe("a workflow's GITHUB_TOKEN permissions", func() {
	DescribeTable("the checker",
		func(workflow string, want map[string]string) {
			got, err := topLevelPermissions([]byte(workflow))
			Expect(err).NotTo(HaveOccurred())
			Expect(got).To(Equal(want))
		},
		Entry("reads contents only", "permissions:\n  contents: read\njobs: {}\n", map[string]string{"contents": "read"}),
		Entry("also writes", "permissions:\n  contents: read\n  pull-requests: write\n", map[string]string{"contents": "read", "pull-requests": "write"}),
		Entry("writes contents", "permissions:\n  contents: write\n", map[string]string{"contents": "write"}),
		Entry("grants nothing", "permissions: {}\n", map[string]string{}),
	)

	DescribeTable("the checker rejects a workflow it cannot check",
		func(workflow string) {
			_, err := topLevelPermissions([]byte(workflow))
			Expect(err).To(HaveOccurred())
		},
		Entry("no block", "jobs: {}\n"),
		Entry("a blanket grant", "permissions: write-all\n"),
	)

	It("is exactly contents: read in every workflow", func() {
		paths, err := filepath.Glob("*.yml")
		Expect(err).NotTo(HaveOccurred())
		Expect(paths).NotTo(BeEmpty(), "no workflows found beside this spec")
		for _, path := range paths {
			workflow, err := os.ReadFile(path)
			Expect(err).NotTo(HaveOccurred())
			granted, err := topLevelPermissions(workflow)
			Expect(err).NotTo(HaveOccurred(), path)
			Expect(granted).To(Equal(map[string]string{"contents": "read"}), path)
		}
	})
})
