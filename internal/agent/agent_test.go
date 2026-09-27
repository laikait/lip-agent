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
	batches     []protocol.Batch
	errs        []error
	heartbeat   int
	answer      protocol.Answer
	inventories []protocol.Inventory
	invErrs     []error
}

func (f *fakePlatform) Inventory(_ context.Context, inventory protocol.Inventory) (protocol.Answer, error) {
	f.inventories = append(f.inventories, inventory)

	if len(f.invErrs) > 0 {
		err := f.invErrs[0]
		f.invErrs = f.invErrs[1:]

		return protocol.Answer{}, err
	}

	return f.answer, nil
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

// machineInventory is what a machine's inventory reads as, and how often.
type machineInventory struct {
	packages []collect.Package
	reads    int
}

func (m *machineInventory) read(context.Context) (protocol.Inventory, error) {
	m.reads++

	return protocol.Inventory{Packages: append([]collect.Package(nil), m.packages...)}, nil
}

func TestAnInventoryIsSentWhenItChangesOrADayHasPassed(t *testing.T) {
	now := time.Date(2026, 9, 27, 10, 0, 0, 0, time.UTC)
	platform := &fakePlatform{}
	machine := &machineInventory{packages: []collect.Package{{Name: "nginx", Version: "1.22", Manager: "dpkg"}}}
	r := runner(platform, &now)
	r.Inventory = machine.read

	if err := r.SendInventory(context.Background()); err != nil || len(platform.inventories) != 1 {
		t.Fatalf("the first is sent at once: %d %v", len(platform.inventories), err)
	}

	// Not time to look yet.
	now = now.Add(30 * time.Minute)
	_ = r.SendInventory(context.Background())

	if machine.reads != 1 {
		t.Fatalf("looked again after half an hour: %d reads", machine.reads)
	}

	// Time to look, and nothing changed: not sent.
	now = now.Add(time.Hour)
	_ = r.SendInventory(context.Background())

	if machine.reads != 2 || len(platform.inventories) != 1 {
		t.Fatalf("an unchanged inventory was sent: %d reads, %d sent", machine.reads, len(platform.inventories))
	}

	// Changed: sent, under a new id.
	machine.packages[0].Version = "1.24"
	now = now.Add(time.Hour)
	_ = r.SendInventory(context.Background())

	if len(platform.inventories) != 2 || platform.inventories[1].InventoryID == platform.inventories[0].InventoryID {
		t.Fatalf("a changed inventory goes under a new id: %+v", platform.inventories)
	}

	// Unchanged for a day: sent again, so the platform knows it is current.
	now = now.Add(25 * time.Hour)
	_ = r.SendInventory(context.Background())

	if len(platform.inventories) != 3 {
		t.Fatalf("a day-old inventory is sent again: %d sent", len(platform.inventories))
	}
}

func TestALostInventoryReplyIsRetriedUnderTheSameIdAndARefusalWaitsADay(t *testing.T) {
	now := time.Date(2026, 9, 27, 10, 0, 0, 0, time.UTC)
	platform := &fakePlatform{invErrs: []error{errors.New("connection reset")}}
	machine := &machineInventory{packages: []collect.Package{{Name: "nginx", Version: "1.22", Manager: "dpkg"}}}
	r := runner(platform, &now)
	r.Inventory = machine.read

	_ = r.SendInventory(context.Background())

	// After the wait, the same inventory, the same id, without reading the machine again.
	now = now.Add(time.Minute)
	_ = r.SendInventory(context.Background())

	if len(platform.inventories) != 2 || platform.inventories[0].InventoryID != platform.inventories[1].InventoryID || machine.reads != 1 {
		t.Fatalf("a retry keeps its id: %+v, %d reads", platform.inventories, machine.reads)
	}

	// A platform older than the call: 404, not asked again until the machine changes or a day passes.
	older := &fakePlatform{invErrs: []error{&protocol.Error{Status: 404}}}
	r = runner(older, &now)
	r.Inventory = machine.read

	_ = r.SendInventory(context.Background())
	now = now.Add(2 * time.Hour)
	_ = r.SendInventory(context.Background())

	if len(older.inventories) != 1 {
		t.Fatalf("a refused inventory was sent again within the day: %d", len(older.inventories))
	}
}

func TestTheAgentAdvertisesInventory(t *testing.T) {
	want := []string{"metrics.read", "service.status", "inventory.read"}

	if fmt.Sprint(Capabilities) != fmt.Sprint(want) {
		t.Fatalf("%v, want %v in the platform's order", Capabilities, want)
	}
}
