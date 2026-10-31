package tests_test

import (
	"os"
	"path/filepath"
	"slices"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/nateinaction/factory/roletest"
)

const textfileFlag = "--collector.textfile.directory=/var/lib/node_exporter/textfile"

func hasLine(text, line string) bool {
	return slices.Contains(strings.Split(text, "\n"), line)
}

// What the host_health role renders: node_exporter on loopback reading the reporter's textfile
// directory, and the oneshot unit and timer that run the reporter. Run against the real group_vars.
var _ = Describe("the host_health role's units", Ordered, func() {
	var dir string

	file := func(name string) string {
		GinkgoHelper()
		b, err := os.ReadFile(filepath.Join(dir, name))
		Expect(err).NotTo(HaveOccurred())
		return string(b)
	}

	BeforeAll(func() {
		dir = roletest.Render("render.yml", roletest.Options{}).Dir
	})

	It("node_exporter listens on loopback only", func() {
		Expect(hasLine(file("node_exporter.service"),
			"ExecStart=/nix/var/nix/profiles/default/bin/node_exporter --web.listen-address=127.0.0.1:9100 --collector.disable-defaults --collector.meminfo --collector.filesystem --collector.textfile "+textfileFlag)).To(BeTrue())
	})

	It("node_exporter reads the reporter's textfile directory", func() {
		Expect(file("node_exporter.service")).To(ContainSubstring(textfileFlag))
	})

	It("the reporter runs as a oneshot unit", func() {
		unit := file("factory-host-health.service")
		Expect(hasLine(unit, "Type=oneshot")).To(BeTrue())
		Expect(hasLine(unit, "ExecStart=/usr/local/bin/factory-host-health")).To(BeTrue())
	})

	It("the reporter writes where node_exporter reads", func() {
		Expect(hasLine(file("factory-host-health.service"),
			"Environment=TEXTFILE_DIR=/var/lib/node_exporter/textfile")).To(BeTrue())
		Expect(file("node_exporter.service")).To(ContainSubstring(textfileFlag))
	})

	It("the reporter runs every 30 seconds", func() {
		timer := file("factory-host-health.timer")
		Expect(hasLine(timer, "OnUnitActiveSec=30s")).To(BeTrue())
		Expect(hasLine(timer, "Unit=factory-host-health.service")).To(BeTrue())
	})
})
