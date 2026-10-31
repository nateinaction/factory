package githubapp_test

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/nateinaction/factory/foreman/internal/githubapp"
)

var _ = Describe("read-only permissions", func() {
	It("are reads only, and omit what the agent role's App lacks", func() {
		got, err := githubapp.ReadOnlyPermissions("curator")
		Expect(err).NotTo(HaveOccurred())
		Expect(got).To(Equal(map[string]string{"issues": "read", "metadata": "read"}))
	})

	It("fail closed for an agent role with none", func() {
		_, err := githubapp.ReadOnlyPermissions("paged")
		Expect(err).To(HaveOccurred())
	})
})
