package tests_test

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/nateinaction/factory/roletest"
)

// bannerTimeout bounds one run of the banner.
const bannerTimeout = time.Minute

// bannerSource is the banner's main package, built for the machine running the specs.
const bannerSource = "../cmd/factory-status"

const (
	green  = "\x1b[32m"
	yellow = "\x1b[33m"
	red    = "\x1b[31m"
)

var ansi = regexp.MustCompile("\x1b\\[[0-9;]*m")

// The banner is run as a black box: its inputs are a proc directory, a thermal zone, a systemctl
// and a filesystem path, and its output is its exit code and the text it prints, so only the
// executable path changes if it is rewritten.
var _ = Describe("the login status banner", Ordered, func() {
	var banner, fakeSystemctlPath, proc, thermal, systemctl, root string

	// write replaces the proc file name's contents.
	write := func(name, contents string) {
		GinkgoHelper()
		Expect(os.WriteFile(filepath.Join(proc, name), []byte(contents), 0o600)).To(Succeed())
	}

	// meminfo gives a host with 1000 MiB of which usedPct percent is used.
	meminfo := func(usedPct int) string {
		total := 1000 * 1024
		return "MemTotal:       " + strconv.Itoa(total) + " kB\nMemFree: 1 kB\nMemAvailable:   " +
			strconv.Itoa(total-total*usedPct/100) + " kB\n"
	}

	// show runs the banner with the fixtures plus extra environment, returning its exit code and
	// standard output with colours as printed.
	show := func(extra ...string) (int, string) {
		GinkgoHelper()
		ctx, cancel := context.WithTimeout(context.Background(), bannerTimeout)
		defer cancel()
		cmd := exec.CommandContext(ctx, banner)
		cmd.Env = append(os.Environ(),
			"FACTORY_STATUS_PROC="+proc, "FACTORY_STATUS_THERMAL_ZONE="+thermal,
			"FACTORY_STATUS_SYSTEMCTL="+systemctl, "FACTORY_STATUS_DISK="+root)
		cmd.Env = append(cmd.Env, extra...)
		cmd.WaitDelay = 5 * time.Second
		out, err := cmd.Output()
		if err == nil {
			return 0, string(out)
		}
		var exit *exec.ExitError
		Expect(errors.As(err, &exit)).To(BeTrue(), "the banner did not run: %v", err)
		return exit.ExitCode(), string(out)
	}

	// row is the banner's line labelled label, with colours as printed.
	row := func(out, label string) string {
		GinkgoHelper()
		for _, l := range strings.Split(out, "\n") {
			if strings.HasPrefix(ansi.ReplaceAllString(l, ""), "  "+label) {
				return l
			}
		}
		return ""
	}

	plain := func(s string) string { return ansi.ReplaceAllString(s, "") }

	BeforeAll(func() {
		dir := GinkgoT().TempDir()
		bin := filepath.Join(dir, "bin")
		Expect(os.Mkdir(bin, 0o755)).To(Succeed())
		banner = filepath.Join(bin, "factory-status")
		built, err := exec.Command("go", "build", "-o", banner, bannerSource).CombinedOutput()
		Expect(err).NotTo(HaveOccurred(), string(built))
		fakeSystemctlPath, err = roletest.BuildFake(bin, "systemctl")
		Expect(err).NotTo(HaveOccurred())
	})

	BeforeEach(func() {
		dir := GinkgoT().TempDir()
		systemctl = fakeSystemctlPath
		proc = filepath.Join(dir, "proc")
		Expect(os.MkdirAll(filepath.Join(proc, "device-tree"), 0o755)).To(Succeed())
		root = dir
		thermal = filepath.Join(dir, "temp")
		write("loadavg", "0.10 0.20 0.30 1/100 4242\n")
		write("meminfo", meminfo(50))
		write("uptime", "183845.12 400000.00\n")
		write("device-tree/model", "Raspberry Pi 4 Model B Rev 1.5\x00")
		Expect(os.WriteFile(thermal, []byte("54321\n"), 0o600)).To(Succeed())
	})

	It("the banner exits zero so a login never fails", func() {
		code, _ := show()
		Expect(code).To(BeZero())
	})

	It("the banner names the host and its board", func() {
		host, err := os.Hostname()
		Expect(err).NotTo(HaveOccurred())
		_, out := show()
		Expect(plain(out)).To(ContainSubstring(host + "  Raspberry Pi 4 Model B Rev 1.5"))
	})

	It("the banner names the architecture when the host has no device tree", func() {
		Expect(os.Remove(filepath.Join(proc, "device-tree", "model"))).To(Succeed())
		_, out := show()
		Expect(plain(out)).To(MatchRegexp(`(aarch64|x86_64|\w+)\n`))
		Expect(plain(out)).NotTo(ContainSubstring("Raspberry"))
	})

	It("the banner words the uptime as uptime -p does", func() {
		_, out := show()
		Expect(plain(out)).To(ContainSubstring("up 2 days, 3 hours, 4 minutes"))
	})

	It("the banner shows the three load averages and the core count", func() {
		_, out := show()
		Expect(plain(row(out, "load"))).To(MatchRegexp(`^  load\s+0\.10 0\.20 0\.30  \(\d+ cores\)$`))
	})

	Describe("the memory bar", func() {
		It("fills half the bar at half used, and says the megabytes", func() {
			_, out := show()
			Expect(plain(row(out, "memory"))).To(Equal("  memory  ##########..........  50%  500 MiB / 1000 MiB"))
		})

		DescribeTable("is coloured by how full it is",
			func(usedPct int, colour string) {
				write("meminfo", meminfo(usedPct))
				_, out := show()
				Expect(row(out, "memory")).To(ContainSubstring(colour + "#"))
			},
			Entry("green below 70%", 69, green),
			Entry("yellow from 70%", 70, yellow),
			Entry("yellow below 90%", 89, yellow),
			Entry("red from 90%", 90, red),
		)

		It("says unavailable, and still exits zero, when meminfo cannot be read", func() {
			Expect(os.Remove(filepath.Join(proc, "meminfo"))).To(Succeed())
			code, out := show()
			Expect(code).To(BeZero())
			Expect(plain(row(out, "memory"))).To(ContainSubstring("unavailable"))
		})
	})

	It("the banner shows the root filesystem's use as a bar and sizes", func() {
		_, out := show()
		Expect(plain(row(out, "disk /"))).To(MatchRegexp(`^  disk /  [#.]{20}\s+\d+%  \S+ / \S+$`))
	})

	Describe("the temperature", func() {
		It("is shown in whole degrees Celsius", func() {
			_, out := show()
			Expect(plain(row(out, "temp"))).To(Equal("  temp    54 C"))
		})

		It("is left out when the host has no thermal zone", func() {
			Expect(os.Remove(thermal)).To(Succeed())
			_, out := show()
			Expect(row(out, "temp")).To(BeEmpty())
		})

		It("says unavailable when the reading is not a number", func() {
			Expect(os.WriteFile(thermal, []byte("hot\n"), 0o600)).To(Succeed())
			code, out := show()
			Expect(code).To(BeZero())
			Expect(plain(row(out, "temp"))).To(ContainSubstring("unavailable"))
		})
	})

	Describe("dagu", func() {
		It("is green, with when it started, while it is active", func() {
			_, out := show("FAKE_DAGU_SINCE=Fri 2026-10-30 08:00:00 UTC")
			Expect(row(out, "dagu")).To(ContainSubstring(green + "active"))
			Expect(plain(row(out, "dagu"))).To(ContainSubstring("since Fri 2026-10-30 08:00:00 UTC"))
		})

		It("is red, with no start time, when it is not active", func() {
			_, out := show("FAKE_DAGU_STATE=inactive")
			Expect(row(out, "dagu")).To(ContainSubstring(red + "inactive"))
			Expect(plain(row(out, "dagu"))).NotTo(ContainSubstring("since"))
		})

		It("says unavailable, and still exits zero, when systemctl fails", func() {
			code, out := show("FAKE_SYSTEMCTL_FAIL=1")
			Expect(code).To(BeZero())
			Expect(plain(row(out, "dagu"))).To(ContainSubstring("unavailable"))
		})

		It("says unavailable, and still exits zero, when there is no systemctl", func() {
			systemctl = filepath.Join(GinkgoT().TempDir(), "missing")
			code, out := show()
			Expect(code).To(BeZero())
			Expect(plain(row(out, "dagu"))).To(ContainSubstring("unavailable"))
		})
	})

	Describe("failed units", func() {
		It("are counted in red when there are some", func() {
			_, out := show("FAKE_FAILED_UNITS=2")
			Expect(row(out, "systemd")).To(ContainSubstring(red + "2 failed unit(s)"))
		})

		It("are not mentioned when there are none", func() {
			_, out := show("FAKE_FAILED_UNITS=0")
			Expect(row(out, "systemd")).To(BeEmpty())
		})
	})
})
