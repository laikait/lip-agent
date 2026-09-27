package collect

import (
	"math"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Sample is one moment of a machine, in the protocol's shape
// (plans/agent.md, protocol v1).
type Sample struct {
	SampledAt   string  `json:"sampledAt"`
	CPU         float64 `json:"cpu"`
	MemoryUsed  uint64  `json:"memoryUsed"`
	MemoryTotal uint64  `json:"memoryTotal"`
	Load1       float64 `json:"load1"`
	Load5       float64 `json:"load5"`
	Load15      float64 `json:"load15"`
	DiskUsed    uint64  `json:"diskUsed"`
	DiskTotal   uint64  `json:"diskTotal"`
	NetIn       uint64  `json:"netIn"`
	NetOut      uint64  `json:"netOut"`
}

// Host is what the machine is.
type Host struct {
	Hostname      string `json:"hostname"`
	OS            string `json:"os,omitempty"`
	Kernel        string `json:"kernel,omitempty"`
	UptimeSeconds int64  `json:"uptimeSeconds,omitempty"`
}

// Sampler takes samples. CPU and network are rates, so each sample is
// measured against the one before it; the first call only takes the
// baseline.
//
// Root is where the host's filesystem is: "/" normally, or wherever it is
// mounted when the agent runs in a container (/host, with /host/proc).
type Sampler struct {
	Root     string
	DiskPath string

	primed  bool
	cpu     CPUTimes
	net     NetCounters
	takenAt time.Time
}

// NewSampler reads the host at root, and the disk holding diskPath ("/" by
// default, under root).
func NewSampler(root, diskPath string) *Sampler {
	if root == "" {
		root = "/"
	}

	if diskPath == "" {
		diskPath = "/"
	}

	return &Sampler{Root: root, DiskPath: diskPath}
}

// Sample reads the machine now. False until a baseline has been taken.
func (s *Sampler) Sample(now time.Time) (Sample, bool, error) {
	cpu, err := read(s, "proc/stat", ParseStat)
	if err != nil {
		return Sample{}, false, err
	}

	net, err := read(s, "proc/net/dev", ParseNetDev)
	if err != nil {
		return Sample{}, false, err
	}

	if !s.primed || !now.After(s.takenAt) {
		s.primed, s.cpu, s.net, s.takenAt = true, cpu, net, now

		return Sample{}, false, nil
	}

	memory, err := read(s, "proc/meminfo", ParseMeminfo)
	if err != nil {
		return Sample{}, false, err
	}

	load, err := read(s, "proc/loadavg", ParseLoadavg)
	if err != nil {
		return Sample{}, false, err
	}

	diskUsed, diskTotal, err := Disk(filepath.Join(s.Root, s.DiskPath))
	if err != nil {
		return Sample{}, false, err
	}

	seconds := now.Sub(s.takenAt).Seconds()
	sample := Sample{
		SampledAt:   now.UTC().Format(time.RFC3339),
		CPU:         round(CPUPercent(s.cpu, cpu)),
		MemoryUsed:  memory.Used,
		MemoryTotal: memory.Total,
		Load1:       round(load.One),
		Load5:       round(load.Five),
		Load15:      round(load.Fifteen),
		DiskUsed:    diskUsed,
		DiskTotal:   diskTotal,
		NetIn:       Rate(s.net.Received, net.Received, seconds),
		NetOut:      Rate(s.net.Sent, net.Sent, seconds),
	}

	s.cpu, s.net, s.takenAt = cpu, net, now

	return sample, true, nil
}

// Host reads what the machine is now. The hostname is the kernel's, under
// Root, so an agent in a container reports the host's and not its own.
func (s *Sampler) Host() Host {
	host := Host{}

	if name, err := os.ReadFile(filepath.Join(s.Root, "proc/sys/kernel/hostname")); err == nil {
		host.Hostname = strings.TrimSpace(string(name))
	}

	if host.Hostname == "" {
		host.Hostname, _ = os.Hostname()
	}

	for _, file := range []string{"etc/os-release", "usr/lib/os-release"} {
		if data, err := os.ReadFile(filepath.Join(s.Root, file)); err == nil {
			host.OS = ParseOSRelease(data)

			break
		}
	}

	if kernel, err := os.ReadFile(filepath.Join(s.Root, "proc/sys/kernel/osrelease")); err == nil {
		host.Kernel = strings.TrimSpace(string(kernel))
	}

	if uptime, err := s.file("proc/uptime"); err == nil {
		host.UptimeSeconds, _ = ParseUptime(uptime)
	}

	return host
}

// file is a file under Root.
func (s *Sampler) file(name string) ([]byte, error) {
	return os.ReadFile(filepath.Join(s.Root, name))
}

// read is a file under Root, parsed.
func read[T any](s *Sampler, name string, parse func([]byte) (T, error)) (T, error) {
	data, err := s.file(name)
	if err != nil {
		var zero T

		return zero, err
	}

	return parse(data)
}

func round(value float64) float64 {
	return math.Round(value*100) / 100
}
