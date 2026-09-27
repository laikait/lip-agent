package agent

import (
	"context"
	"errors"
	"log"
	"time"

	"github.com/laikait/lip-agent/internal/collect"
	"github.com/laikait/lip-agent/internal/protocol"
)

// Capabilities are the named operations this agent advertises. No shell,
// and no commands until Phase 4 (plans/agent.md).
var Capabilities = []string{"metrics.read", "service.status"}

// ErrRevoked means the platform no longer accepts the credential: the
// server was removed, or the agent's record went. Only enrolling again
// helps, so the agent stops rather than knocking every minute.
var ErrRevoked = errors.New("the platform no longer accepts this agent's credential: enrol again")

// Platform is the part of the protocol client the loop uses.
type Platform interface {
	Heartbeat(ctx context.Context, heartbeat protocol.Heartbeat) (protocol.Answer, error)
	Metrics(ctx context.Context, batch protocol.Batch) (protocol.Answer, error)
}

// Machine is what the loop reads.
type Machine interface {
	Sample(now time.Time) (collect.Sample, bool, error)
	Host() collect.Host
}

// Runner is the loop.
type Runner struct {
	Platform Platform
	Machine  Machine
	Services func(ctx context.Context) ([]collect.Service, error)
	Version  string
	Log      *log.Logger
	Now      func() time.Time

	outbox        *Outbox
	interval      time.Duration
	heartbeat     time.Duration
	lastHeartbeat time.Time
	retryAt       time.Time
	backoff       time.Duration
	servicesErr   string
}

const (
	defaultInterval = 60 * time.Second
	minInterval     = 15 * time.Second
	maxInterval     = time.Hour
	maxBackoff      = 10 * time.Minute
)

func (r *Runner) init() {
	if r.outbox == nil {
		r.outbox = NewOutbox(BufferMax)
	}

	if r.interval == 0 {
		r.interval = defaultInterval
	}

	if r.heartbeat == 0 {
		r.heartbeat = defaultInterval
	}

	if r.Now == nil {
		r.Now = time.Now
	}

	if r.Log == nil {
		r.Log = log.Default()
	}
}

// Run samples and reports until ctx is done (nil), or the credential is
// refused (ErrRevoked).
func (r *Runner) Run(ctx context.Context) error {
	r.init()

	// The first reading is the baseline for CPU and network.
	if _, _, err := r.Machine.Sample(r.Now()); err != nil {
		r.Log.Printf("cannot read this machine: %v", err)
	}

	r.Log.Printf("running: a sample every %s", r.interval)

	if err := r.Heartbeat(ctx); errors.Is(err, ErrRevoked) {
		return err
	}

	timer := time.NewTimer(r.interval)
	defer timer.Stop()

	for {
		select {
		case <-ctx.Done():
			// One last try with what is waiting, briefly.
			final, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			r.retryAt = time.Time{}
			_ = r.Flush(final)
			cancel()

			return nil
		case <-timer.C:
		}

		if err := r.Tick(ctx); errors.Is(err, ErrRevoked) {
			return err
		}

		timer.Reset(r.interval)
	}
}

// Tick is one round: a sample, the heartbeat if one is due, and whatever is
// waiting sent.
func (r *Runner) Tick(ctx context.Context) error {
	r.init()
	now := r.Now()

	sample, ok, err := r.Machine.Sample(now)
	if err != nil {
		r.Log.Printf("cannot read this machine: %v", err)
	} else if ok {
		r.outbox.Add(sample)
	}

	if dropped := r.outbox.Dropped(); dropped > 0 {
		r.Log.Printf("the platform has been unreachable too long: %d oldest samples dropped", dropped)
	}

	if now.Sub(r.lastHeartbeat) >= r.heartbeat {
		if err := r.Heartbeat(ctx); errors.Is(err, ErrRevoked) {
			return err
		}
	}

	return r.Flush(ctx)
}

// Heartbeat says the agent is alive, and what it is.
func (r *Runner) Heartbeat(ctx context.Context) error {
	r.init()

	if r.now().Before(r.retryAt) {
		return nil
	}

	answer, err := r.Platform.Heartbeat(ctx, protocol.Heartbeat{Version: r.Version, Capabilities: Capabilities})

	return r.after(answer, err, "heartbeat", func() { r.lastHeartbeat = r.now() })
}

// Flush sends waiting samples, a batch at a time, until none are left or the
// platform cannot take them now.
func (r *Runner) Flush(ctx context.Context) error {
	r.init()

	for r.outbox.Len() > 0 && !r.now().Before(r.retryAt) {
		id, samples := r.outbox.Next()
		host := r.Machine.Host()

		answer, err := r.Platform.Metrics(ctx, protocol.Batch{BatchID: id, Host: &host, Samples: samples, Services: r.services(ctx)})

		if protocol.Rejected(err) {
			// Sending it again would be refused again: it is dropped, said
			// once, and the next batch goes.
			r.Log.Printf("the platform refused a batch of %d samples, which are dropped: %v", len(samples), err)
			r.outbox.Done()

			continue
		}

		if err := r.after(answer, err, "metrics", r.outbox.Done); err != nil {
			return err
		}

		// Receiving metrics is being heard from.
		r.lastHeartbeat = r.now()
	}

	return nil
}

// after handles a reply: the credential refused, the platform unreachable
// (wait, longer each time), or an answer (and its intervals).
func (r *Runner) after(answer protocol.Answer, err error, call string, done func()) error {
	if protocol.Unauthorized(err) {
		r.Log.Print(ErrRevoked)

		return ErrRevoked
	}

	if err != nil {
		if r.backoff == 0 {
			r.backoff = 30 * time.Second
		} else if r.backoff < maxBackoff {
			r.backoff *= 2
		}

		if r.backoff > maxBackoff {
			r.backoff = maxBackoff
		}

		r.retryAt = r.now().Add(r.backoff)
		r.Log.Printf("%s: %v; trying again in %s (%d samples waiting)", call, err, r.backoff, r.outbox.Len())

		return err
	}

	if r.backoff > 0 {
		r.Log.Printf("the platform is reachable again")
	}

	r.backoff = 0
	r.retryAt = time.Time{}
	done()
	r.intervals(answer)

	return nil
}

// intervals follows what the platform asks, within reason.
func (r *Runner) intervals(answer protocol.Answer) {
	if answer.MetricsSeconds > 0 {
		r.interval = bounded(time.Duration(answer.MetricsSeconds) * time.Second)
	}

	if answer.HeartbeatSeconds > 0 {
		r.heartbeat = bounded(time.Duration(answer.HeartbeatSeconds) * time.Second)
	}
}

func (r *Runner) services(ctx context.Context) []collect.Service {
	if r.Services == nil {
		return nil
	}

	services, err := r.Services(ctx)

	// Said once per kind of failure, not every minute.
	if err != nil && err.Error() != r.servicesErr {
		r.Log.Printf("services are not reported: %v", err)
		r.servicesErr = err.Error()
	}

	if err == nil {
		r.servicesErr = ""
	}

	return services
}

func (r *Runner) now() time.Time {
	return r.Now()
}

func bounded(interval time.Duration) time.Duration {
	switch {
	case interval < minInterval:
		return minInterval
	case interval > maxInterval:
		return maxInterval
	default:
		return interval
	}
}
