/*
 * Copyright (C) 2026 Franklin D. Amador
 *
 * This software is dual-licensed under the terms of the GPL v3.0 and
 * a commercial license. You may choose to use this software under either
 * license.
 *
 * See the LICENSE files in the project root for full license text.
 */

// Package forward is durable store-and-forward of a TagDatabase's changes
// to databases that may be unreachable for a while.
//
// A Collector follows the change feed and appends every change (or every
// change beyond a Deadband) to a Spool, a SQLite file on the node's own
// disk, in the same transaction as its position. A Forwarder per Sink sends
// the spool's records in order and confirms each batch once the sink has
// stored it. The collector only ever writes to local disk, so it keeps up
// with the feed however long the network is down; when the sink comes back
// the forwarder sends the backlog in order. A full spool drops its oldest
// records and every sink that had not received them is told how many.
//
//	TagDatabase ──Changes──► Collector ──► Spool (SQLite) ──► Forwarder ──► Sink (SQLSink, ...)
//
// Open puts the three together and publishes their health as tags.
package forward

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/apiarytech/honeycomb"
	plc "github.com/apiarytech/royaljelly/iec"
)

// DefaultStatusPrefix starts the names of the status tags.
const DefaultStatusPrefix = "SF_"

// Status tag names, after the prefix.
const (
	StatusBacklog   = "Backlog"    // LINT: records the slowest sink has not stored
	StatusOldestAge = "OldestAgeS" // LREAL: age in seconds of the oldest of them
	StatusOnline    = "Online"     // BOOL: every sink's last send succeeded
	StatusDropped   = "Dropped"    // LINT: records dropped from a full spool, ever
	StatusLastError = "LastError"  // STRING: the last error, empty when none
)

// Options configures Open.
type Options struct {
	// Source, Reader, List, Names, Exclude and Deadbands are the
	// collector's (CollectorOptions).
	Source    honeycomb.ChangeSource
	Reader    honeycomb.TagReader
	List      func(ctx context.Context) ([]string, error)
	Names     []string
	Exclude   []string
	Deadbands []Deadband
	// SpoolPath is the spool's SQLite file, on the node's own disk.
	SpoolPath string
	// Sinks are the destinations; each receives every record.
	Sinks []Sink
	// MaxBytes, MaxRows and MaxAge limit the spool (SpoolOptions).
	MaxBytes int64
	MaxRows  int64
	MaxAge   time.Duration
	// Status, when set, gets the status tags (prefixed StatusPrefix),
	// updated every StatusEvery (one second when 0). The collector never
	// collects them.
	Status       *honeycomb.TagDatabase
	StatusPrefix string
	StatusEvery  time.Duration
	// Forwarder is each forwarder's options; its OnError is OnError.
	Forwarder ForwarderOptions
	OnError   func(error)
}

// Service is a running collector, spool and forwarders.
type Service struct {
	spool      *Spool
	collector  *Collector
	forwarders []*Forwarder
	opts       Options

	collectStop, forwardStop context.CancelFunc
	collectDone, forwardDone chan struct{}

	mu      sync.Mutex
	lastErr string
	closed  bool
}

// Open opens the spool and starts collecting and forwarding.
func Open(ctx context.Context, opts Options) (*Service, error) {
	if len(opts.Sinks) == 0 {
		return nil, errors.New("forward: no sinks")
	}
	if opts.SpoolPath == "" {
		return nil, errors.New("forward: no spool path")
	}
	if opts.StatusPrefix == "" {
		opts.StatusPrefix = DefaultStatusPrefix
	}
	if opts.StatusEvery <= 0 {
		opts.StatusEvery = time.Second
	}
	s := &Service{opts: opts}
	report := opts.OnError
	opts.OnError = func(err error) {
		s.mu.Lock()
		s.lastErr = err.Error()
		s.mu.Unlock()
		if report != nil {
			report(err)
		}
	}
	names := make([]string, len(opts.Sinks))
	for i, sink := range opts.Sinks {
		names[i] = sink.Name()
	}
	spool, err := OpenSpool(ctx, opts.SpoolPath, SpoolOptions{
		Sinks: names, MaxBytes: opts.MaxBytes, MaxRows: opts.MaxRows, MaxAge: opts.MaxAge,
		OnDrop: func(n int64, from, to time.Time) {
			opts.OnError(fmt.Errorf("forward: spool full: %d records from %s to %s dropped unsent",
				n, from.Format(time.RFC3339), to.Format(time.RFC3339)))
		},
	})
	if err != nil {
		return nil, err
	}
	exclude := slices.Clone(opts.Exclude)
	if opts.Status != nil {
		exclude = append(exclude, opts.StatusPrefix+"*")
	}
	collector, err := NewCollector(spool, CollectorOptions{
		Source: opts.Source, Reader: opts.Reader, List: opts.List, Names: opts.Names,
		Exclude: exclude, Deadbands: opts.Deadbands, OnError: opts.OnError,
	})
	if err != nil {
		spool.Close()
		return nil, err
	}
	s.spool, s.collector, s.opts = spool, collector, opts
	if opts.Status != nil {
		if err := s.addStatusTags(); err != nil {
			spool.Close()
			return nil, err
		}
	}
	fopts := opts.Forwarder
	fopts.OnError = opts.OnError
	for _, sink := range opts.Sinks {
		s.forwarders = append(s.forwarders, NewForwarder(spool, sink, fopts))
	}

	cctx, cstop := context.WithCancel(context.Background())
	fctx, fstop := context.WithCancel(context.Background())
	s.collectStop, s.forwardStop = cstop, fstop
	s.collectDone, s.forwardDone = make(chan struct{}), make(chan struct{})
	go func() {
		defer close(s.collectDone)
		collector.Run(cctx)
	}()
	go func() {
		defer close(s.forwardDone)
		var wg sync.WaitGroup
		for _, f := range s.forwarders {
			wg.Go(func() { f.Run(fctx) })
		}
		if opts.Status != nil {
			wg.Go(func() { s.publish(fctx) })
		}
		wg.Wait()
	}()
	return s, nil
}

// Spool returns the service's spool.
func (s *Service) Spool() *Spool { return s.spool }

// Status is the state of a Service.
type Status struct {
	Spool     SpoolStats
	Sinks     []SinkStatus
	Backlog   int64     // records the slowest sink has not stored
	Oldest    time.Time // time of the oldest of them; zero when none
	Online    bool      // every sink's last send succeeded
	LastError string
}

// Status reports the spool and each sink.
func (s *Service) Status(ctx context.Context) (Status, error) {
	st := Status{Online: true}
	var err error
	if st.Spool, err = s.spool.Stats(ctx); err != nil {
		return st, err
	}
	for _, f := range s.forwarders {
		fs := f.Status()
		st.Sinks = append(st.Sinks, fs)
		st.Online = st.Online && fs.Online
		st.Backlog = max(st.Backlog, st.Spool.Pending[fs.Sink])
		if t := st.Spool.OldestPending[fs.Sink]; !t.IsZero() && (st.Oldest.IsZero() || t.Before(st.Oldest)) {
			st.Oldest = t
		}
	}
	s.mu.Lock()
	st.LastError = s.lastErr
	s.mu.Unlock()
	return st, nil
}

// Close stops collecting, gives the forwarders until ctx ends to send
// what is left, then stops them and closes the spool. What is not sent
// stays in the spool for the next start.
func (s *Service) Close(ctx context.Context) error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	s.mu.Unlock()
	s.collectStop()
	<-s.collectDone
	var wg sync.WaitGroup
	for _, f := range s.forwarders {
		if f.Status().Online { // an offline sink would only wait out ctx
			wg.Go(func() { f.Drain(ctx) })
		}
	}
	wg.Wait()
	s.forwardStop()
	<-s.forwardDone
	return s.spool.Close()
}

// addStatusTags creates the status tags that do not exist yet.
func (s *Service) addStatusTags() error {
	for _, t := range []struct {
		name  string
		value any
	}{
		{StatusBacklog, plc.LINT(0)},
		{StatusOldestAge, plc.LREAL(0)},
		{StatusOnline, plc.BOOL(true)},
		{StatusDropped, plc.LINT(0)},
		{StatusLastError, plc.STRING("")},
	} {
		name := s.opts.StatusPrefix + t.name
		if _, err := s.opts.Status.ReadTag(name); err == nil {
			continue
		}
		err := s.opts.Status.AddTag(&honeycomb.Tag{Name: name, Value: t.value,
			Description: "store-and-forward status (honeycomb/store/forward)"})
		if err != nil {
			return fmt.Errorf("forward: status tag %s: %w", name, err)
		}
	}
	return nil
}

// publish updates the status tags until ctx ends.
func (s *Service) publish(ctx context.Context) {
	tick := time.NewTicker(s.opts.StatusEvery)
	defer tick.Stop()
	for {
		st, err := s.Status(ctx)
		if err == nil {
			age := 0.0
			if !st.Oldest.IsZero() {
				age = time.Since(st.Oldest).Seconds()
			}
			p := s.opts.StatusPrefix
			db := s.opts.Status
			// A write of an unchanged value records nothing.
			_ = db.SetTagValueQuality(p+StatusBacklog, plc.LINT(st.Backlog), honeycomb.QualityGood)
			_ = db.SetTagValueQuality(p+StatusOldestAge, plc.LREAL(age), honeycomb.QualityGood)
			_ = db.SetTagValueQuality(p+StatusOnline, plc.BOOL(st.Online), honeycomb.QualityGood)
			_ = db.SetTagValueQuality(p+StatusDropped, plc.LINT(st.Spool.Dropped), honeycomb.QualityGood)
			_ = db.SetTagValueQuality(p+StatusLastError, plc.STRING(st.LastError), honeycomb.QualityGood)
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}
