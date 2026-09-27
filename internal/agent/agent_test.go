package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"testing"
	"time"

	"github.com/laikait/lip-agent/internal/collect"
	"github.com/laikait/lip-agent/internal/protocol"
)

func sample(n int) collect.Sample {
	return collect.Sample{SampledAt: fmt.Sprintf("2026-09-27T10:%02d:00Z", n%60), CPU: float64(n)}
}

func TestABatchKeepsItsIdUntilItIsAcknowledged(t *testing.T) {
	outbox := NewOutbox(0)

	for n := 0; n < 70; n++ {
		outbox.Add(sample(n))
	}

	id, samples := outbox.Next()
	if len(samples) != BatchMax || len(id) != 32 {
		t.Fatalf("%d samples under %q", len(samples), id)
	}

	outbox.Add(sample(70))

	again, samplesAgain := outbox.Next()
	if again != id || len(samplesAgain) != BatchMax || samplesAgain[0] != samples[0] {
		t.Fatal("a retry is the same batch, whatever arrived since")
	}

	outbox.Done()

	next, rest := outbox.Next()
	if next == id || len(rest) != 11 || rest[0].CPU != 60 {
		t.Fatalf("then the rest, under a new id: %d, first %v", len(rest), rest[0].CPU)
	}
}

func TestOverflowDropsTheOldestButNeverTheBatchInFlight(t *testing.T) {
	outbox := NewOutbox(5)

	for n := 0; n < 3; n++ {
		outbox.Add(sample(n))
	}

	_, inflight := outbox.Next()

	for n := 3; n < 9; n++ {
		outbox.Add(sample(n))
	}

	_, still := outbox.Next()
	if len(still) != len(inflight) || still[0].CPU != 0 {
		t.Fatal("the batch in flight changed")
	}

	if outbox.Len() != 5 || outbox.Dropped() != 4 {
		t.Fatalf("len %d", outbox.Len())
	}

	outbox.Done()

	_, rest := outbox.Next()
	if len(rest) != 2 || rest[0].CPU != 7 {
		t.Fatalf("the newest kept: %v", rest)
	}
}

type fakePlatform struct {
	batches   []protocol.Batch
	errs      []error
	heartbeat int
	answer    protocol.Answer
}

func (f *fakePlatform) Heartbeat(context.Context, protocol.Heartbeat) (protocol.Answer, error) {
	f.heartbeat++

	return f.answer, nil
}

func (f *fakePlatform) Metrics(_ context.Context, batch protocol.Batch) (protocol.Answer, error) {
	f.batches = append(f.batches, batch)

	if len(f.errs) > 0 {
		err := f.errs[0]
		f.errs = f.errs[1:]

		return protocol.Answer{}, err
	}

	return f.answer, nil
}

type fakeMachine struct{ n int }

func (m *fakeMachine) Sample(time.Time) (collect.Sample, bool, error) {
	m.n++

	return sample(m.n), true, nil
}

func (m *fakeMachine) Host() collect.Host {
	return collect.Host{Hostname: "web-1"}
}

func runner(platform *fakePlatform, now *time.Time) *Runner {
	return &Runner{
		Platform: platform,
		Machine:  &fakeMachine{},
		Version:  "test",
		Log:      log.New(io.Discard, "", 0),
		Now:      func() time.Time { return *now },
	}
}

func TestALostReplyIsSentAgainUnderTheSameIdAfterAWait(t *testing.T) {
	now := time.Date(2026, 9, 27, 10, 0, 0, 0, time.UTC)
	platform := &fakePlatform{errs: []error{errors.New("connection reset")}}
	r := runner(platform, &now)

	_ = r.Tick(context.Background())
	if len(platform.batches) != 1 {
		t.Fatal("sent")
	}

	// Within the wait: nothing is sent, and samples queue.
	now = now.Add(20 * time.Second)
	_ = r.Tick(context.Background())

	if len(platform.batches) != 1 {
		t.Fatal("sent during the wait")
	}

	now = now.Add(20 * time.Second)
	if err := r.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}

	// The retry is the same batch, and the samples taken meanwhile follow
	// in a new one.
	if len(platform.batches) != 3 || platform.batches[1].BatchID != platform.batches[0].BatchID || len(platform.batches[1].Samples) != 1 {
		t.Fatalf("%d batches", len(platform.batches))
	}

	if platform.batches[2].BatchID == platform.batches[1].BatchID || len(platform.batches[2].Samples) != 2 || r.outbox.Len() != 0 {
		t.Fatalf("then the rest: %d samples, %d waiting", len(platform.batches[2].Samples), r.outbox.Len())
	}

	if platform.batches[1].Host == nil || platform.batches[1].Host.Hostname != "web-1" {
		t.Fatal("each batch says what the host is")
	}
}

func TestARefusedBatchIsDroppedAndARevokedCredentialStops(t *testing.T) {
	now := time.Date(2026, 9, 27, 10, 0, 0, 0, time.UTC)
	platform := &fakePlatform{errs: []error{&protocol.Error{Status: 422}}}
	r := runner(platform, &now)

	if err := r.Tick(context.Background()); err != nil || r.outbox.Len() != 0 {
		t.Fatalf("%v, %d waiting", err, r.outbox.Len())
	}

	platform.errs = []error{&protocol.Error{Status: 401}}
	now = now.Add(time.Minute)

	if err := r.Tick(context.Background()); !errors.Is(err, ErrRevoked) {
		t.Fatalf("%v", err)
	}
}

func TestThePlatformSetsTheIntervalsWithinReason(t *testing.T) {
	now := time.Now()
	platform := &fakePlatform{answer: protocol.Answer{MetricsSeconds: 1, HeartbeatSeconds: 120}}
	r := runner(platform, &now)

	_ = r.Tick(context.Background())

	if r.interval != 15*time.Second || r.heartbeat != 2*time.Minute {
		t.Fatalf("%s %s", r.interval, r.heartbeat)
	}
}
