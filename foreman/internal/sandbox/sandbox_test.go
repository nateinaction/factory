package sandbox

import (
	"encoding/json"
	"path/filepath"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/nateinaction/factory/foreman/internal/workflow"
)

var _ = Describe("Spec", func() {
	layout := Layout{
		RunDir:    "/work/run-implementer-5",
		Clone:     "/work/issue-5",
		Scratch:   "/work/implementer-5",
		Output:    "/work/implementer-5/retro.md",
		Skills:    "/opt/skills",
		Toolchain: []string{"/nix/store/abc-go-1.27"},
		Hidden:    []string{"/home/factory", "/work", "/var/lib/dagu", "/run/factory"},
	}

	It("gives the Implementer a writable clone, the run directory and its output", func() {
		spec := Plan(workflow.Implementer, layout)
		Expect(spec.ReadWrite).To(ConsistOf(layout.RunDir, layout.Scratch, layout.Clone))
		Expect(spec.ReadOnly).To(ConsistOf(layout.Skills, "/nix/store/abc-go-1.27"))
		Expect(spec.DenyWrite).To(ConsistOf(filepath.Join(layout.Clone, ".git")),
			"the model may edit the working tree but never the clone's history")
		Expect(spec.Hidden).To(ConsistOf("/home/factory", "/work", "/var/lib/dagu", "/run/factory"))
	})

	It("gives the Reviewer a writable throwaway clone", func() {
		spec := Plan(workflow.Reviewer, layout)
		Expect(spec.ReadWrite).To(ContainElement(layout.Clone))
	})

	DescribeTable("gives an agent role that never commits a read-only clone",
		func(role workflow.Role) {
			spec := Plan(role, layout)
			Expect(spec.ReadWrite).NotTo(ContainElement(layout.Clone))
			Expect(spec.ReadOnly).To(ContainElement(layout.Clone))
		},
		Entry(nil, workflow.Planner), Entry(nil, workflow.Researcher), Entry(nil, workflow.Curator),
	)

	It("compiles to srt settings with the run's egress proxy and every hidden path", func() {
		spec := Plan(workflow.Implementer, layout)
		raw, err := Settings(spec, 43210)
		Expect(err).NotTo(HaveOccurred())

		var got struct {
			Network struct {
				AllowedDomains []string `json:"allowedDomains"`
				DeniedDomains  []string `json:"deniedDomains"`
				HTTPProxyPort  int      `json:"httpProxyPort"`
			} `json:"network"`
			Filesystem struct {
				AllowRead  []string `json:"allowRead"`
				AllowWrite []string `json:"allowWrite"`
				DenyRead   []string `json:"denyRead"`
				DenyWrite  []string `json:"denyWrite"`
			} `json:"filesystem"`
		}
		Expect(json.Unmarshal(raw, &got)).To(Succeed())

		Expect(got.Network.HTTPProxyPort).To(Equal(43210), "srt must send egress through foreman's proxy")
		Expect(got.Network.AllowedDomains).To(BeEmpty(), "the egress proxy enforces the allowlist")
		Expect(got.Network.DeniedDomains).To(BeEmpty())
		Expect(got.Filesystem.DenyRead).To(ConsistOf(append([]string{"/"}, layout.Hidden...)),
			"the host is hidden by default, not only the named sensitive paths")
		Expect(got.Filesystem.AllowRead).To(ContainElements(
			"/nix/store", "/usr", "/private/var", layout.Skills, layout.RunDir, layout.Clone),
			"the operating system and this run's paths are bound back")
		Expect(got.Filesystem.AllowRead).NotTo(ContainElements("/opt", "/Library"),
			"a path outside the toolchain's needs must not widen the documented allowlist")
		Expect(got.Filesystem.AllowWrite).To(ConsistOf(layout.RunDir, layout.Scratch, layout.Clone),
			"the scratch directory covers the output file")
		Expect(got.Filesystem.DenyWrite).To(ConsistOf(filepath.Join(layout.Clone, ".git")))
	})

	It("leaves another run's scratch directory and clone out of the run's visible paths", func() {
		spec := Plan(workflow.Implementer, layout)
		raw, err := Settings(spec, 43210)
		Expect(err).NotTo(HaveOccurred())

		var got struct {
			Filesystem struct {
				AllowRead []string `json:"allowRead"`
				DenyRead  []string `json:"denyRead"`
			} `json:"filesystem"`
		}
		Expect(json.Unmarshal(raw, &got)).To(Succeed())

		otherRun := "/work/run-implementer-6"
		otherScratch := "/work/implementer-6"

		Expect(got.Filesystem.DenyRead).To(ContainElement("/work"), "the work root holds every run")
		Expect(got.Filesystem.AllowRead).NotTo(ContainElement(otherRun))
		Expect(got.Filesystem.AllowRead).NotTo(ContainElement(otherScratch))
		Expect(got.Filesystem.AllowRead).NotTo(ContainElement("/work/**"),
			"a work-root wildcard would reopen every run")
	})

	It("refuses a settings file with no proxy port", func() {
		_, err := Settings(Plan(workflow.Implementer, layout), 0)
		Expect(err).To(MatchError(ContainSubstring("proxy port")))
	})

	It("writes empty path lists as arrays, which srt accepts and null it refuses", func() {
		raw, err := Settings(Spec{}, 1234)
		Expect(err).NotTo(HaveOccurred())
		Expect(string(raw)).To(ContainSubstring(`"denyRead":["/"]`), "the host is hidden even with no paths")
		Expect(string(raw)).To(ContainSubstring(`"denyWrite":[]`))
		Expect(string(raw)).To(ContainSubstring(`"allowWrite":[]`))
		Expect(string(raw)).To(ContainSubstring(`"allowRead":["/nix/store"`), "the OS is bound back")
	})
})

var _ = Describe("Srt", func() {
	It("wraps the whole command in srt with the run's settings", func() {
		argv := Argv("/usr/local/bin/srt", "/work/run/srt.json", []string{"opencode", "run", "hi"})
		Expect(argv).To(Equal([]string{
			"/usr/local/bin/srt", "--settings", "/work/run/srt.json", "--", "opencode", "run", "hi",
		}))
	})

	It("refuses to build a command without a backend or settings", func() {
		Expect(Argv("", "/s.json", []string{"opencode"})).To(BeNil())
		Expect(Argv("/usr/local/bin/srt", "", []string{"opencode"})).To(BeNil())
		Expect(Argv("/usr/local/bin/srt", "/s.json", nil)).To(BeNil())
	})

	DescribeTable("serves only the worker operating systems srt supports",
		func(osName string, ok bool) {
			err := Supported(osName)
			if ok {
				Expect(err).NotTo(HaveOccurred())
			} else {
				Expect(err).To(MatchError(ContainSubstring("cannot serve")))
			}
		},
		Entry(nil, "linux", true),
		Entry(nil, "darwin", true),
		Entry(nil, "windows", false),
		Entry(nil, "freebsd", false),
	)
})

var _ = Describe("Network", func() {
	It("is open when no allowlist is set", func() {
		n, err := ParseNetwork(nil)
		Expect(err).NotTo(HaveOccurred())
		Expect(n.Open()).To(BeTrue())
		Expect(n.Permits("anything.example")).To(BeTrue())
	})

	It("permits an exact host and a wildcard's subdomains only", func() {
		n, err := ParseNetwork([]string{"api.example.com", "*.npmjs.org"})
		Expect(err).NotTo(HaveOccurred())
		Expect(n.Open()).To(BeFalse())
		Expect(n.Permits("api.example.com")).To(BeTrue())
		Expect(n.Permits("API.EXAMPLE.COM")).To(BeTrue())
		Expect(n.Permits("other.example.com")).To(BeFalse())
		Expect(n.Permits("cdn.npmjs.org")).To(BeTrue())
		Expect(n.Permits("npmjs.org")).To(BeFalse(), "a wildcard matches strict subdomains")
	})

	DescribeTable("refuses an entry that would not mean what it says",
		func(entry string) {
			_, err := ParseNetwork([]string{entry})
			Expect(err).To(MatchError(ContainSubstring("invalid egress domain")))
		},
		Entry(nil, "*"),
		Entry(nil, "*.com"),
		Entry(nil, "https://example.com"),
		Entry(nil, "example.com/path"),
		Entry(nil, "example.com:443"),
		Entry(nil, ""),
	)
})
