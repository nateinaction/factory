// Command factory-status prints the login status banner: load, memory, disk, temperature and
// dagu. It runs as root through pam_motd on every login, so it must be fast and must never fail
// the login: a value it cannot read is shown as unavailable with the reason, and the exit code is
// always zero.
//
// The inputs are overridable (FACTORY_STATUS_PROC, _THERMAL_ZONE, _SYSTEMCTL, _DISK) so the
// role's specs can run this without a Pi or systemd.
package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const (
	defaultProc    = "/proc"
	defaultThermal = "/sys/class/thermal/thermal_zone0/temp"
	defaultDisk    = "/"
	defaultSystemd = "systemctl"
	// commandTimeout bounds all the systemctl queries together, so a wedged systemd cannot hold up a login.
	commandTimeout = 5 * time.Second
	// maxReadBytes bounds one /proc or /sys read; the files hold a few hundred bytes.
	maxReadBytes = 1 << 20
)

// reading is a value that may have failed to read; the banner shows the failure in its place.
type reading[T any] struct {
	value T
	err   error
}

type loadavg struct{ one, five, fifteen string }

type memory struct{ usedKiB, totalKiB uint64 }

type disk struct{ usedBytes, availBytes, totalBytes uint64 }

type service struct {
	state    string
	since    string
	sinceErr error
}

// snapshot is everything the banner shows.
type snapshot struct {
	host   reading[string]
	ip     reading[string]
	model  string
	uptime reading[time.Duration]
	load   reading[loadavg]
	cores  int
	mem    reading[memory]
	disk   reading[disk]
	// temp is in millidegrees Celsius; fs.ErrNotExist means the host has no thermal zone, so no line.
	temp   reading[int64]
	dagu   reading[service]
	failed reading[int]
}

type config struct {
	proc, thermal, systemctl, disk string
}

func main() {
	cfg := config{
		proc:      orDefault(os.Getenv("FACTORY_STATUS_PROC"), defaultProc),
		thermal:   orDefault(os.Getenv("FACTORY_STATUS_THERMAL_ZONE"), defaultThermal),
		systemctl: orDefault(os.Getenv("FACTORY_STATUS_SYSTEMCTL"), defaultSystemd),
		disk:      orDefault(os.Getenv("FACTORY_STATUS_DISK"), defaultDisk),
	}
	ctx, cancel := context.WithTimeout(context.Background(), commandTimeout)
	defer cancel()
	if _, err := io.WriteString(os.Stdout, banner(collect(ctx, cfg))); err != nil {
		// There is nothing to fall back to, and a login must not fail over its banner.
		fmt.Fprintln(os.Stderr, "factory-status:", err)
	}
}

func orDefault(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}

// collect reads the host. No read failure stops the others.
func collect(ctx context.Context, cfg config) snapshot {
	var s snapshot
	host, err := os.Hostname()
	s.host = reading[string]{host, err}
	ip, err := firstAddress()
	s.ip = reading[string]{ip, err}
	s.model = readModel(cfg.proc)
	secs, err := readUptime(filepath.Join(cfg.proc, "uptime"))
	s.uptime = reading[time.Duration]{secs, err}
	load, err := readLoadavg(filepath.Join(cfg.proc, "loadavg"))
	s.load = reading[loadavg]{load, err}
	s.cores = runtime.NumCPU()
	mem, err := readMemory(filepath.Join(cfg.proc, "meminfo"))
	s.mem = reading[memory]{mem, err}
	d, err := readDisk(cfg.disk)
	s.disk = reading[disk]{d, err}
	temp, err := readMillidegrees(cfg.thermal)
	s.temp = reading[int64]{temp, err}
	dagu, err := readDagu(ctx, cfg.systemctl)
	s.dagu = reading[service]{dagu, err}
	failed, err := readFailedUnits(ctx, cfg.systemctl)
	s.failed = reading[int]{failed, err}
	return s
}

// firstAddress is the first address on a non-loopback interface that is not link-local, or "".
func firstAddress() (string, error) {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return "", fmt.Errorf("cannot list addresses: %w", err)
	}
	for _, a := range addrs {
		n, ok := a.(*net.IPNet)
		if !ok || n.IP.IsLoopback() || n.IP.IsLinkLocalUnicast() {
			continue
		}
		return n.IP.String(), nil
	}
	return "", nil
}

// readModel is the board's model, or the CPU architecture where the board has none (a VM).
func readModel(proc string) string {
	raw, err := readSmall(filepath.Join(proc, "device-tree", "model"))
	model := strings.TrimSpace(strings.ReplaceAll(raw, "\x00", ""))
	if err != nil || model == "" {
		// The model only decorates the header, and a host without a device tree is normal.
		return unameMachine()
	}
	return model
}

// unameMachine is `uname -m`'s name for the architecture this binary was built for.
func unameMachine() string {
	switch runtime.GOARCH {
	case "arm64":
		return "aarch64"
	case "amd64":
		return "x86_64"
	default:
		return runtime.GOARCH
	}
}

// readSmall reads a small file to the end, within the bound.
func readSmall(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close() // read-only; a close failure loses nothing

	// One Read may return part of a file; ReadAll reads to the end, within the bound.
	raw, err := io.ReadAll(io.LimitReader(f, maxReadBytes))
	if err != nil {
		return "", fmt.Errorf("cannot read %s: %w", path, err)
	}
	return string(raw), nil
}

func readUptime(path string) (time.Duration, error) {
	raw, err := readSmall(path)
	if err != nil {
		return 0, err
	}
	fields := strings.Fields(raw)
	if len(fields) == 0 {
		return 0, fmt.Errorf("%s is empty", path)
	}
	secs, err := strconv.ParseFloat(fields[0], 64)
	if err != nil || secs < 0 {
		return 0, fmt.Errorf("%s does not start with seconds: %q", path, fields[0])
	}
	return time.Duration(secs * float64(time.Second)), nil
}

func readLoadavg(path string) (loadavg, error) {
	raw, err := readSmall(path)
	if err != nil {
		return loadavg{}, err
	}
	fields := strings.Fields(raw)
	if len(fields) < 3 {
		return loadavg{}, fmt.Errorf("%s holds fewer than three load averages: %q", path, strings.TrimSpace(raw))
	}
	return loadavg{fields[0], fields[1], fields[2]}, nil
}

func readMemory(path string) (memory, error) {
	f, err := os.Open(path)
	if err != nil {
		return memory{}, err
	}
	defer f.Close() // read-only; a close failure loses nothing

	var total, avail uint64
	var haveTotal, haveAvail bool
	sc := bufio.NewScanner(io.LimitReader(f, maxReadBytes))
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 2 {
			continue
		}
		var dst *uint64
		switch fields[0] {
		case "MemTotal:":
			dst, haveTotal = &total, true
		case "MemAvailable:":
			dst, haveAvail = &avail, true
		default:
			continue
		}
		v, err := strconv.ParseUint(fields[1], 10, 64)
		if err != nil {
			return memory{}, fmt.Errorf("%s: %s is not a number: %q", path, fields[0], fields[1])
		}
		*dst = v
	}
	if err := sc.Err(); err != nil {
		return memory{}, fmt.Errorf("cannot read %s: %w", path, err)
	}
	if !haveTotal || !haveAvail || total == 0 || avail > total {
		return memory{}, fmt.Errorf("%s lacks a usable MemTotal and MemAvailable", path)
	}
	return memory{usedKiB: total - avail, totalKiB: total}, nil
}

// readDisk is the filesystem holding path, as `df -P` counts it: used blocks against used plus
// the blocks available to unprivileged users.
func readDisk(path string) (disk, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return disk{}, fmt.Errorf("cannot stat the filesystem at %s: %w", path, err)
	}
	// The field types differ by platform, so each is converted.
	block := uint64(st.Bsize)
	blocks, free, avail := uint64(st.Blocks), uint64(st.Bfree), uint64(st.Bavail)
	if blocks == 0 || free > blocks {
		return disk{}, fmt.Errorf("the filesystem at %s reports no usable size", path)
	}
	return disk{
		usedBytes:  (blocks - free) * block,
		availBytes: avail * block,
		totalBytes: blocks * block,
	}, nil
}

func readMillidegrees(path string) (int64, error) {
	raw, err := readSmall(path)
	if err != nil {
		return 0, err
	}
	text := strings.TrimSpace(raw)
	v, err := strconv.ParseInt(text, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%s does not hold millidegrees Celsius: %q", path, text)
	}
	return v, nil
}

// output runs a command and returns its standard output.
func output(ctx context.Context, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.WaitDelay = time.Second
	var out bytes.Buffer
	cmd.Stdout = &out
	err := cmd.Run()
	return out.String(), err
}

func readDagu(ctx context.Context, systemctl string) (service, error) {
	out, err := output(ctx, systemctl, "is-active", "dagu")
	state := strings.TrimSpace(out)
	var exit *exec.ExitError
	// is-active exits non-zero for every state but active while still printing the state.
	if err != nil && !(errors.As(err, &exit) && state != "") {
		return service{}, fmt.Errorf("%s is-active dagu failed: %w", systemctl, err)
	}
	if state != "active" {
		return service{state: state}, nil
	}
	since, err := output(ctx, systemctl, "show", "dagu", "-p", "ActiveEnterTimestamp", "--value")
	if err != nil {
		return service{state: state, sinceErr: fmt.Errorf("%s show dagu failed: %w", systemctl, err)}, nil
	}
	return service{state: state, since: strings.TrimSpace(since)}, nil
}

func readFailedUnits(ctx context.Context, systemctl string) (int, error) {
	out, err := output(ctx, systemctl, "--failed", "--no-legend")
	if err != nil {
		return 0, fmt.Errorf("%s --failed failed: %w", systemctl, err)
	}
	n := 0
	for _, line := range strings.Split(out, "\n") {
		if strings.TrimSpace(line) != "" {
			n++
		}
	}
	return n, nil
}
