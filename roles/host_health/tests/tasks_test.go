package tests_test

import (
	"os"
	"slices"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"go.yaml.in/yaml/v3"
)

// The host_health role's tasks build the reporter on the controller for every host. These specs
// read the task list, since running it needs real hosts.
var _ = Describe("the host_health role's build tasks", func() {
	var tasks []map[string]any

	// index is the position of the task named name.
	index := func(name string) int {
		GinkgoHelper()
		i := slices.IndexFunc(tasks, func(t map[string]any) bool { return t["name"] == name })
		Expect(i).To(BeNumerically(">=", 0), "no task named %q", name)
		return i
	}

	BeforeEach(func() {
		raw, err := os.ReadFile("../tasks/main.yml")
		Expect(err).NotTo(HaveOccurred())
		Expect(yaml.Unmarshal(raw, &tasks)).To(Succeed())
	})

	It("checks the architecture is supported before it looks the architecture up", func() {
		Expect(tasks[index("Check that the CPU architecture is supported")]).To(HaveKey("ansible.builtin.assert"))
		Expect(index("Check that the CPU architecture is supported")).
			To(BeNumerically("<", index("Map the CPU architecture to Go's naming")))
	})

	It("builds once per architecture, not once per host, so hosts do not race on one output path", func() {
		build := tasks[index("Build the health reporter for each architecture in the play")]
		Expect(build).To(HaveKeyWithValue("run_once", true))
		Expect(build).To(HaveKey("loop"))
	})
})
