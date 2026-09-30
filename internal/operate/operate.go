// Package operate does the few named operations the platform may ask for
// (plans/agent.md and plans/04-automation.md in the platform's repository).
//
// **There is no shell and no command line from the platform.** It sends an
// operation's name and one parameter; this package checks the parameter is a
// plain name, checks the local administrator allowed that name on this
// machine (the config file's "operations"), and builds the argument list
// itself. An operation the file does not allow is not advertised to the
// platform, and is refused if asked for anyway.
package operate

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// Order is the platform's order for advertised operations
// (Agent\Model\Operations): the agent lists its capabilities in it.
var Order = []string{
	"metrics.read", "service.status", "inventory.read",
	"service.restart", "package.status", "package.update", "deployment.execute", "backup.create",
}

// Any allows every name of an operation in the config file. Only for
// operations that change nothing.
const Any = "*"

// valuePattern is what the platform accepts too: a plain name, no path, no
// option (it starts with a letter or digit), at most 120 characters.
var valuePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9@+._:-]{0,119}$`)

// Command is what the platform asked for.
type Command struct {
	ID         string
	Operation  string
	Parameters map[string]string
	ExpiresAt  time.Time
}

// Result is what came of it, in the words the platform takes.
type Result struct {
	Succeeded bool
	ExitCode  *int
	Output    string
}

// Executor runs one program with an argument list. Never a shell.
type Executor interface {
	Run(ctx context.Context, name string, args ...string) (output string, exitCode int, err error)

	// Has is whether a program of that name can be found here.
	Has(program string) bool
}

// Operator does the operations this machine allows.
type Operator struct {
	// Allowed is the config file's "operations": each operation to the names
	// it may be asked about (or Any).
	Allowed map[string][]string
	Exec    Executor
	Now     func() time.Time
}

// operations maps a name to its parameter and what it does.
var operations = map[string]struct {
	parameter string
	do        func(o *Operator, ctx context.Context, value string) Result
	readOnly  bool
	// within is how long the operation may take, when a minute is not enough.
	within time.Duration
}{
	"service.restart": {"service", (*Operator).restartService, false, 0},
	"package.status":  {"package", (*Operator).packageStatus, true, 0},
	"package.update":  {"package", (*Operator).updatePackage, false, PackageUpdateWithin},
	"backup.create":   {"job", (*Operator).createBackup, false, BackupWithin},
}

const (
	// PackageUpdateWithin and BackupWithin are the longest the two slow
	// operations may take. The platform accepts an answer for a command until
	// 30 minutes after it was queued and the agent takes it within 15 seconds
	// of a poll, so these keep the answer inside that.
	PackageUpdateWithin = 10 * time.Minute
	BackupWithin        = 20 * time.Minute

	// UpdateUnit is the systemd template unit the administrator installs
	// (packaging/laika-package-update@.service), which does the update as root.
	UpdateUnit = "laika-package-update@"
)

// Capabilities are the operations this machine has allowed, in the
// platform's order. An operation with an empty list is not allowed.
func (o *Operator) Capabilities() []string {
	var names []string

	for _, name := range Order {
		if known, ok := operations[name]; ok && len(o.names(name, known.readOnly)) > 0 {
			names = append(names, name)
		}
	}

	return names
}

// Do carries out one command, or says why it did not. It never panics on
// what the platform sent.
func (o *Operator) Do(ctx context.Context, command Command) Result {
	now := time.Now
	if o.Now != nil {
		now = o.Now
	}

	operation, known := operations[command.Operation]
	if !known || len(o.names(command.Operation, operation.readOnly)) == 0 {
		return refused("this agent is not set up to do " + command.Operation + " (see \"operations\" in its file)")
	}

	if !command.ExpiresAt.IsZero() && now().After(command.ExpiresAt) {
		return refused("not run: it expired before this agent could run it")
	}

	value := command.Parameters[operation.parameter]
	if !valuePattern.MatchString(value) {
		return refused("not run: the " + operation.parameter + " is not a plain name")
	}

	if !o.allows(command.Operation, value, operation.readOnly) {
		return refused("not run: \"" + value + "\" is not one of the " + operation.parameter + "s this agent allows for " + command.Operation)
	}

	if operation.within > 0 {
		var cancel context.CancelFunc

		ctx, cancel = context.WithTimeout(ctx, operation.within)
		defer cancel()
	}

	return operation.do(o, ctx, value)
}

// names are the entries of the file that count for an operation: "*" counts
// only for one that changes nothing, so a file that says "*" for a restart
// allows nothing.
func (o *Operator) names(operation string, readOnly bool) []string {
	var names []string

	for _, allowed := range o.Allowed[operation] {
		if allowed != Any || readOnly {
			names = append(names, allowed)
		}
	}

	return names
}

func (o *Operator) allows(operation, value string, readOnly bool) bool {
	for _, allowed := range o.names(operation, readOnly) {
		if allowed == Any || strings.TrimSuffix(allowed, ".service") == strings.TrimSuffix(value, ".service") {
			return true
		}
	}

	return false
}

func refused(why string) Result {
	return Result{Output: why}
}

// restartService restarts one systemd unit, then checks it came back. It
// runs systemctl as the agent's own user: what it may restart is up to
// polkit's rules for that user, which the administrator wrote for the same
// names (README), so the file's list is the first gate and the system's the
// last.
func (o *Operator) restartService(ctx context.Context, name string) Result {
	unit := strings.TrimSuffix(name, ".service") + ".service"

	output, code, err := o.Exec.Run(ctx, "systemctl", "restart", "--no-ask-password", unit)
	if err != nil || code != 0 {
		return failed(code, err, "systemctl restart "+unit+" failed", output)
	}

	state, code, err := o.Exec.Run(ctx, "systemctl", "is-active", unit)
	state = strings.TrimSpace(state)

	if err != nil || code != 0 || state != "active" {
		return failed(code, err, unit+" was restarted but is not active: "+state, output)
	}

	zero := 0

	return Result{Succeeded: true, ExitCode: &zero, Output: "restarted " + unit + "; it is active"}
}

// updatePackage brings one installed package to the newest version the
// system offers. The agent does not run a package manager, which needs root:
// it starts a systemd template unit the administrator installed
// (laika-package-update@.service), which does. The polkit rule that lets this
// user start it is written for the same names as the file's list. The
// package's name goes into the unit's instance name, escaped the way
// systemd-escape does.
func (o *Operator) updatePackage(ctx context.Context, name string) Result {
	unit := UpdateUnit + EscapeInstance(name) + ".service"

	output, code, err := o.Exec.Run(ctx, "systemctl", "start", "--no-ask-password", unit)
	if err != nil || code != 0 {
		return failed(code, err, "updating "+name+" failed ("+unit+")", output)
	}

	after := o.packageStatus(ctx, name)
	zero := 0

	return Result{Succeeded: true, ExitCode: &zero, Output: "updated " + name + "\n" + after.Output}
}

// createBackup starts one of the machine's own backup jobs, a systemd service
// the administrator wrote and named in the file, and waits for it to end. The
// job decides what a backup is; the platform only says when.
func (o *Operator) createBackup(ctx context.Context, name string) Result {
	unit := strings.TrimSuffix(name, ".service") + ".service"

	output, code, err := o.Exec.Run(ctx, "systemctl", "start", "--no-ask-password", unit)
	if err != nil || code != 0 {
		return failed(code, err, "the backup job "+unit+" failed", output)
	}

	zero := 0

	return Result{Succeeded: true, ExitCode: &zero, Output: "the backup job " + unit + " ran and ended without error"}
}

// EscapeInstance is a package's name as a systemd unit instance: anything
// but a letter, digit, ":", "_" or "." (and a leading ".") becomes \xHH, as
// systemd-escape does, so "libstdc++6" and "python3-pip" are legal instance
// names. The template unit reads it back unescaped (%I).
func EscapeInstance(name string) string {
	var b strings.Builder

	for i := 0; i < len(name); i++ {
		c := name[i]

		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == ':', c == '_':
			b.WriteByte(c)
		case c == '.' && i > 0:
			b.WriteByte(c)
		default:
			fmt.Fprintf(&b, "\\x%02x", c)
		}
	}

	return b.String()
}

// packageStatus reads what is installed and, where the system can say, what
// is available. It changes nothing.
func (o *Operator) packageStatus(ctx context.Context, name string) Result {
	var lines []string

	switch {
	case o.Exec.Has("dpkg-query"):
		output, code, err := o.Exec.Run(ctx, "dpkg-query", "-W", "-f=${Package} ${Version} (${db:Status-Abbrev})\n", name)
		if err != nil || code != 0 {
			return failed(code, err, name+" is not installed", output)
		}

		lines = append(lines, "installed: "+strings.TrimSpace(output))

		if policy, code, err := o.Exec.Run(ctx, "apt-cache", "policy", name); err == nil && code == 0 {
			lines = append(lines, strings.TrimSpace(policy))
		}
	case o.Exec.Has("rpm"):
		output, code, err := o.Exec.Run(ctx, "rpm", "-q", "--qf", "%{NAME} %{VERSION}-%{RELEASE}\n", name)
		if err != nil || code != 0 {
			return failed(code, err, name+" is not installed", output)
		}

		lines = append(lines, "installed: "+strings.TrimSpace(output))
	case o.Exec.Has("apk"):
		output, code, err := o.Exec.Run(ctx, "apk", "info", "-v", name)
		if err != nil || code != 0 || strings.TrimSpace(output) == "" {
			return failed(code, err, name+" is not installed", output)
		}

		lines = append(lines, "installed: "+strings.TrimSpace(output))
	default:
		return refused("no package manager this agent knows (dpkg, rpm or apk)")
	}

	zero := 0

	return Result{Succeeded: true, ExitCode: &zero, Output: strings.Join(lines, "\n")}
}

func failed(code int, err error, why, output string) Result {
	text := why

	if err != nil {
		text += ": " + err.Error()
	}

	if trimmed := strings.TrimSpace(output); trimmed != "" {
		text += "\n" + trimmed
	}

	if code == 0 {
		code = 1
	}

	return Result{ExitCode: &code, Output: text}
}

// String is for logs.
func (c Command) String() string {
	return fmt.Sprintf("%s %s", c.ID, c.Operation)
}
