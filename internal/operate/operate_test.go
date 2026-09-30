package operate

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
)

// fake answers by program and first argument; it records every call.
type fake struct {
	calls    [][]string
	answers  map[string]answer
	programs map[string]bool
}

type answer struct {
	output string
	code   int
	err    error
}

func (f *fake) Has(program string) bool { return f.programs[program] }

func (f *fake) Run(_ context.Context, name string, args ...string) (string, int, error) {
	f.calls = append(f.calls, append([]string{name}, args...))

	key := name
	if len(args) > 0 {
		key += " " + args[0]
	}

	a, ok := f.answers[key]
	if !ok {
		return "", 0, nil
	}

	return a.output, a.code, a.err
}

func operator(allowed map[string][]string, f *fake) *Operator {
	return &Operator{Allowed: allowed, Exec: f, Now: func() time.Time { return time.Date(2026, 10, 1, 10, 0, 0, 0, time.UTC) }}
}

func restart(name string) Command {
	return Command{ID: "cmd_1", Operation: "service.restart", Parameters: map[string]string{"service": name}}
}

func TestOnlyWhatTheFileAllowsIsAdvertised(t *testing.T) {
	if got := operator(nil, &fake{}).Capabilities(); len(got) != 0 {
		t.Fatalf("nothing is allowed, yet %v is advertised", got)
	}

	got := operator(map[string][]string{"package.status": {Any}, "service.restart": {"nginx"}, "package.update": {"x"}, "backup.create": nil, "deployment.execute": {"x"}}, &fake{}).Capabilities()

	// In the platform's order; deployment.execute is not something this agent does yet, and backup.create has no names.
	if want := []string{"service.restart", "package.status", "package.update"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("advertised %v, want %v", got, want)
	}
}

func TestARestartIsBuiltFromFixedPartsAndChecked(t *testing.T) {
	f := &fake{answers: map[string]answer{"systemctl is-active": {output: "active\n"}}}
	result := operator(map[string][]string{"service.restart": {"nginx", "php8.3-fpm"}}, f).Do(context.Background(), restart("nginx"))

	if !result.Succeeded || result.ExitCode == nil || *result.ExitCode != 0 {
		t.Fatalf("not a success: %+v", result)
	}

	want := [][]string{
		{"systemctl", "restart", "--no-ask-password", "nginx.service"},
		{"systemctl", "is-active", "nginx.service"},
	}
	if !reflect.DeepEqual(f.calls, want) {
		t.Fatalf("ran %v, want %v", f.calls, want)
	}

	// A unit written with its suffix is the same one.
	f.calls = nil
	if !operator(map[string][]string{"service.restart": {"nginx.service"}}, f).Do(context.Background(), restart("nginx")).Succeeded {
		t.Fatal("nginx.service in the file should allow nginx")
	}
}

func TestWhatIsNotAllowedOrNotAPlainNameIsNeverRun(t *testing.T) {
	allowed := map[string][]string{"service.restart": {"nginx"}, "package.status": {Any}}

	for name, command := range map[string]Command{
		"a unit not in the file": restart("sshd"),
		"a shell":                restart("nginx; reboot"),
		"an option":              restart("--force"),
		"a path":                 restart("../../etc/passwd"),
		"a space":                restart("nginx sshd"),
		"nothing":                restart(""),
		"too long":               restart(strings.Repeat("a", 121)),
		"an operation not done":  {Operation: "deployment.execute", Parameters: map[string]string{"application": "x"}},
		"an unknown operation":   {Operation: "shell.run", Parameters: map[string]string{"command": "id"}},
		"a package as a service": {Operation: "service.restart", Parameters: map[string]string{"package": "nginx"}},
		"a bad package":          {Operation: "package.status", Parameters: map[string]string{"package": "a b"}},
	} {
		f := &fake{programs: map[string]bool{"dpkg-query": true}}
		result := operator(allowed, f).Do(context.Background(), command)

		if result.Succeeded || len(f.calls) != 0 {
			t.Errorf("%s: ran %v, result %+v", name, f.calls, result)
		}
	}
}

func TestAnExpiredCommandIsNotRun(t *testing.T) {
	f := &fake{}
	command := restart("nginx")
	command.ExpiresAt = time.Date(2026, 10, 1, 9, 59, 0, 0, time.UTC)

	result := operator(map[string][]string{"service.restart": {"nginx"}}, f).Do(context.Background(), command)

	if result.Succeeded || len(f.calls) != 0 || !strings.Contains(result.Output, "expired") {
		t.Fatalf("ran an expired command: %v %+v", f.calls, result)
	}
}

func TestAFailedOrNotActiveRestartIsFailure(t *testing.T) {
	allowed := map[string][]string{"service.restart": {"nginx"}}

	denied := operator(allowed, &fake{answers: map[string]answer{"systemctl restart": {output: "Access denied", code: 1}}}).Do(context.Background(), restart("nginx"))
	if denied.Succeeded || denied.ExitCode == nil || *denied.ExitCode != 1 || !strings.Contains(denied.Output, "Access denied") {
		t.Fatalf("a refused restart: %+v", denied)
	}

	down := operator(allowed, &fake{answers: map[string]answer{"systemctl is-active": {output: "failed\n", code: 3}}}).Do(context.Background(), restart("nginx"))
	if down.Succeeded || !strings.Contains(down.Output, "not active") {
		t.Fatalf("a unit that did not come back: %+v", down)
	}

	missing := operator(allowed, &fake{answers: map[string]answer{"systemctl restart": {err: errors.New("executable file not found")}}}).Do(context.Background(), restart("nginx"))
	if missing.Succeeded || missing.ExitCode == nil || *missing.ExitCode != 1 {
		t.Fatalf("no systemctl: %+v", missing)
	}
}

func TestPackageStatusReadsAndChangesNothing(t *testing.T) {
	allowed := map[string][]string{"package.status": {"openssl"}}
	command := Command{Operation: "package.status", Parameters: map[string]string{"package": "openssl"}}

	f := &fake{
		programs: map[string]bool{"dpkg-query": true, "apt-cache": true},
		answers: map[string]answer{
			"dpkg-query -W":    {output: "openssl 3.0.11-1 (ii )\n"},
			"apt-cache policy": {output: "openssl:\n  Installed: 3.0.11-1\n  Candidate: 3.0.13-1\n"},
		},
	}

	result := operator(allowed, f).Do(context.Background(), command)
	if !result.Succeeded || !strings.Contains(result.Output, "3.0.11-1") || !strings.Contains(result.Output, "Candidate: 3.0.13-1") {
		t.Fatalf("status: %+v", result)
	}

	for _, call := range f.calls {
		if call[0] != "dpkg-query" && call[0] != "apt-cache" {
			t.Errorf("ran %v, which is not a read", call)
		}
	}

	missing := operator(allowed, &fake{programs: map[string]bool{"dpkg-query": true}, answers: map[string]answer{"dpkg-query -W": {output: "no packages found", code: 1}}}).Do(context.Background(), command)
	if missing.Succeeded || !strings.Contains(missing.Output, "not installed") {
		t.Fatalf("a package that is not there: %+v", missing)
	}

	rpm := operator(allowed, &fake{programs: map[string]bool{"rpm": true}, answers: map[string]answer{"rpm -q": {output: "openssl 3.0.7-1\n"}}}).Do(context.Background(), command)
	if !rpm.Succeeded || !strings.Contains(rpm.Output, "3.0.7-1") {
		t.Fatalf("rpm: %+v", rpm)
	}

	none := operator(allowed, &fake{}).Do(context.Background(), command)
	if none.Succeeded {
		t.Fatalf("no package manager should not succeed: %+v", none)
	}
}

func TestAnUpdateStartsTheAdministratorsUnitForAnEscapedInstance(t *testing.T) {
	f := &fake{programs: map[string]bool{"dpkg-query": true}, answers: map[string]answer{"dpkg-query -W": {output: "libstdc++6 13.2.0-4 (ii )\n"}}}
	op := operator(map[string][]string{"package.update": {"libstdc++6", "openssl"}}, f)

	result := op.Do(context.Background(), Command{Operation: "package.update", Parameters: map[string]string{"package": "libstdc++6"}})
	if !result.Succeeded || !strings.Contains(result.Output, "13.2.0-4") {
		t.Fatalf("%+v", result)
	}

	want := []string{"systemctl", "start", "--no-ask-password", `laika-package-update@libstdc\x2b\x2b6.service`}
	if !reflect.DeepEqual(f.calls[0], want) {
		t.Fatalf("ran %v, want %v", f.calls[0], want)
	}

	for _, call := range f.calls {
		if call[0] == "apt-get" || call[0] == "dnf" || call[0] == "sudo" {
			t.Errorf("the agent ran a package manager itself: %v", call)
		}
	}

	failedUpdate := operator(map[string][]string{"package.update": {"openssl"}}, &fake{answers: map[string]answer{"systemctl start": {output: "Job failed", code: 1}}}).
		Do(context.Background(), Command{Operation: "package.update", Parameters: map[string]string{"package": "openssl"}})
	if failedUpdate.Succeeded || !strings.Contains(failedUpdate.Output, "Job failed") {
		t.Fatalf("%+v", failedUpdate)
	}
}

func TestInstanceNamesAreEscapedLikeSystemdEscape(t *testing.T) {
	for name, want := range map[string]string{
		"openssl":     "openssl",
		"python3-pip": `python3\x2dpip`,
		"libstdc++6":  `libstdc\x2b\x2b6`,
		"g++":         `g\x2b\x2b`,
		"a.b":         "a.b",
		"gtk+2.0":     `gtk\x2b2.0`,
		"x@y":         `x\x40y`,
	} {
		if got := EscapeInstance(name); got != want {
			t.Errorf("%s: %s, want %s", name, got, want)
		}
	}
}

func TestABackupStartsTheNamedJobAndWaitsForIt(t *testing.T) {
	f := &fake{}
	op := operator(map[string][]string{"backup.create": {"nightly-backup"}}, f)

	result := op.Do(context.Background(), Command{Operation: "backup.create", Parameters: map[string]string{"job": "nightly-backup"}})
	if !result.Succeeded {
		t.Fatalf("%+v", result)
	}

	if want := []string{"systemctl", "start", "--no-ask-password", "nightly-backup.service"}; !reflect.DeepEqual(f.calls[0], want) {
		t.Fatalf("ran %v", f.calls[0])
	}

	other := op.Do(context.Background(), Command{Operation: "backup.create", Parameters: map[string]string{"job": "sshd"}})
	if other.Succeeded || len(f.calls) != 1 {
		t.Fatalf("a job the file does not list ran: %+v", other)
	}

	bad := operator(map[string][]string{"backup.create": {"nightly"}}, &fake{answers: map[string]answer{"systemctl start": {output: "exit status 2", code: 1}}}).
		Do(context.Background(), Command{Operation: "backup.create", Parameters: map[string]string{"job": "nightly"}})
	if bad.Succeeded || bad.ExitCode == nil {
		t.Fatalf("%+v", bad)
	}
}

func TestAnyNameIsOnlyForOperationsThatChangeNothing(t *testing.T) {
	f := &fake{}
	op := operator(map[string][]string{"service.restart": {Any}, "package.update": {Any}, "backup.create": {Any}, "package.status": {Any}}, f)

	if got := op.Capabilities(); !reflect.DeepEqual(got, []string{"package.status"}) {
		t.Fatalf("advertised %v: * must not open a restart, an update or a backup", got)
	}

	for _, command := range []Command{
		{Operation: "service.restart", Parameters: map[string]string{"service": "sshd"}},
		{Operation: "package.update", Parameters: map[string]string{"package": "openssl"}},
		{Operation: "backup.create", Parameters: map[string]string{"job": "anything"}},
	} {
		if result := op.Do(context.Background(), command); result.Succeeded || len(f.calls) != 0 {
			t.Errorf("%s ran: %v", command.Operation, f.calls)
		}
	}

	// All four are advertised, in the platform's order, when each is listed by name.
	all := operator(map[string][]string{"backup.create": {"b"}, "package.update": {"p"}, "package.status": {Any}, "service.restart": {"s"}}, &fake{}).Capabilities()
	if want := []string{"service.restart", "package.status", "package.update", "backup.create"}; !reflect.DeepEqual(all, want) {
		t.Fatalf("%v", all)
	}
}
