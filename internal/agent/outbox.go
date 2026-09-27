// Package agent is the agent's loop: sample every minute, send what has not
// been received, say it is alive, and wait for the platform when it cannot
// be reached.
package agent

import (
	"crypto/rand"
	"encoding/hex"

	"github.com/laikait/lip-agent/internal/collect"
)

const (
	// BatchMax is the most samples the protocol takes in one batch.
	BatchMax = 60

	// BufferMax is the most samples kept while the platform cannot be
	// reached: six hours at one a minute. Older ones are dropped first.
	BufferMax = 360
)

// Outbox holds samples until the platform has them.
//
// **A batch keeps its id until it is acknowledged.** If the reply to a batch
// is lost, the same samples go again under the same id, and the platform
// answers "duplicate" instead of keeping them twice. So the batch in flight
// is fixed: new samples queue behind it, and overflow drops the oldest
// sample that is not in it.
type Outbox struct {
	samples  []collect.Sample
	inflight *batch
	max      int
	dropped  int
}

type batch struct {
	id    string
	count int
}

// NewOutbox holds at most max samples (BufferMax when 0).
func NewOutbox(max int) *Outbox {
	if max <= 0 {
		max = BufferMax
	}

	return &Outbox{max: max}
}

// Add queues a sample.
func (o *Outbox) Add(sample collect.Sample) {
	o.samples = append(o.samples, sample)

	for len(o.samples) > o.max {
		keep := 0
		if o.inflight != nil {
			keep = o.inflight.count
		}

		if keep >= len(o.samples) {
			break
		}

		o.samples = append(o.samples[:keep], o.samples[keep+1:]...)
		o.dropped++
	}
}

// Next is the batch to send: the one in flight, or a new one of the oldest
// samples. Empty when there is nothing to send.
func (o *Outbox) Next() (string, []collect.Sample) {
	if o.inflight == nil {
		if len(o.samples) == 0 {
			return "", nil
		}

		count := len(o.samples)
		if count > BatchMax {
			count = BatchMax
		}

		o.inflight = &batch{id: NewBatchID(), count: count}
	}

	return o.inflight.id, append([]collect.Sample(nil), o.samples[:o.inflight.count]...)
}

// Done forgets the batch in flight: the platform has it, or refused it for
// good.
func (o *Outbox) Done() {
	if o.inflight == nil {
		return
	}

	o.samples = append([]collect.Sample(nil), o.samples[o.inflight.count:]...)
	o.inflight = nil
}

// Len is how many samples are waiting, in flight included.
func (o *Outbox) Len() int {
	return len(o.samples)
}

// Dropped is how many samples overflow has cost, and resets the count.
func (o *Outbox) Dropped() int {
	dropped := o.dropped
	o.dropped = 0

	return dropped
}

// NewBatchID is 16 random bytes in hex: unique without coordination.
func NewBatchID() string {
	bytes := make([]byte, 16)
	if _, err := rand.Read(bytes); err != nil {
		panic("no randomness: " + err.Error())
	}

	return hex.EncodeToString(bytes)
}
