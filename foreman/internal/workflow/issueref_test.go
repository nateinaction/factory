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

var _ = Describe("ParseIssueRef", func() {
	It("parses the repository and issue number arguments once, into one value", func() {
		ref, err := workflow.ParseIssueRef("o/r", "5")
		Expect(err).NotTo(HaveOccurred())
		Expect(ref.Repo.String()).To(Equal("o/r"))
		Expect(ref.Number).To(Equal(5))
		Expect(ref.String()).To(Equal("o/r#5"))
	})

	DescribeTable("returns an error, never a value, when either part is bad",
		func(repo, number string) {
			ref, err := workflow.ParseIssueRef(repo, number)
			Expect(err).To(HaveOccurred())
			Expect(ref).To(Equal(workflow.IssueRef{}))
		},
		Entry(nil, "nope", "5"), Entry(nil, "o/r", "x"), Entry(nil, "o/r", "-1"),
	)
})

var _ = Describe("IssueRef", func() {
	It("formats as owner/name#N for logs", func() {
		repo, err := workflow.ParseRepo("nateinaction/factory")
		Expect(err).NotTo(HaveOccurred())
		Expect(workflow.IssueRef{Repo: repo, Number: 61}.String()).To(Equal("nateinaction/factory#61"))
	})
})

// mustRepo parses a repository name the test knows is valid.
func mustRepo(s string) workflow.Repo {
	repo, err := workflow.ParseRepo(s)
	Expect(err).NotTo(HaveOccurred())

	return repo
}
