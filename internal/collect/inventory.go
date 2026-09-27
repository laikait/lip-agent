package collect

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"time"
)

// The inventory: what the machine is and what runs on it (protocol v1's
// inventory call, platform Phase 2 Step 3). Every parser takes bytes, so it
// is tested with fixtures on any operating system; running the commands and
// reading the files is Linux's.

// Hardware is the machine itself.
type Hardware struct {
	CPUCores       int    `json:"cpuCores,omitempty"`
	MemoryBytes    uint64 `json:"memoryBytes,omitempty"`
	Architecture   string `json:"architecture,omitempty"`
	Virtualization string `json:"virtualization,omitempty"`
}

// Address is one network address and the interface it is on.
type Address struct {
	Interface string `json:"interface"`
	Address   string `json:"address"`
}

// Package is one installed package.
type Package struct {
	Name         string `json:"name"`
	Version      string `json:"version"`
	Architecture string `json:"architecture,omitempty"`
	Manager      string `json:"manager"`
}

// CronJob is one cron line, without its arguments: its schedule, its user,
// the program it runs, and the SHA-256 of the whole line, which says that it
// changed without saying what it says. **The arguments never leave the
// machine**: a line such as `mysqldump -pSECRET` carries passwords.
type CronJob struct {
	Schedule string `json:"schedule"`
	User     string `json:"user,omitempty"`
	Program  string `json:"program"`
	LineHash string `json:"lineHash"`
	File     string `json:"file"`
}

// Timer is one systemd timer.
type Timer struct {
	Name     string `json:"name"`
	Schedule string `json:"schedule,omitempty"`
	Unit     string `json:"unit,omitempty"`
}

// The most of each the protocol takes.
const (
	PackagesMax  = 5000
	CronJobsMax  = 500
	TimersMax    = 500
	AddressesMax = 64
)

// ErrNoPackageManager means none of dpkg, rpm or apk answered: the
// machine's packages are not reported, and the platform keeps what it knew.
var ErrNoPackageManager = errors.New("no package manager (dpkg, rpm or apk) answered")

// ParseDpkg reads `dpkg-query -W -f='${db:Status-Abbrev}\t${Package}\t${Version}\t${Architecture}\n'`,
// keeping only what is installed ("ii"): dpkg also remembers removed
// packages whose configuration is still there.
func ParseDpkg(output []byte) []Package {
	var packages []Package

	for _, line := range lines(output) {
		fields := strings.Split(line, "\t")
		if len(fields) < 4 || strings.TrimSpace(fields[0]) != "ii" || fields[1] == "" {
			continue
		}

		packages = append(packages, Package{Name: fields[1], Version: fields[2], Architecture: fields[3], Manager: "dpkg"})
	}

	return capped(packages)
}

// ParseRpm reads `rpm -qa --qf '%{NAME}\t%{VERSION}-%{RELEASE}\t%{ARCH}\n'`.
func ParseRpm(output []byte) []Package {
	var packages []Package

	for _, line := range lines(output) {
		fields := strings.Split(line, "\t")
		if len(fields) < 3 || fields[0] == "" {
			continue
		}

		arch := fields[2]
		if arch == "(none)" {
			arch = ""
		}

		packages = append(packages, Package{Name: fields[0], Version: fields[1], Architecture: arch, Manager: "rpm"})
	}

	return capped(packages)
}

// ParseApk reads `apk info -v`: name-version-rN on each line. The version is
// the last two parts, split at "-"; a name may hold dashes itself.
func ParseApk(output []byte) []Package {
	var packages []Package

	for _, line := range lines(output) {
		parts := strings.Split(line, "-")
		if len(parts) < 3 || !strings.HasPrefix(parts[len(parts)-1], "r") {
			continue
		}

		packages = append(packages, Package{
			Name:    strings.Join(parts[:len(parts)-2], "-"),
			Version: parts[len(parts)-2] + "-" + parts[len(parts)-1],
			Manager: "apk",
		})
	}

	return capped(packages)
}

// Packages asks the package manager the machine has, directly and never
// through a shell, with a deadline. Under a host root other than "/" (the
// agent in a container), each is pointed at the host's database.
func Packages(ctx context.Context, root string) ([]Package, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	host := root != "" && root != "/"

	if path, err := exec.LookPath("dpkg-query"); err == nil {
		args := []string{"-W", "-f=${db:Status-Abbrev}\t${Package}\t${Version}\t${Architecture}\n"}
		if host {
			args = append([]string{"--admindir=" + filepath.Join(root, "var/lib/dpkg")}, args...)
		}

		if output, err := exec.CommandContext(ctx, path, args...).Output(); err == nil {
			return ParseDpkg(output), nil
		}
	}

	if path, err := exec.LookPath("rpm"); err == nil {
		args := []string{"-qa", "--qf", "%{NAME}\t%{VERSION}-%{RELEASE}\t%{ARCH}\n"}
		if host {
			args = append([]string{"--root", root}, args...)
		}

		if output, err := exec.CommandContext(ctx, path, args...).Output(); err == nil {
			return ParseRpm(output), nil
		}
	}

	if path, err := exec.LookPath("apk"); err == nil {
		args := []string{"info", "-v"}
		if host {
			args = append([]string{"--root", root}, args...)
		}

		if output, err := exec.CommandContext(ctx, path, args...).Output(); err == nil {
			return ParseApk(output), nil
		}
	}

	return nil, ErrNoPackageManager
}

var (
	assignment = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*=`)
	// What cron and run-parts accept as a file name in cron.d and the
	// cron.daily-style directories (Debian's rule); anything else, such as
	// a backup file.dpkg-old, is not run and not reported.
	runnable = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)
)

// ParseCrontab reads a crontab: a system one (/etc/crontab, /etc/cron.d/*)
// has a user before the command, which withUser says. Comments, blank lines
// and variable assignments are not jobs.
func ParseCrontab(data []byte, file string, withUser bool) []CronJob {
	var jobs []CronJob

	for _, line := range lines(data) {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") || assignment.MatchString(line) {
			continue
		}

		fields := strings.Fields(line)
		scheduleFields := 5

		if strings.HasPrefix(fields[0], "@") {
			scheduleFields = 1
		}

		userFields := 0
		if withUser {
			userFields = 1
		}

		if len(fields) <= scheduleFields+userFields {
			continue
		}

		job := CronJob{
			Schedule: cut(strings.Join(fields[:scheduleFields], " "), scheduleMax),
			Program:  cut(program(fields[scheduleFields+userFields:]), programMax),
			LineHash: hash(line),
			File:     cut(file, fileMax),
		}

		if withUser {
			job.User = cut(fields[scheduleFields], userMax)
		}

		if job.Program != "" {
			jobs = append(jobs, job)
		}
	}

	return jobs
}

// program is the command a cron line runs, and nothing else of it.
//
// Leading assignments (`PGPASSWORD=… pg_dump`) are skipped, values unseen.
// A line that tests a program before running it (`[ -x /usr/lib/php/sessionclean ] && …`,
// Debian's habit) runs that program, so that path is taken: a path tested
// with -x is a program by definition, not a secret. Only -x: a path tested
// with -e or -f (`test -e /run/systemd/system || …`) is just an argument,
// and stays on the machine like any other.
func program(command []string) string {
	for len(command) > 0 && assignment.MatchString(command[0]) {
		command = command[1:]
	}

	if len(command) == 0 {
		return ""
	}

	if (command[0] == "[" || command[0] == "test") && len(command) > 2 && command[1] == "-x" {
		return command[2]
	}

	return command[0]
}

// CronJobs reads the system's cron: /etc/crontab, /etc/cron.d, and the
// programs in /etc/cron.{hourly,daily,weekly,monthly}, under root. Users'
// own crontabs (/var/spool/cron) need root to read and are not reported.
//
// An empty list is an answer ("none"); an error means /etc could not be read
// at all, and nothing is reported.
func CronJobs(root string) ([]CronJob, error) {
	if root == "" {
		root = "/"
	}

	if _, err := os.Stat(filepath.Join(root, "etc")); err != nil {
		return nil, err
	}

	jobs := []CronJob{}

	if data, err := os.ReadFile(filepath.Join(root, "etc/crontab")); err == nil {
		jobs = append(jobs, ParseCrontab(data, "/etc/crontab", true)...)
	}

	for _, name := range entries(filepath.Join(root, "etc/cron.d")) {
		if data, err := os.ReadFile(filepath.Join(root, "etc/cron.d", name)); err == nil {
			jobs = append(jobs, ParseCrontab(data, "/etc/cron.d/"+name, true)...)
		}
	}

	for _, period := range []string{"hourly", "daily", "weekly", "monthly"} {
		dir := "/etc/cron." + period

		for _, name := range entries(filepath.Join(root, dir)) {
			path := dir + "/" + name
			jobs = append(jobs, CronJob{Schedule: "@" + period, User: "root", Program: cut(path, programMax), LineHash: hash("run-parts " + path), File: dir})
		}
	}

	if len(jobs) > CronJobsMax {
		jobs = jobs[:CronJobsMax]
	}

	return jobs, nil
}

// entries is a directory's runnable regular files, by name.
func entries(dir string) []string {
	list, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}

	var names []string

	for _, entry := range list {
		if entry.Type().IsRegular() && runnable.MatchString(entry.Name()) {
			names = append(names, entry.Name())
		}
	}

	sort.Strings(names)

	return names
}

// ParseTimerUnits reads `systemctl list-units --type=timer --all --no-legend --plain`:
// the timers' unit names.
func ParseTimerUnits(output []byte) []string {
	var names []string

	for _, line := range lines(output) {
		fields := strings.Fields(line)
		if len(fields) > 0 && (fields[0] == "●" || fields[0] == "*") {
			fields = fields[1:]
		}

		if len(fields) > 0 && strings.HasSuffix(fields[0], ".timer") {
			names = append(names, fields[0])
		}
	}

	return names
}

var trigger = regexp.MustCompile(`\{ (On[A-Za-z]+)=([^;]*?) ;`)

// ParseTimers reads `systemctl show <timers> --property=Id,Unit,TimersCalendar,TimersMonotonic`:
// a block per timer, blank lines between. A calendar timer's schedule is its
// OnCalendar expression; a monotonic one's is how it is written
// (OnUnitActiveSec=1d).
func ParseTimers(output []byte) []Timer {
	var timers []Timer

	var current *Timer

	flush := func() {
		if current != nil && current.Name != "" {
			timers = append(timers, Timer{Name: cut(current.Name, nameMax), Schedule: cut(current.Schedule, scheduleMax), Unit: cut(current.Unit, nameMax)})
		}

		current = nil
	}

	for _, line := range strings.Split(string(output), "\n") {
		line = strings.TrimRight(line, "\r")
		if strings.TrimSpace(line) == "" {
			flush()

			continue
		}

		if current == nil {
			current = &Timer{}
		}

		key, value, _ := strings.Cut(line, "=")

		switch key {
		case "Id":
			current.Name = strings.TrimSuffix(value, ".timer")
		case "Unit":
			current.Unit = value
		case "TimersCalendar", "TimersMonotonic":
			for _, match := range trigger.FindAllStringSubmatch(value, -1) {
				schedule := strings.TrimSpace(match[2])
				if match[1] != "OnCalendar" {
					schedule = match[1] + "=" + schedule
				}

				if current.Schedule == "" {
					current.Schedule = schedule
				} else {
					current.Schedule += ", " + schedule
				}
			}
		}
	}

	flush()

	if len(timers) > TimersMax {
		timers = timers[:TimersMax]
	}

	return timers
}

// Timers asks systemd for its timers: two calls, directly, with a deadline.
func Timers(ctx context.Context) ([]Timer, error) {
	path, err := exec.LookPath("systemctl")
	if err != nil {
		return nil, ErrNoSystemd
	}

	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	listed, err := exec.CommandContext(ctx, path, "list-units", "--type=timer", "--all", "--no-legend", "--plain", "--no-pager").Output()
	if err != nil {
		return nil, err
	}

	names := ParseTimerUnits(listed)
	if len(names) == 0 {
		return nil, nil
	}

	if len(names) > TimersMax {
		names = names[:TimersMax]
	}

	shown, err := exec.CommandContext(ctx, path, append([]string{"show", "--property=Id,Unit,TimersCalendar,TimersMonotonic", "--no-pager"}, names...)...).Output()
	if err != nil {
		return nil, err
	}

	return ParseTimers(shown), nil
}

// ParseCPUInfo counts the processors /proc/cpuinfo lists: the cores the
// kernel schedules on, hyperthreads included.
func ParseCPUInfo(data []byte) int {
	count := 0

	for _, line := range lines(data) {
		if key, _, found := strings.Cut(line, ":"); found && strings.TrimSpace(key) == "processor" {
			count++
		}
	}

	return count
}

// Architecture is the machine's, in the kernel's words (uname -m). The
// agent is built for the machine it runs on, so Go's name for it is mapped.
func Architecture(goarch string) string {
	switch goarch {
	case "amd64":
		return "x86_64"
	case "arm64":
		return "aarch64"
	case "386":
		return "i686"
	default:
		return goarch
	}
}

// Hardware reads the machine: its cores and memory under Root, its
// architecture, and what it runs on (systemd-detect-virt, when there is one;
// "none" is bare metal and reads as nothing).
func (s *Sampler) Hardware(ctx context.Context) Hardware {
	hardware := Hardware{Architecture: Architecture(runtime.GOARCH)}

	if data, err := s.file("proc/cpuinfo"); err == nil {
		hardware.CPUCores = ParseCPUInfo(data)
	}

	if memory, err := read(s, "proc/meminfo", ParseMeminfo); err == nil {
		hardware.MemoryBytes = memory.Total
	}

	if path, err := exec.LookPath("systemd-detect-virt"); err == nil {
		ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()

		// It exits 1 on bare metal, having said "none".
		output, _ := exec.CommandContext(ctx, path).Output()
		if virt := strings.TrimSpace(string(output)); virt != "none" && len(virt) <= 40 {
			hardware.Virtualization = virt
		}
	}

	return hardware
}

// Addresses is the machine's addresses, from the interfaces that are up,
// without loopback and link-local ones, which say nothing about where it
// is. In a container they are the container's unless it shares the host's
// network.
func Addresses() []Address {
	interfaces, err := net.Interfaces()
	if err != nil {
		return nil
	}

	var addresses []Address

	for _, iface := range interfaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}

		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}

		for _, addr := range addrs {
			network, ok := addr.(*net.IPNet)
			if !ok || network.IP.IsLoopback() || network.IP.IsLinkLocalUnicast() {
				continue
			}

			addresses = append(addresses, Address{Interface: iface.Name, Address: network.IP.String()})

			if len(addresses) == AddressesMax {
				return addresses
			}
		}
	}

	return addresses
}

func capped(packages []Package) []Package {
	packages = fitted(packages)

	sort.Slice(packages, func(i, j int) bool {
		if packages[i].Name != packages[j].Name {
			return packages[i].Name < packages[j].Name
		}

		return packages[i].Architecture < packages[j].Architecture
	})

	if len(packages) > PackagesMax {
		packages = packages[:PackagesMax]
	}

	return packages
}

func lines(data []byte) []string {
	var out []string

	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	for scanner.Scan() {
		if line := strings.TrimRight(scanner.Text(), "\r"); line != "" {
			out = append(out, line)
		}
	}

	return out
}

func hash(text string) string {
	sum := sha256.Sum256([]byte(text))

	return hex.EncodeToString(sum[:])
}

// The platform's limits, in characters (its AgentSchema::inventory()). A
// value over one would have the whole report refused, so each is cut to fit
// here, on a character boundary: a cut through a UTF-8 sequence would be
// refused too.
const (
	nameMax     = 190
	versionMax  = 120
	archMax     = 20
	userMax     = 32
	programMax  = 255
	scheduleMax = 190
	fileMax     = 255
)

func cut(value string, max int) string {
	if len(value) <= max {
		return value
	}

	runes := []rune(value)
	if len(runes) <= max {
		return value
	}

	return string(runes[:max])
}

// fitted is the package list cut to the platform's limits: long values
// shortened, and a package with no name left out.
func fitted(packages []Package) []Package {
	out := packages[:0]

	for _, p := range packages {
		p.Name = cut(p.Name, nameMax)
		p.Version = cut(p.Version, versionMax)
		p.Architecture = cut(p.Architecture, archMax)

		if p.Name != "" {
			out = append(out, p)
		}
	}

	return out
}
