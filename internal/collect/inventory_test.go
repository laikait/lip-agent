package collect

import (
	"context"
	"strings"
	"testing"
)

func TestDpkgListsOnlyWhatIsInstalled(t *testing.T) {
	packages := ParseDpkg([]byte("ii \tnginx\t1.22.1-9\tamd64\nrc \told-thing\t1.0\tamd64\nii \tlibc6\t2.36-9\ti386\nii \tlibc6\t2.36-9\tamd64\n"))

	want := []Package{
		{Name: "libc6", Version: "2.36-9", Architecture: "amd64", Manager: "dpkg"},
		{Name: "libc6", Version: "2.36-9", Architecture: "i386", Manager: "dpkg"},
		{Name: "nginx", Version: "1.22.1-9", Architecture: "amd64", Manager: "dpkg"},
	}

	if len(packages) != len(want) {
		t.Fatalf("removed packages with config left are not installed: %+v", packages)
	}

	for i := range want {
		if packages[i] != want[i] {
			t.Fatalf("%d: %+v, want %+v (sorted by name, then architecture)", i, packages[i], want[i])
		}
	}
}

func TestRpmAndApkAreReadToo(t *testing.T) {
	rpm := ParseRpm([]byte("openssl\t3.0.7-27.el9\tx86_64\ngpg-pubkey\t8483c65d-5ccc5b19\t(none)\n"))
	if len(rpm) != 2 || rpm[0].Name != "gpg-pubkey" || rpm[0].Architecture != "" || rpm[1].Version != "3.0.7-27.el9" {
		t.Fatalf("%+v", rpm)
	}

	apk := ParseApk([]byte("musl-1.2.4-r2\nca-certificates-bundle-20230506-r0\nnot a package\n"))
	if len(apk) != 2 || apk[0].Name != "ca-certificates-bundle" || apk[0].Version != "20230506-r0" || apk[1].Name != "musl" || apk[1].Manager != "apk" {
		t.Fatalf("a name may hold dashes; the version is the last two parts: %+v", apk)
	}
}

func TestACronLineSendsItsProgramAndNeverItsArguments(t *testing.T) {
	line := "30 2 * * * root PGPASSWORD=hunter2 /usr/local/bin/db-backup --all --to /var/backups"
	jobs := ParseCrontab([]byte("# comment\nSHELL=/bin/sh\n\n"+line+"\n@reboot root /usr/local/bin/warm-cache\n"), "/etc/crontab", true)

	if len(jobs) != 2 {
		t.Fatalf("comments and variables are not jobs: %+v", jobs)
	}

	backup := jobs[0]
	if backup.Schedule != "30 2 * * *" || backup.User != "root" || backup.Program != "/usr/local/bin/db-backup" || backup.File != "/etc/crontab" {
		t.Fatalf("%+v", backup)
	}

	if backup.LineHash != hash(line) || len(backup.LineHash) != 64 {
		t.Fatalf("the line is a hash: %q", backup.LineHash)
	}

	for _, job := range jobs {
		for _, value := range []string{job.Schedule, job.User, job.Program, job.File} {
			if strings.Contains(value, "hunter2") || strings.Contains(value, "--all") {
				t.Fatalf("an argument left the machine: %+v", job)
			}
		}
	}

	if jobs[1].Schedule != "@reboot" || jobs[1].Program != "/usr/local/bin/warm-cache" {
		t.Fatalf("%+v", jobs[1])
	}

	// A user's own crontab has no user column.
	own := ParseCrontab([]byte("*/5 * * * * /home/ada/bin/sync\n"), "ada", false)
	if len(own) != 1 || own[0].User != "" || own[0].Program != "/home/ada/bin/sync" {
		t.Fatalf("%+v", own)
	}
}

func TestADebianTestedProgramIsTheProgram(t *testing.T) {
	jobs := ParseCrontab([]byte("09,39 * * * * root [ -x /usr/lib/php/sessionclean ] && /usr/lib/php/sessionclean\n"), "/etc/cron.d/php", true)

	if len(jobs) != 1 || jobs[0].Program != "/usr/lib/php/sessionclean" {
		t.Fatalf("%+v", jobs)
	}

	// Debian's e2scrub_all: -e tests a path that is not the program, so it
	// stays on the machine and the program is the test itself.
	jobs = ParseCrontab([]byte("30 3 * * 0 root test -e /run/systemd/system || SERVICE_MODE=1 /usr/lib/x86_64-linux-gnu/e2fsprogs/e2scrub_all_cron\n"), "/etc/cron.d/e2scrub_all", true)
	if len(jobs) != 1 || jobs[0].Program != "test" {
		t.Fatalf("an -e argument left the machine: %+v", jobs)
	}
}

func TestCronIsReadFromTheSystemFilesUnderTheRoot(t *testing.T) {
	jobs, err := CronJobs("../../testdata/host")
	if err != nil {
		t.Fatal(err)
	}

	var got []string
	for _, job := range jobs {
		got = append(got, job.File+" "+job.Schedule+" "+job.Program)
	}

	want := []string{
		"/etc/crontab 17 * * * * cd",
		"/etc/crontab 30 2 * * * /usr/local/bin/db-backup",
		"/etc/crontab @reboot /usr/local/bin/warm-cache",
		"/etc/cron.d/php 09,39 * * * * /usr/lib/php/sessionclean",
		"/etc/cron.daily @daily /etc/cron.daily/logrotate",
	}

	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("got\n%s\nwant\n%s\n(a file.dpkg-old is not run, so not listed)", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}

	if _, err := CronJobs("../../testdata/nowhere"); err == nil {
		t.Fatal("no /etc at all is an error, not an empty list")
	}
}

func TestTimersAreReadFromSystemctlShow(t *testing.T) {
	units := ParseTimerUnits([]byte("logrotate.timer loaded active waiting Daily rotation of log files\n● fstrim.timer loaded failed failed Discard unused blocks\n"))
	if strings.Join(units, ",") != "logrotate.timer,fstrim.timer" {
		t.Fatalf("%v", units)
	}

	timers := ParseTimers([]byte("Id=logrotate.timer\nUnit=logrotate.service\nTimersMonotonic=\nTimersCalendar={ OnCalendar=*-*-* 00:00:00 ; next_elapse=Mon 2026-09-28 00:00:00 UTC }\n\n" +
		"Id=apt-daily.timer\nUnit=apt-daily.service\nTimersMonotonic={ OnUnitActiveSec=12h ; next_elapse=n/a }\nTimersCalendar={ OnCalendar=*-*-* 06,18:00:00 ; next_elapse=n/a }\n"))

	want := []Timer{
		{Name: "logrotate", Schedule: "*-*-* 00:00:00", Unit: "logrotate.service"},
		{Name: "apt-daily", Schedule: "OnUnitActiveSec=12h, *-*-* 06,18:00:00", Unit: "apt-daily.service"},
	}

	if len(timers) != 2 || timers[0] != want[0] || timers[1] != want[1] {
		t.Fatalf("%+v", timers)
	}
}

func TestHardwareCountsTheProcessors(t *testing.T) {
	if n := ParseCPUInfo([]byte("processor\t: 0\nmodel name\t: X\n\nprocessor\t: 1\n")); n != 2 {
		t.Fatalf("%d", n)
	}

	hardware := NewSampler("../../testdata/host", "/").Hardware(context.Background())
	if hardware.CPUCores != 4 || hardware.MemoryBytes == 0 || hardware.Architecture == "" {
		t.Fatalf("%+v", hardware)
	}

	if Architecture("amd64") != "x86_64" || Architecture("arm64") != "aarch64" {
		t.Fatal("in the kernel's words")
	}
}

func TestValuesAreCutToThePlatformsLimitsOnACharacterBoundary(t *testing.T) {
	long := strings.Repeat("é", 300)
	packages := ParseDpkg([]byte("ii \t" + long + "\t1\tamd64\n"))

	if len([]rune(packages[0].Name)) != nameMax || !strings.HasPrefix(long, packages[0].Name) {
		t.Fatalf("cut to %d characters, whole ones: %d", nameMax, len([]rune(packages[0].Name)))
	}
}
