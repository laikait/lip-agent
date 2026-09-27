package collect

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func TestStatGivesBusyShareOfAllCores(t *testing.T) {
	before, err := ParseStat([]byte("cpu  100 0 100 700 100 0 0 0 0 0\ncpu0 1 2 3 4\n"))
	if err != nil {
		t.Fatal(err)
	}

	if before.Total != 1000 || before.Idle != 800 {
		t.Fatalf("idle is idle + iowait: %+v", before)
	}

	after, _ := ParseStat([]byte("cpu  200 0 200 800 100 0 0 0 50 50\n"))

	// 200 busy of 300 more, guest columns not counted twice.
	if got := CPUPercent(before, after); got < 66.66 || got > 66.67 {
		t.Fatalf("CPU %v, want 66.67", got)
	}

	if CPUPercent(after, before) != 0 {
		t.Fatal("a counter going backwards reads as idle")
	}

	if _, err := ParseStat([]byte("intr 1 2 3\n")); err == nil {
		t.Fatal("no cpu line is an error")
	}
}

func TestMemoryCountsCacheAsAvailable(t *testing.T) {
	memory, err := ParseMeminfo([]byte("MemTotal: 1000 kB\nMemFree: 100 kB\nMemAvailable: 600 kB\n"))
	if err != nil || memory.Total != 1024000 || memory.Used != 409600 {
		t.Fatalf("%+v %v", memory, err)
	}

	// An old kernel with no MemAvailable.
	memory, _ = ParseMeminfo([]byte("MemTotal: 1000 kB\nMemFree: 100 kB\nBuffers: 100 kB\nCached: 300 kB\n"))
	if memory.Used != 500*1024 {
		t.Fatalf("free + buffers + cache stand in: %+v", memory)
	}

	if _, err := ParseMeminfo([]byte("MemFree: 1 kB\n")); err == nil {
		t.Fatal("no total is an error")
	}
}

func TestNetworkLeavesOutLoopback(t *testing.T) {
	data, _ := os.ReadFile("../../testdata/host/proc/net/dev")

	counters, err := ParseNetDev(data)
	if err != nil || counters.Received != 1000000 || counters.Sent != 500000 {
		t.Fatalf("%+v %v", counters, err)
	}

	if Rate(100, 700, 60) != 10 || Rate(700, 100, 60) != 0 {
		t.Fatal("rate, and a counter that went backwards")
	}
}

func TestOSReleasePrefersThePrettyName(t *testing.T) {
	if got := ParseOSRelease([]byte("NAME=\"Alpine Linux\"\nPRETTY_NAME=\"Alpine Linux v3.20\"\n")); got != "Alpine Linux v3.20" {
		t.Fatal(got)
	}

	if got := ParseOSRelease([]byte("NAME='Some Linux'\nVERSION=7\n")); got != "Some Linux 7" {
		t.Fatal(got)
	}
}

func TestServicesReportWhatIsDoingSomethingOrFailed(t *testing.T) {
	output := []byte(`nginx.service          loaded    active   running Web server
cron.service           loaded    active   running Regular background program processing daemon
● backup.service       loaded    failed   failed  Nightly backup
systemd-tmpfiles.service loaded  active   exited  Create volatile files
ssh.service            loaded    inactive dead    OpenBSD Secure Shell server
worker.service         loaded    activating auto-restart Queue worker
ghost.service          not-found inactive dead    ghost.service
`)

	got := ParseSystemctl(output, nil)
	want := []Service{{"backup", "failed"}, {"worker", "unknown"}, {"cron", "running"}, {"nginx", "running"}}

	if len(got) != len(want) {
		t.Fatalf("got %v", got)
	}

	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v: worst first, one-shots and dead ones left out", got, want)
		}
	}

	named := ParseSystemctl(output, []string{"ssh", "nginx.service", "ghost", "missing", "ssh"})
	if len(named) != 4 || named[0] != (Service{"ssh", "stopped"}) || named[3] != (Service{"nginx", "running"}) {
		t.Fatalf("named services, whatever their state: %v", named)
	}
}

func TestTheSamplerMeasuresAgainstItsBaseline(t *testing.T) {
	root := t.TempDir()
	copyTree(t, "../../testdata/host", root)

	sampler := NewSampler(root, "/")
	start := time.Date(2026, 9, 27, 10, 0, 0, 0, time.UTC)

	if _, ok, err := sampler.Sample(start); ok || err != nil {
		t.Fatalf("the first call is the baseline: %v %v", ok, err)
	}

	write(t, filepath.Join(root, "proc/stat"), "cpu  4765 356 604 3699196 23060 0 277 0 0 0\n")
	write(t, filepath.Join(root, "proc/net/dev"), "Inter-|\n face |\n  eth0: 1600000 0 0 0 0 0 0 0 800000 0 0 0 0 0 0 0\n")

	sample, ok, err := sampler.Sample(start.Add(time.Minute))
	if err != nil || !ok {
		t.Fatal(ok, err)
	}

	if sample.SampledAt != "2026-09-27T10:01:00Z" || sample.CPU != 80 || sample.NetIn != 10000 || sample.NetOut != 5000 {
		t.Fatalf("%+v", sample)
	}

	if sample.MemoryTotal != 8000000*1024 || sample.Load1 != 0.42 {
		t.Fatalf("%+v", sample)
	}

	host := sampler.Host()
	if host.Hostname != "web-1" || host.OS != "Debian GNU/Linux 12 (bookworm)" || host.Kernel != "6.1.0-26-amd64" || host.UptimeSeconds != 93600 {
		t.Fatalf("%+v", host)
	}

	if runtime.GOOS != "linux" && (sample.DiskTotal != 0 || sample.DiskUsed != 0) {
		t.Fatal("no disk off Linux")
	}
}

func copyTree(t *testing.T, from, to string) {
	t.Helper()

	err := filepath.WalkDir(from, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}

		relative, _ := filepath.Rel(from, path)
		target := filepath.Join(to, relative)

		if entry.IsDir() {
			return os.MkdirAll(target, 0o755)
		}

		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}

		return os.WriteFile(target, data, 0o644)
	})
	if err != nil {
		t.Fatal(err)
	}
}

func write(t *testing.T, path, content string) {
	t.Helper()

	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
