package operate

import (
	"bytes"
	"context"
	"errors"
	"os/exec"
	"time"
)

const (
	// OutputMax is the most output kept from one program: the platform keeps
	// 4,000 characters, and a runaway program must not fill this process.
	OutputMax = 8 << 10

	// Deadline is how long one program may run unless its caller says otherwise.
	Deadline = 60 * time.Second
)

// System runs programs on this machine, directly, never through a shell,
// with a deadline and a cap on what it reads back.
type System struct{}

// Has is whether the program is on the path.
func (System) Has(program string) bool {
	_, err := exec.LookPath(program)

	return err == nil
}

// Run runs a program. A program that ran and failed is a non-zero exit code
// and no error; one that could not start or ran out of time is an error.
func (System) Run(ctx context.Context, name string, args ...string) (string, int, error) {
	path, err := exec.LookPath(name)
	if err != nil {
		return "", -1, err
	}

	// A caller that set its own deadline (a backup) has it; the rest get a minute.
	if _, set := ctx.Deadline(); !set {
		var cancel context.CancelFunc

		ctx, cancel = context.WithTimeout(ctx, Deadline)
		defer cancel()
	}

	var output bytes.Buffer

	command := exec.CommandContext(ctx, path, args...)
	command.Stdout = &limited{buffer: &output, room: OutputMax}
	command.Stderr = command.Stdout

	err = command.Run()

	var exited *exec.ExitError
	if errors.As(err, &exited) {
		return output.String(), exited.ExitCode(), nil
	}

	if err != nil {
		return output.String(), -1, err
	}

	return output.String(), 0, nil
}

// limited keeps the first room bytes and quietly drops the rest, so the
// program is never blocked on a full pipe.
type limited struct {
	buffer *bytes.Buffer
	room   int
}

func (l *limited) Write(p []byte) (int, error) {
	if left := l.room - l.buffer.Len(); left > 0 {
		l.buffer.Write(p[:min(len(p), left)])
	}

	return len(p), nil
}
