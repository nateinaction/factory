package main

import (
	"errors"
	"fmt"
	"io/fs"
	"math"
	"strings"
	"time"
)

const (
	bold = "\033[1m"
	dim  = "\033[2m"
	grn  = "\033[32m"
	red  = "\033[31m"
	ylw  = "\033[33m"
	rst  = "\033[0m"

	barCells = 20
	// warnPct and critPct colour a bar yellow and red.
	warnPct = 70
	critPct = 90
)

// banner is the text printed at login for a snapshot.
func banner(s snapshot) string {
	var b strings.Builder

	fmt.Fprintf(&b, "\n%s%s%s  %s%s%s\n", bold, orElse(s.host, "unknown host"), rst, dim, s.model, rst)
	fmt.Fprintf(&b, "%s%s  up %s%s\n\n", dim, address(s.ip), uptime(s.uptime), rst)

	line(&b, "load", s.load, func(l loadavg) string {
		return fmt.Sprintf("%s %s %s  (%d cores)", l.one, l.five, l.fifteen, s.cores)
	})
	line(&b, "memory", s.mem, func(m memory) string {
		return fmt.Sprintf("%s  %d MiB / %d MiB", bar(percent(m.usedKiB, m.totalKiB)), m.usedKiB/1024, m.totalKiB/1024)
	})
	line(&b, "disk /", s.disk, func(d disk) string {
		// df -P counts the percentage against used plus available, not the whole size.
		return fmt.Sprintf("%s  %s / %s", bar(percentUp(d.usedBytes, d.usedBytes+d.availBytes)),
			humanSize(d.usedBytes), humanSize(d.totalBytes))
	})
	if !errors.Is(s.temp.err, fs.ErrNotExist) {
		line(&b, "temp", s.temp, func(m int64) string { return fmt.Sprintf("%d C", m/1000) })
	}
	daguLine(&b, s.dagu)
	if s.failed.err != nil || s.failed.value > 0 {
		line(&b, "systemd", s.failed, func(n int) string { return fmt.Sprintf("%s%d failed unit(s)%s", red, n, rst) })
	}
	b.WriteString("\n")
	return b.String()
}

// line writes one labelled row: the formatted value, or why there is none.
func line[T any](b *strings.Builder, label string, r reading[T], format func(T) string) {
	if r.err != nil {
		fmt.Fprintf(b, "  %-7s %s\n", label, unavailable(r.err))
		return
	}
	fmt.Fprintf(b, "  %-7s %s\n", label, format(r.value))
}

func unavailable(err error) string {
	return fmt.Sprintf("%sunavailable%s %s(%v)%s", red, rst, dim, err, rst)
}

func daguLine(b *strings.Builder, r reading[service]) {
	if r.err != nil {
		fmt.Fprintf(b, "  %-7s %s\n", "dagu", unavailable(r.err))
		return
	}
	colour, state := red, r.value.state
	if state == "active" {
		colour = grn
	}
	if state == "" {
		state = "unknown"
	}
	fmt.Fprintf(b, "  %-7s %s%s%s", "dagu", colour, state, rst)
	switch {
	case r.value.sinceErr != nil:
		fmt.Fprintf(b, "  %ssince unknown (%v)%s", dim, r.value.sinceErr, rst)
	case r.value.since != "":
		fmt.Fprintf(b, "  %ssince %s%s", dim, r.value.since, rst)
	}
	b.WriteString("\n")
}

func orElse(r reading[string], fallback string) string {
	if r.err != nil || r.value == "" {
		return fallback
	}
	return r.value
}

func address(r reading[string]) string {
	switch {
	case r.err != nil:
		return "ip unavailable"
	case r.value == "":
		return "no-ip"
	default:
		return r.value
	}
}

// bar is a barCells-wide bar for a percentage, coloured by threshold, with the number after it.
func bar(pct int) string {
	pct = min(max(pct, 0), 100)
	colour := grn
	switch {
	case pct >= critPct:
		colour = red
	case pct >= warnPct:
		colour = ylw
	}
	filled := pct * barCells / 100
	return fmt.Sprintf("%s%s%s%s %3d%%", colour, strings.Repeat("#", filled), strings.Repeat(".", barCells-filled), rst, pct)
}

// percent is part over whole, rounded down.
func percent(part, whole uint64) int {
	if whole == 0 {
		return 0
	}
	return int(part * 100 / whole)
}

// percentUp is part over whole, rounded up, as df prints it.
func percentUp(part, whole uint64) int {
	if whole == 0 {
		return 0
	}
	return int((part*100 + whole - 1) / whole)
}

// uptime is the duration as `uptime -p` words it: weeks, days, hours and minutes, without the zero ones.
func uptime(r reading[time.Duration]) string {
	if r.err != nil {
		return "?"
	}
	mins := int(r.value / time.Minute)
	units := []struct {
		name string
		mins int
	}{{"week", 7 * 24 * 60}, {"day", 24 * 60}, {"hour", 60}, {"minute", 1}}
	var parts []string
	for _, u := range units {
		n := mins / u.mins
		mins %= u.mins
		switch {
		case n == 1:
			parts = append(parts, "1 "+u.name)
		case n > 1:
			parts = append(parts, fmt.Sprintf("%d %ss", n, u.name))
		}
	}
	if len(parts) == 0 {
		return "0 minutes"
	}
	return strings.Join(parts, ", ")
}

// humanSize is a byte count in powers of 1024, rounded up as `df -h` does: one decimal below 10, whole above.
func humanSize(n uint64) string {
	suffixes := []string{"", "K", "M", "G", "T", "P"}
	v := float64(n)
	i := 0
	for v > 1023 && i < len(suffixes)-1 {
		v /= 1024
		i++
	}
	if i == 0 {
		return fmt.Sprintf("%d", n)
	}
	if v < 10 {
		return fmt.Sprintf("%.1f%s", math.Ceil(v*10)/10, suffixes[i])
	}
	return fmt.Sprintf("%.0f%s", math.Ceil(v), suffixes[i])
}
