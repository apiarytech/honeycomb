/*
 * Copyright (C) 2026 Franklin D. Amador
 *
 * This software is dual-licensed under the terms of the GPL v3.0 and
 * a commercial license. You may choose to use this software under either
 * license.
 *
 * See the LICENSE files in the project root for full license text.
 */

package forward

import (
	"context"
	"math/rand/v2"
	"sync"
	"time"
)

// Sink is a destination of a spool's records, such as a remote database.
type Sink interface {
	// Name identifies the sink in the spool; it must not change between
	// runs, or the sink receives the spool's records again.
	Name() string
	// Send stores a batch: all of it, or none of it and an error. A batch
	// may come again after a crash or a lost reply, so storing it twice
	// must leave the same result as storing it once.
	Send(ctx context.Context, b Batch) error
}

// Forwarder defaults.
const (
	DefaultBatchSize  = 500
	DefaultMinBackoff = time.Second
	DefaultMaxBackoff = time.Minute
	// DefaultSendTimeout bounds one Send: a link that drops without a
	// reset (a pulled cable) would otherwise hold a connection for as long
	// as TCP waits.
	DefaultSendTimeout = 30 * time.Second
	DefaultLinger      = 50 * time.Millisecond
	// idlePoll is how often an idle forwarder looks at the spool without
	// being woken, so records appended by another process are sent too.
	idlePoll = 5 * time.Second
)

// ForwarderOptions configures a Forwarder.
type ForwarderOptions struct {
	BatchSize  int           // records per Send; DefaultBatchSize when 0
	MinBackoff time.Duration // the first wait after a failure; DefaultMinBackoff when 0
	MaxBackoff time.Duration // the longest wait; DefaultMaxBackoff when 0
	// SendTimeout bounds each Send (DefaultSendTimeout when 0; negative:
	// no bound beyond Run's context).
	SendTimeout time.Duration
	// Linger: woken from idle by an append, or after a Send that was not
	// full, the forwarder waits this long before reading, so appends that
	// come one at a time go in one Send (DefaultLinger when 0; negative: no
	// wait). A backlog is sent without waiting.
	Linger  time.Duration
	OnError func(error) // each failed Send or spool error
}

// SinkStatus is a forwarder's state.
type SinkStatus struct {
	Sink string
	// Online: the last Send succeeded, or none has been needed yet.
	Online      bool
	LastError   string
	LastErrorAt time.Time
	LastSent    time.Time // when a Send last succeeded
	Sent        uint64    // records stored since the forwarder started
	Failures    uint64    // failed Sends in a row
}

// Forwarder sends a spool's records to one sink, in order, and confirms
// each batch once the sink has stored it. While the sink fails it retries
// the same batch, waiting longer each time (with jitter) up to MaxBackoff;
// once it succeeds it sends the backlog at full speed.
type Forwarder struct {
	spool *Spool
	sink  Sink
	opts  ForwarderOptions

	mu     sync.Mutex
	status SinkStatus
}

// NewForwarder returns a forwarder from spool to sink. The spool must list
// sink's name in SpoolOptions.Sinks.
func NewForwarder(spool *Spool, sink Sink, opts ForwarderOptions) *Forwarder {
	if opts.BatchSize <= 0 {
		opts.BatchSize = DefaultBatchSize
	}
	if opts.MinBackoff <= 0 {
		opts.MinBackoff = DefaultMinBackoff
	}
	if opts.MaxBackoff < opts.MinBackoff {
		opts.MaxBackoff = max(DefaultMaxBackoff, opts.MinBackoff)
	}
	if opts.SendTimeout == 0 {
		opts.SendTimeout = DefaultSendTimeout
	}
	if opts.Linger == 0 {
		opts.Linger = DefaultLinger
	}
	if opts.OnError == nil {
		opts.OnError = func(error) {}
	}
	return &Forwarder{spool: spool, sink: sink, opts: opts, status: SinkStatus{Sink: sink.Name(), Online: true}}
}

// Status returns the forwarder's state.
func (f *Forwarder) Status() SinkStatus {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.status
}

// Run forwards until ctx ends.
func (f *Forwarder) Run(ctx context.Context) {
	f.run(ctx, false)
}

// Drain forwards until the sink has every record or ctx ends, and reports
// whether it has them all.
func (f *Forwarder) Drain(ctx context.Context) bool {
	return f.run(ctx, true)
}

func (f *Forwarder) run(ctx context.Context, drain bool) bool {
	backoff := f.opts.MinBackoff
	for ctx.Err() == nil {
		wake := f.spool.Wake() // before Read, so an append after it is not missed
		b, err := f.spool.Read(ctx, f.sink.Name(), f.opts.BatchSize)
		if err == nil && b.Empty() {
			if drain {
				return true
			}
			select {
			case <-ctx.Done():
			case <-wake:
				if f.opts.Linger > 0 {
					select {
					case <-ctx.Done():
					case <-time.After(f.opts.Linger):
					}
				}
			case <-time.After(idlePoll):
			}
			continue
		}
		if err == nil {
			if err = f.send(ctx, b); err == nil {
				err = f.spool.Ack(ctx, f.sink.Name(), b.Through)
				f.sent(len(b.Records))
				if err == nil {
					backoff = f.opts.MinBackoff
					// A batch that was not full: appends are trickling in;
					// let a few gather before the next Send.
					if !drain && len(b.Records) < f.opts.BatchSize && f.opts.Linger > 0 {
						select {
						case <-ctx.Done():
						case <-time.After(f.opts.Linger):
						}
					}
					continue
				}
			} else {
				f.failed(err)
			}
		}
		if ctx.Err() != nil {
			break
		}
		f.opts.OnError(err)
		// Jitter keeps many units from retrying a recovered server at once.
		wait := backoff/2 + rand.N(backoff/2+1)
		select {
		case <-ctx.Done():
		case <-time.After(wait):
		}
		backoff = min(backoff*2, f.opts.MaxBackoff)
	}
	return false
}

// send is one Send, bounded by SendTimeout.
func (f *Forwarder) send(ctx context.Context, b Batch) error {
	if f.opts.SendTimeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, f.opts.SendTimeout)
		defer cancel()
	}
	return f.sink.Send(ctx, b)
}

func (f *Forwarder) sent(n int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.status.Online, f.status.Failures, f.status.LastSent = true, 0, time.Now()
	f.status.Sent += uint64(n)
}

func (f *Forwarder) failed(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.status.Online = false
	f.status.Failures++
	f.status.LastError, f.status.LastErrorAt = err.Error(), time.Now()
}
