package tests_test

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/nateinaction/factory/roletest"
)

// reporterTimeout bounds one run of the reporter.
const reporterTimeout = time.Minute

// reporterSource is the reporter's main package, built for the machine running the specs.
const reporterSource = "../cmd/factory-host-health"

// The reporter is run as a black box: its inputs are the textfile directory, the thermal zone and
// the vcgencmd path, and its outputs are its exit code and the metric file, so only the executable
// path changes if it is rewritten.
var _ = Describe("the host health reporter", Ordered, func() {
	var reporter, textfile, thermal, vcgencmd, metrics string

	// report runs the reporter with the fake inputs plus extra environment, returning its exit
	// code and combined output.
	report := func(extra ...string) (int, string) {
		GinkgoHelper()
		ctx, cancel := context.WithTimeout(context.Background(), reporterTimeout)
		defer cancel()
		cmd := exec.CommandContext(ctx, reporter)
		cmd.Env = append(os.Environ(),
			"TEXTFILE_DIR="+textfile, "THERMAL_ZONE="+thermal, "VCGENCMD="+vcgencmd)
		cmd.Env = append(cmd.Env, extra...)
		cmd.WaitDelay = 5 * time.Second
		out, err := cmd.CombinedOutput()
		if err == nil {
			return 0, string(out)
		}
		var exit *exec.ExitError
		Expect(errors.As(err, &exit)).To(BeTrue(), "the reporter did not run: %v", err)
		return exit.ExitCode(), string(out)
	}

	// published is the metric file as series (name and labels) to value.
	published := func() map[string]string {
		GinkgoHelper()
		b, err := os.ReadFile(metrics)
		Expect(err).NotTo(HaveOccurred())
		series := map[string]string{}
		for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
			i := strings.LastIndex(line, " ")
			Expect(i).To(BeNumerically(">", 0), "not a sample: %q", line)
			series[line[:i]] = line[i+1:]
		}
		return series
	}

	BeforeAll(func() {
		dir := GinkgoT().TempDir()
		textfile = filepath.Join(dir, "textfile")
		Expect(os.Mkdir(textfile, 0o755)).To(Succeed())
		thermal = filepath.Join(dir, "temp")
		Expect(os.WriteFile(thermal, []byte("54321\n"), 0o600)).To(Succeed())
		bin := filepath.Join(dir, "bin")
		Expect(os.Mkdir(bin, 0o755)).To(Succeed())
		reporter = filepath.Join(bin, "factory-host-health")
		built, err := exec.Command("go", "build", "-o", reporter, reporterSource).CombinedOutput()
		Expect(err).NotTo(HaveOccurred(), string(built))
		vcgencmd, err = roletest.BuildFake(bin, "vcgencmd")
		Expect(err).NotTo(HaveOccurred())
		metrics = filepath.Join(textfile, "factory_host.prom")

		// 0x50005: under-voltage and throttled, now and since boot.
		code, out := report("FAKE_THROTTLED=0x50005")
		Expect(code).To(BeZero(), out)
	})

	It("the reporter writes the SoC temperature in Celsius", func() {
		Expect(published()).To(HaveKey("factory_host_temperature_celsius"))
	})

	It("the temperature is the millidegrees reading over 1000", func() {
		Expect(published()).To(HaveKeyWithValue("factory_host_temperature_celsius", "54.321"))
	})

	It("node_exporter's DynamicUser can read the reporter's file", func() {
		info, err := os.Stat(metrics)
		Expect(err).NotTo(HaveOccurred())
		Expect(info.Mode().Perm()).To(Equal(os.FileMode(0o644)))
	})

	It("the reporter writes the current throttling bits", func() {
		s := published()
		Expect(s).To(HaveKeyWithValue(`factory_host_throttled{kind="under_voltage",state="now"}`, "1"))
		Expect(s).To(HaveKeyWithValue(`factory_host_throttled{kind="arm_frequency_capped",state="now"}`, "0"))
		Expect(s).To(HaveKeyWithValue(`factory_host_throttled{kind="throttled",state="now"}`, "1"))
		Expect(s).To(HaveKeyWithValue(`factory_host_throttled{kind="soft_temperature_limit",state="now"}`, "0"))
	})

	It("the reporter writes the throttling that happened since boot", func() {
		s := published()
		Expect(s).To(HaveKeyWithValue(`factory_host_throttled{kind="under_voltage",state="since_boot"}`, "1"))
		Expect(s).To(HaveKeyWithValue(`factory_host_throttled{kind="throttled",state="since_boot"}`, "1"))
		Expect(s).To(HaveKeyWithValue(`factory_host_throttled{kind="arm_frequency_capped",state="since_boot"}`, "0"))
	})

	It("the reporter publishes the temperature and the eight throttling series and nothing else", func() {
		Expect(published()).To(HaveLen(9))
	})

	Describe("a failed read", Ordered, func() {
		var lastGood []byte

		BeforeAll(func() {
			var err error
			lastGood, err = os.ReadFile(metrics)
			Expect(err).NotTo(HaveOccurred())
		})

		It("a failed throttling read fails the reporter", func() {
			code, _ := report("FAKE_VCGENCMD_FAIL=1")
			Expect(code).NotTo(BeZero())
		})

		It("a failed read leaves the last good metrics in place", func() {
			b, err := os.ReadFile(metrics)
			Expect(err).NotTo(HaveOccurred())
			Expect(b).To(Equal(lastGood))
		})

		It("a failed temperature read fails the reporter and publishes nothing", func() {
			Expect(os.WriteFile(thermal, []byte("not a number\n"), 0o600)).To(Succeed())
			code, _ := report("FAKE_THROTTLED=0x0")
			Expect(code).NotTo(BeZero())
			b, err := os.ReadFile(metrics)
			Expect(err).NotTo(HaveOccurred())
			Expect(b).To(Equal(lastGood))
		})

		It("a failed read leaves no partial file behind", func() {
			entries, err := os.ReadDir(textfile)
			Expect(err).NotTo(HaveOccurred())
			Expect(entries).To(HaveLen(1))
		})
	})
})
