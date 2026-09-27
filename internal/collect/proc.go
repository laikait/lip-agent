// Package collect reads what a Linux machine says about itself: /proc, the
// root filesystem and systemd. Every parser takes the file's bytes, so it is
// tested with fixtures on any operating system; only reading the files and
// statfs are Linux's.
package collect

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// CPUTimes is the first line of /proc/stat, reduced to what a percentage
// needs: time spent idle (idle + iowait) and time spent at all.
type CPUTimes struct {
	Idle  uint64
	Total uint64
}

// ParseStat reads the aggregate "cpu" line of /proc/stat. Guest time is
// already counted in user time by the kernel, so only the first eight
// columns are summed.
func ParseStat(data []byte) (CPUTimes, error) {
	line, _, _ := bytes.Cut(data, []byte("\n"))
	fields := strings.Fields(string(line))

	if len(fields) < 5 || fields[0] != "cpu" {
		return CPUTimes{}, errors.New("/proc/stat: no aggregate cpu line")
	}

	var times CPUTimes

	for i, field := range fields[1:] {
		if i >= 8 {
			break
		}

		value, err := strconv.ParseUint(field, 10, 64)
		if err != nil {
			return CPUTimes{}, fmt.Errorf("/proc/stat: %w", err)
		}

		times.Total += value

		// idle (3) and iowait (4), counting from user (0).
		if i == 3 || i == 4 {
			times.Idle += value
		}
	}

	return times, nil
}

// CPUPercent is the share of all cores busy between two readings, 0–100.
// A counter that went backwards (a restarted container) reads as idle.
func CPUPercent(before, after CPUTimes) float64 {
	if after.Total <= before.Total || after.Idle < before.Idle {
		return 0
	}

	total := float64(after.Total - before.Total)
	idle := float64(after.Idle - before.Idle)

	return clamp((total-idle)/total*100, 0, 100)
}

// Memory is used and total bytes. Used is total less what the kernel says
// is available, which counts reclaimable cache as free: a machine with a
// full page cache is not out of memory.
type Memory struct {
	Used  uint64
	Total uint64
}

// ParseMeminfo reads /proc/meminfo. Kernels older than 3.14 have no
// MemAvailable; free, buffers and cache stand in for it.
func ParseMeminfo(data []byte) (Memory, error) {
	values := map[string]uint64{}
	scanner := bufio.NewScanner(bytes.NewReader(data))

	for scanner.Scan() {
		name, rest, found := strings.Cut(scanner.Text(), ":")
		if !found {
			continue
		}

		fields := strings.Fields(rest)
		if len(fields) == 0 {
			continue
		}

		value, err := strconv.ParseUint(fields[0], 10, 64)
		if err != nil {
			continue
		}

		values[name] = value * 1024
	}

	total, ok := values["MemTotal"]
	if !ok || total == 0 {
		return Memory{}, errors.New("/proc/meminfo: no MemTotal")
	}

	available, ok := values["MemAvailable"]
	if !ok {
		available = values["MemFree"] + values["Buffers"] + values["Cached"]
	}

	if available > total {
		available = total
	}

	return Memory{Used: total - available, Total: total}, nil
}

// Load is the kernel's load averages over 1, 5 and 15 minutes.
type Load struct {
	One, Five, Fifteen float64
}

// ParseLoadavg reads /proc/loadavg.
func ParseLoadavg(data []byte) (Load, error) {
	fields := strings.Fields(string(data))
	if len(fields) < 3 {
		return Load{}, errors.New("/proc/loadavg: too short")
	}

	var numbers [3]float64

	for i := range numbers {
		value, err := strconv.ParseFloat(fields[i], 64)
		if err != nil {
			return Load{}, fmt.Errorf("/proc/loadavg: %w", err)
		}

		numbers[i] = value
	}

	return Load{One: numbers[0], Five: numbers[1], Fifteen: numbers[2]}, nil
}

// NetCounters is bytes received and sent since boot, over every interface
// except loopback: what the machine exchanged with anything else.
type NetCounters struct {
	Received uint64
	Sent     uint64
}

// ParseNetDev reads /proc/net/dev.
func ParseNetDev(data []byte) (NetCounters, error) {
	var counters NetCounters

	scanner := bufio.NewScanner(bytes.NewReader(data))
	seen := false

	for scanner.Scan() {
		name, rest, found := strings.Cut(scanner.Text(), ":")
		if !found {
			continue
		}

		name = strings.TrimSpace(name)
		fields := strings.Fields(rest)

		if name == "lo" || len(fields) < 9 {
			continue
		}

		received, err1 := strconv.ParseUint(fields[0], 10, 64)
		sent, err2 := strconv.ParseUint(fields[8], 10, 64)

		if err1 != nil || err2 != nil {
			continue
		}

		counters.Received += received
		counters.Sent += sent
		seen = true
	}

	if !seen && !bytes.Contains(data, []byte("|")) {
		return NetCounters{}, errors.New("/proc/net/dev: not recognised")
	}

	return counters, nil
}

// Rate is bytes a second between two counter readings. A counter that went
// backwards (an interface removed, a wrap) gives 0 rather than nonsense.
func Rate(before, after uint64, seconds float64) uint64 {
	if after < before || seconds <= 0 {
		return 0
	}

	return uint64(float64(after-before)/seconds + 0.5)
}

// ParseUptime reads /proc/uptime: seconds since boot.
func ParseUptime(data []byte) (int64, error) {
	fields := strings.Fields(string(data))
	if len(fields) == 0 {
		return 0, errors.New("/proc/uptime: empty")
	}

	value, err := strconv.ParseFloat(fields[0], 64)
	if err != nil {
		return 0, fmt.Errorf("/proc/uptime: %w", err)
	}

	return int64(value), nil
}

// ParseOSRelease is os-release's PRETTY_NAME ("Debian GNU/Linux 12
// (bookworm)"), or NAME and VERSION when it has none.
func ParseOSRelease(data []byte) string {
	values := map[string]string{}
	scanner := bufio.NewScanner(bytes.NewReader(data))

	for scanner.Scan() {
		key, value, found := strings.Cut(strings.TrimSpace(scanner.Text()), "=")
		if !found || strings.HasPrefix(key, "#") {
			continue
		}

		if unquoted, err := strconv.Unquote(value); err == nil {
			value = unquoted
		} else {
			value = strings.Trim(value, `'"`)
		}

		values[key] = value
	}

	if values["PRETTY_NAME"] != "" {
		return values["PRETTY_NAME"]
	}

	return strings.TrimSpace(values["NAME"] + " " + values["VERSION"])
}

func clamp(value, low, high float64) float64 {
	switch {
	case value < low:
		return low
	case value > high:
		return high
	default:
		return value
	}
}
