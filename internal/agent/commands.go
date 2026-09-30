package agent

import (
	"context"
	"errors"
	"log"
	"time"

	"github.com/laikait/lip-agent/internal/operate"
	"github.com/laikait/lip-agent/internal/protocol"
)

const (
	defaultPoll = 15 * time.Second
	minPoll     = 5 * time.Second
	maxPoll     = 5 * time.Minute

	// A platform older than the commands call is not asked again for this long.
	oldPlatformWait = time.Hour

	// Command ids remembered, so a command handed over twice runs once.
	seenMax = 500
)

// resultWaits are the pauses before each try at telling the platform what
// came of a command. The command has run by then, so it is worth a few tries,
// but not forever: an answer the platform never gets is a command it lets
// lapse.
var resultWaits = []time.Duration{0, 2 * time.Second, 10 * time.Second, 30 * time.Second}

// CommandPlatform is the part of the protocol client commands use.
type CommandPlatform interface {
	Commands(ctx context.Context) (protocol.Commands, error)
	Result(ctx context.Context, commandID string, result protocol.CommandResult) (bool, error)
}

// Commander asks the platform what it wants done, does it, and says what
// came of it. Polling only: nothing is pushed to this machine and no port is
// opened.
type Commander struct {
	Platform CommandPlatform
	Operator *operate.Operator
	Log      *log.Logger
	Now      func() time.Time
	// Wait pauses; nil sleeps. A test replaces it.
	Wait func(ctx context.Context, d time.Duration)

	interval time.Duration
	seen     map[string]bool
}

// Run polls until ctx is done (nil), or the credential is refused
// (ErrRevoked). An agent that is allowed to do nothing does not poll at all.
func (c *Commander) Run(ctx context.Context) error {
	c.init()

	if len(c.Operator.Capabilities()) == 0 {
		return nil
	}

	c.Log.Printf("commands: asking every %s for %v", c.interval, c.Operator.Capabilities())

	for {
		if err := c.Poll(ctx); errors.Is(err, ErrRevoked) {
			return err
		}

		select {
		case <-ctx.Done():
			return nil
		case <-time.After(c.interval):
		}
	}
}

func (c *Commander) init() {
	if c.interval == 0 {
		c.interval = defaultPoll
	}

	if c.seen == nil {
		c.seen = map[string]bool{}
	}

	if c.Log == nil {
		c.Log = log.Default()
	}

	if c.Now == nil {
		c.Now = time.Now
	}

	if c.Wait == nil {
		c.Wait = func(ctx context.Context, d time.Duration) {
			select {
			case <-ctx.Done():
			case <-time.After(d):
			}
		}
	}
}

// Poll is one round: ask, do what was handed over, answer each.
func (c *Commander) Poll(ctx context.Context) error {
	c.init()

	commands, err := c.Platform.Commands(ctx)

	switch {
	case protocol.Unauthorized(err):
		c.Log.Print(ErrRevoked)

		return ErrRevoked
	case protocol.Rejected(err):
		// A platform that does not have the call yet.
		c.Log.Printf("the platform has no commands to give (%v); asking again in %s", err, oldPlatformWait)
		c.interval = oldPlatformWait

		return nil
	case err != nil:
		c.interval = min(max(c.interval*2, defaultPoll), maxPoll)
		c.Log.Printf("commands: %v; trying again in %s", err, c.interval)

		return err
	}

	c.interval = defaultPoll
	if commands.PollSeconds > 0 {
		c.interval = min(max(time.Duration(commands.PollSeconds)*time.Second, minPoll), maxPoll)
	}

	for _, wire := range commands.Commands {
		if c.seen[wire.CommandID] {
			continue
		}

		if len(c.seen) >= seenMax {
			c.seen = map[string]bool{}
		}

		c.seen[wire.CommandID] = true

		command := operate.Command{ID: wire.CommandID, Operation: wire.Operation, Parameters: wire.Parameters}
		if expires, err := time.Parse(time.RFC3339, wire.ExpiresAt); err == nil {
			command.ExpiresAt = expires
		}

		c.Log.Printf("command %s: %s", command, describe(wire))
		result := c.Operator.Do(ctx, command)
		c.Log.Printf("command %s: %s", command, outcome(result))

		if err := c.tell(ctx, wire.CommandID, result); errors.Is(err, ErrRevoked) {
			return err
		}
	}

	return nil
}

// tell says what came of a command, trying a few times.
func (c *Commander) tell(ctx context.Context, id string, result operate.Result) error {
	status := "failed"
	if result.Succeeded {
		status = "succeeded"
	}

	body := protocol.CommandResult{Status: status, ExitCode: result.ExitCode, Output: result.Output}

	var last error

	for _, wait := range resultWaits {
		if wait > 0 {
			c.Wait(ctx, wait)
		}

		if ctx.Err() != nil {
			break
		}

		_, err := c.Platform.Result(ctx, id, body)

		switch {
		case err == nil:
			return nil
		case protocol.Unauthorized(err):
			c.Log.Print(ErrRevoked)

			return ErrRevoked
		case protocol.Rejected(err):
			c.Log.Printf("command %s: the platform refused the answer, which is dropped: %v", id, err)

			return nil
		}

		last = err
	}

	c.Log.Printf("command %s: the platform could not be told (%v); it lets the command lapse", id, last)

	return last
}

func describe(wire protocol.WireCommand) string {
	for _, value := range wire.Parameters {
		return wire.Operation + " " + value
	}

	return wire.Operation
}

func outcome(result operate.Result) string {
	if result.Succeeded {
		return "done"
	}

	return "not done: " + result.Output
}
