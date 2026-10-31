// Command factory-host-health writes the two host health values node_exporter cannot read on a
// Raspberry Pi: the SoC temperature and the firmware's throttling bits. The output is Prometheus
// text for node_exporter's textfile collector; the host's collector keeps only these families and
// the memory and filesystem ones, and forwards them to the telemetry backend.
//
// The inputs are overridable (TEXTFILE_DIR, THERMAL_ZONE, VCGENCMD) so the role's specs can run
// this without a Pi. A failed read publishes nothing, so the last good file keeps being served:
// readers see a stale value rather than a partial one until a read succeeds.
package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	defaultTextfile = "/var/lib/node_exporter/textfile"
	defaultThermal  = "/sys/class/thermal/thermal_zone0/temp"
	defaultVcgencmd = "/usr/bin/vcgencmd"
	metricsFile     = "factory_host.prom"
	// vcgencmdTimeout bounds one firmware query; the timer starts the next run anyway.
	vcgencmdTimeout = 30 * time.Second
	// maxThermalBytes bounds the thermal zone read; the file holds a few digits.
	maxThermalBytes = 64
)

// throttleBits are the firmware's four bits. Bit n is the state now; bit n+16 is that state since boot.
var throttleBits = [...]struct {
	bit  uint
	kind string
}{
	{0, "under_voltage"},
	{1, "arm_frequency_capped"},
	{2, "throttled"},
	{3, "soft_temperature_limit"},
}

func main() {
	if err := run(os.Getenv); err != nil {
		fmt.Fprintln(os.Stderr, "factory-host-health:", err)
		os.Exit(1)
	}
}

// run reads the host, renders the metrics and publishes them, or returns the first failure with
// nothing published.
func run(getenv func(string) string) error {
	textfile := orDefault(getenv("TEXTFILE_DIR"), defaultTextfile)
	thermal := orDefault(getenv("THERMAL_ZONE"), defaultThermal)
	vcgencmd := getenv("VCGENCMD")
	if vcgencmd == "" {
		if found, err := exec.LookPath("vcgencmd"); err == nil {
			vcgencmd = found
		} else {
			vcgencmd = defaultVcgencmd
		}
	}

	millidegrees, err := readMillidegrees(thermal)
	if err != nil {
		return err
	}
	mask, err := readThrottled(vcgencmd)
	if err != nil {
		return err
	}
	return publish(filepath.Join(textfile, metricsFile), render(millidegrees, mask))
}

func orDefault(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}

func readMillidegrees(path string) (uint64, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, fmt.Errorf("cannot read %s: %w", path, err)
	}
	defer f.Close() // read-only; a close failure loses nothing

	buf := make([]byte, maxThermalBytes)
	n, err := f.Read(buf)
	if err != nil {
		return 0, fmt.Errorf("cannot read %s: %w", path, err)
	}
	return parseMillidegrees(path, string(buf[:n]))
}

// parseMillidegrees parses the thermal zone's contents, which are non-negative millidegrees Celsius.
func parseMillidegrees(path, raw string) (uint64, error) {
	text := strings.TrimSpace(raw)
	v, err := strconv.ParseUint(text, 10, 32)
	if err != nil {
		return 0, fmt.Errorf("%s does not hold millidegrees Celsius: %q", path, text)
	}
	return v, nil
}

func readThrottled(vcgencmd string) (uint64, error) {
	ctx, cancel := context.WithTimeout(context.Background(), vcgencmdTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, vcgencmd, "get_throttled")
	cmd.WaitDelay = 5 * time.Second
	out, err := cmd.Output()
	if err != nil {
		return 0, fmt.Errorf("%s get_throttled failed: %w", vcgencmd, err)
	}
	return parseThrottled(vcgencmd, string(out))
}

// parseThrottled parses `throttled=0x<hex>`.
func parseThrottled(vcgencmd, raw string) (uint64, error) {
	text := strings.TrimSpace(raw)
	digits, ok := strings.CutPrefix(text, "throttled=0x")
	if !ok {
		return 0, fmt.Errorf("%s get_throttled printed an unexpected value: %q", vcgencmd, text)
	}
	mask, err := strconv.ParseUint(digits, 16, 32)
	if err != nil {
		return 0, fmt.Errorf("%s get_throttled printed an unexpected value: %q", vcgencmd, text)
	}
	return mask, nil
}

// render is the metric file's text for a reading.
func render(millidegrees, mask uint64) string {
	var b strings.Builder
	fmt.Fprintf(&b, "factory_host_temperature_celsius %d.%03d\n", millidegrees/1000, millidegrees%1000)
	for _, t := range throttleBits {
		fmt.Fprintf(&b, "factory_host_throttled{kind=%q,state=\"now\"} %d\n", t.kind, (mask>>t.bit)&1)
		fmt.Fprintf(&b, "factory_host_throttled{kind=%q,state=\"since_boot\"} %d\n", t.kind, (mask>>(t.bit+16))&1)
	}
	return b.String()
}

// publish replaces path with content atomically: it writes a temporary file in the same directory
// and renames it over path, so node_exporter reads either the old file or the new one.
func publish(path, content string) (err error) {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".")
	if err != nil {
		return fmt.Errorf("cannot create a file in %s: %w", filepath.Dir(path), err)
	}
	defer func() {
		if err != nil {
			// The original failure is the one to report; the temp file is best-effort cleanup.
			_ = os.Remove(tmp.Name())
		}
	}()

	if _, err = tmp.WriteString(content); err != nil {
		_ = tmp.Close() // already failing; the write error is the one reported
		return fmt.Errorf("cannot write %s: %w", tmp.Name(), err)
	}
	// CreateTemp makes the file 0600; node_exporter runs as a DynamicUser and must read it.
	if err = tmp.Chmod(0o644); err != nil {
		_ = tmp.Close() // already failing
		return fmt.Errorf("cannot make %s readable: %w", tmp.Name(), err)
	}
	if err = tmp.Close(); err != nil {
		return fmt.Errorf("cannot close %s: %w", tmp.Name(), err)
	}
	if err = os.Rename(tmp.Name(), path); err != nil {
		return fmt.Errorf("cannot replace %s: %w", path, err)
	}
	return nil
}
