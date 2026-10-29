package workflow_test

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/nateinaction/factory/foreman/internal/workflow"
)

var _ = Describe("ParseRepo", func() {
	It("returns the validated owner/name", func() {
		repo, err := workflow.ParseRepo("nateinaction/factory.x_y-z")
		Expect(err).NotTo(HaveOccurred())
		Expect(repo.String()).To(Equal("nateinaction/factory.x_y-z"))
	})

	DescribeTable("returns an error, never a value, for anything else",
		func(s string) {
			repo, err := workflow.ParseRepo(s)
			Expect(err).To(HaveOccurred())
			Expect(repo.String()).To(BeEmpty())
		},
		Entry(nil, ""), Entry(nil, "a"), Entry(nil, "a/b/c"), Entry(nil, "a/b c"), Entry(nil, "a/../b?x"),
		Entry(nil, "/b"), Entry(nil, "a/b\n"), Entry(nil, "../etc"), Entry(nil, "a/.."),
	)
})

var _ = Describe("IssueRef", func() {
	It("formats as owner/name#N for logs", func() {
		repo, err := workflow.ParseRepo("nateinaction/factory")
		Expect(err).NotTo(HaveOccurred())
		Expect(workflow.IssueRef{Repo: repo, Number: 61}.String()).To(Equal("nateinaction/factory#61"))
	})
})
