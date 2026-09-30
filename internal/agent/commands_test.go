package agent

import (
	"context"
	"errors"
	"io"
	"log"
	"net/http"
	"testing"
	"time"

	"github.com/laikait/lip-agent/internal/operate"
	"github.com/laikait/lip-agent/internal/protocol"
)

type fakeCommands struct {
	polls   []protocol.Commands
	pollErr error
	results []protocol.CommandResult
	ids     []string
	// resultErrs are returned by successive Result calls.
	resultErrs []error
}

func (f *fakeCommands) Commands(context.Context) (protocol.Commands, error) {
	if f.pollErr != nil {
		return protocol.Commands{}, f.pollErr
	}

	if len(f.polls) == 0 {
		return protocol.Commands{}, nil
	}

	next := f.polls[0]
	f.polls = f.polls[1:]

	return next, nil
}

func (f *fakeCommands) Result(_ context.Context, id string, result protocol.CommandResult) (bool, error) {
	f.ids = append(f.ids, id)
	f.results = append(f.results, result)

	if len(f.resultErrs) > 0 {
		err := f.resultErrs[0]
		f.resultErrs = f.resultErrs[1:]

		return false, err
	}

	return true, nil
}

type ran struct{ calls [][]string }

func (r *ran) Has(string) bool { return false }

func (r *ran) Run(_ context.Context, name string, args ...string) (string, int, error) {
	r.calls = append(r.calls, append([]string{name}, args...))

	if len(args) > 0 && args[0] == "is-active" {
		return "active\n", 0, nil
	}

	return "", 0, nil
}

func commander(platform *fakeCommands, executor *ran) *Commander {
	return &Commander{
		Platform: platform,
		Operator: &operate.Operator{Allowed: map[string][]string{"service.restart": {"nginx"}}, Exec: executor, Now: func() time.Time { return time.Date(2026, 10, 1, 10, 0, 0, 0, time.UTC) }},
		Log:      log.New(io.Discard, "", 0),
		Wait:     func(context.Context, time.Duration) {},
	}
}

func handed(id, service string) protocol.Commands {
	return protocol.Commands{PollSeconds: 30, Commands: []protocol.WireCommand{{CommandID: id, Operation: "service.restart", Parameters: map[string]string{"service": service}, ExpiresAt: "2026-10-01T10:15:00Z"}}}
}

func TestACommandIsDoneAndAnsweredOnce(t *testing.T) {
	platform := &fakeCommands{polls: []protocol.Commands{handed("cmd_1", "nginx"), handed("cmd_1", "nginx")}}
	executor := &ran{}
	c := commander(platform, executor)

	for range 2 {
		if err := c.Poll(context.Background()); err != nil {
			t.Fatal(err)
		}
	}

	if len(platform.results) != 1 || platform.results[0].Status != "succeeded" || platform.ids[0] != "cmd_1" {
		t.Fatalf("answers %+v for %v: a command handed over twice runs once", platform.results, platform.ids)
	}

	if len(executor.calls) != 2 {
		t.Fatalf("ran %v", executor.calls)
	}

	if c.interval != 30*time.Second {
		t.Fatalf("the platform's poll interval is followed: %s", c.interval)
	}
}

func TestACommandThatIsNotAllowedIsAnsweredAsFailedAndNotRun(t *testing.T) {
	platform := &fakeCommands{polls: []protocol.Commands{handed("cmd_2", "sshd")}}
	executor := &ran{}

	if err := commander(platform, executor).Poll(context.Background()); err != nil {
		t.Fatal(err)
	}

	if len(executor.calls) != 0 || len(platform.results) != 1 || platform.results[0].Status != "failed" {
		t.Fatalf("ran %v, answered %+v", executor.calls, platform.results)
	}
}

func TestTheAnswerIsRetriedThenLetGo(t *testing.T) {
	unreachable := errors.New("connection refused")
	platform := &fakeCommands{polls: []protocol.Commands{handed("cmd_3", "nginx")}, resultErrs: []error{unreachable, unreachable, nil}}

	if err := commander(platform, &ran{}).Poll(context.Background()); err != nil {
		t.Fatal(err)
	}

	if len(platform.results) != 3 {
		t.Fatalf("tried %d times", len(platform.results))
	}

	gone := &fakeCommands{polls: []protocol.Commands{handed("cmd_4", "nginx")}, resultErrs: []error{unreachable, unreachable, unreachable, unreachable, unreachable}}
	_ = commander(gone, &ran{}).Poll(context.Background())

	if len(gone.results) != len(resultWaits) {
		t.Fatalf("tried %d times, want %d", len(gone.results), len(resultWaits))
	}
}

func TestARefusedCredentialStopsItAndAnOldPlatformIsLeftAlone(t *testing.T) {
	revoked := &fakeCommands{pollErr: &protocol.Error{Status: http.StatusUnauthorized}}
	if err := commander(revoked, &ran{}).Poll(context.Background()); !errors.Is(err, ErrRevoked) {
		t.Fatalf("%v", err)
	}

	old := &fakeCommands{pollErr: &protocol.Error{Status: http.StatusNotFound}}
	c := commander(old, &ran{})

	if err := c.Poll(context.Background()); err != nil || c.interval != oldPlatformWait {
		t.Fatalf("err=%v interval=%s", err, c.interval)
	}

	down := &fakeCommands{pollErr: errors.New("no route")}
	c = commander(down, &ran{})

	if err := c.Poll(context.Background()); err == nil || c.interval <= defaultPoll-1 {
		t.Fatalf("err=%v interval=%s", err, c.interval)
	}
}

func TestAnAgentThatMayDoNothingDoesNotPoll(t *testing.T) {
	platform := &fakeCommands{pollErr: errors.New("must not be asked")}
	c := commander(platform, &ran{})
	c.Operator.Allowed = nil

	if err := c.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestAdvertisedCapabilitiesAreInThePlatformsOrder(t *testing.T) {
	got := Advertise([]string{"package.status", "service.restart", "shell.run"})
	want := []string{"metrics.read", "service.status", "inventory.read", "service.restart", "package.status"}

	if len(got) != len(want) {
		t.Fatalf("%v", got)
	}

	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("%v", got)
		}
	}
}
