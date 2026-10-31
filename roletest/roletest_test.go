package roletest_test

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/nateinaction/factory/roletest"
)

const writePlaybook = `---
- name: Write a file into the render directory
  hosts: localhost
  connection: local
  gather_facts: false
  tasks:
    - name: Write it
      ansible.builtin.copy:
        content: "{{ greeting }}\n"
        dest: "{{ render_dir }}/out.txt"
        mode: "0600"
`

const failPlaybook = `---
- name: Fail on purpose
  hosts: localhost
  connection: local
  gather_facts: false
  tasks:
    - name: Fail
      ansible.builtin.fail:
        msg: the marker message
`

const slowPlaybook = `---
- name: Outlive the timeout
  hosts: localhost
  connection: local
  gather_facts: false
  tasks:
    - name: Wait
      ansible.builtin.pause:
        seconds: 30
`

func playbook(content string) string {
	path := filepath.Join(GinkgoT().TempDir(), "play.yml")
	Expect(os.WriteFile(path, []byte(content), 0o600)).To(Succeed())
	return path
}

var _ = Describe("rendering a playbook", func() {
	It("renders into a directory of its own for each call", func() {
		pb := playbook(writePlaybook)
		a, err := roletest.TryRender(pb, roletest.Options{Vars: map[string]any{"greeting": "hello"}})
		Expect(err).NotTo(HaveOccurred())
		b, err := roletest.TryRender(pb, roletest.Options{Vars: map[string]any{"greeting": "hello"}})
		Expect(err).NotTo(HaveOccurred())

		Expect(a.Dir).NotTo(Equal(b.Dir))
		got, err := os.ReadFile(filepath.Join(a.Dir, "out.txt"))
		Expect(err).NotTo(HaveOccurred())
		Expect(string(got)).To(Equal("hello\n"))
	})

	It("reports the playbook's log when the render fails", func() {
		_, err := roletest.TryRender(playbook(failPlaybook), roletest.Options{})
		Expect(err).To(MatchError(ContainSubstring("the marker message")))
	})

	It("gives up on a playbook that outlives the timeout", func() {
		started := time.Now()
		_, err := roletest.TryRender(playbook(slowPlaybook), roletest.Options{Timeout: 2 * time.Second})
		Expect(err).To(HaveOccurred())
		Expect(time.Since(started)).To(BeNumerically("<", 20*time.Second))
	})
})

var _ = Describe("building a fake binary", func() {
	It("runs the test binary under the fake's name", func() {
		path, err := roletest.BuildFake(GinkgoT().TempDir(), "fake-echo")
		Expect(err).NotTo(HaveOccurred())
		Expect(filepath.Base(path)).To(Equal("fake-echo"))

		out, err := exec.Command(path, "a", "b").Output()
		var exit *exec.ExitError
		Expect(errors.As(err, &exit)).To(BeTrue())
		Expect(exit.ExitCode()).To(Equal(7))
		Expect(string(out)).To(Equal("fake-echo a b\n"))
	})
})
