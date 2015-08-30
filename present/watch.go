// Copyright (C) 2015 Space Monkey, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//   http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package present

import (
	"sync"
	"sync/atomic"
	"time"
	"unsafe"

	"golang.org/x/net/context"
	"gopkg.in/spacemonkeygo/monitor.v2"
)

type FinishedSpan struct {
	Span     *monitor.Span
	Err      error
	Panicked bool
	Finish   time.Time
}

// WatchForSpans will watch for traces that cross functions that 'matcher'
// returns true for. As soon as a trace generates a span for a matched
// function, all spans from that trace that finish from that point on are
// collected until the matching function span completes. Those spans are
// returned.
// To cancel this operation, simply cancel the ctx argument.
// There is a small but permanent amount of overhead added by this function to
// every trace that is started while this function is running. This only really
// affects long-running traces.
func WatchForSpans(ctx context.Context, r *monitor.Registry,
	matcher func(f *monitor.Func) bool) (rootSpan *FinishedSpan,
	spansByParent map[*monitor.Span][]*FinishedSpan) {
	collector := newSpanCollector(matcher)
	canceler := r.ObserveTraces(func(t *monitor.Trace) {
		t.ObserveSpans(collector)
	})
	defer canceler()

	select {
	case <-ctx.Done():
		collector.Stop()
		return nil, nil
	case <-collector.Done():
		return collector.Spans()
	}
}

type spanCollector struct {
	// sync/atomic
	check unsafe.Pointer

	// construction
	matcher func(f *monitor.Func) bool
	done    chan struct{}

	// mtx protected
	mtx           sync.Mutex
	root          *FinishedSpan
	spansByParent map[*monitor.Span][]*FinishedSpan
}

func newSpanCollector(matcher func(f *monitor.Func) bool) (rv *spanCollector) {
	return &spanCollector{
		matcher:       matcher,
		done:          make(chan struct{}),
		spansByParent: map[*monitor.Span][]*FinishedSpan{},
	}
}

func (c *spanCollector) Done() <-chan struct{} {
	return c.done
}

type nonce struct {
	int
}

var (
	donePointer = unsafe.Pointer(&nonce{})
)

func (c *spanCollector) Start(s *monitor.Span) {
	if atomic.LoadPointer(&c.check) != nil || !c.matcher(s.Func()) {
		return
	}
	atomic.CompareAndSwapPointer(&c.check, nil, unsafe.Pointer(s))
}

func (c *spanCollector) Finish(s *monitor.Span, err error, panicked bool,
	finish time.Time) {
	existing := atomic.LoadPointer(&c.check)
	if existing == donePointer || existing == nil ||
		((*monitor.Span)(existing)).Trace() != s.Trace() {
		return
	}
	fs := &FinishedSpan{Span: s, Err: err, Panicked: panicked, Finish: finish}
	c.mtx.Lock()
	if c.root != nil {
		c.mtx.Unlock()
		return
	}
	c.spansByParent[s.Parent()] = append(c.spansByParent[s.Parent()], fs)
	if (*monitor.Span)(existing) == s {
		c.root = fs
		c.mtx.Unlock()
		c.Stop()
	} else {
		c.mtx.Unlock()
	}
}

func (c *spanCollector) Stop() {
	atomic.StorePointer(&c.check, donePointer)
	close(c.done)
}

func (c *spanCollector) Spans() (rootSpan *FinishedSpan,
	spansByParent map[*monitor.Span][]*FinishedSpan) {
	// shouldn't need a mutex at this point
	return c.root, c.spansByParent
}
