// Copyright 2026 The Loch.sh Authors
// SPDX-License-Identifier: Apache-2.0

// Package metrics collects the runtime's figures and writes them in the
// Prometheus text exposition format (written by hand: no dependency).
// Labels only carry fixed values: never event contents or secrets.
package metrics

import (
	"fmt"
	"io"
	"maps"
	"slices"
	"sync"
	"time"

	"github.com/loch-sh/lambda-runtime/internal/engine"
)

// OutcomeThrottled is the outcome label of the requests refused with 429,
// besides the engine's result kinds.
const OutcomeThrottled = "throttled"

// durationBuckets are the upper bounds (seconds) of the duration
// histograms, from 1 ms to 15 min.
var durationBuckets = []float64{0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60, 300, 900}

// QueueStats gives the queue figures at scrape time.
type QueueStats func() (depth, peak int)

// PeakMemory gives the cgroup memory peak at scrape time.
type PeakMemory func() (int64, bool)

// Collector implements engine.Observer and serves the figures.
type Collector struct {
	version string
	queue   QueueStats
	memory  PeakMemory

	mu          sync.Mutex
	invocations map[string]uint64
	durations   *histogram
	inits       map[string]uint64
	initTimes   *histogram
	authFails   uint64
	state       engine.State
}

// New returns a collector.
func New(version string, queue QueueStats, memory PeakMemory) *Collector {
	return &Collector{
		version:     version,
		queue:       queue,
		memory:      memory,
		invocations: map[string]uint64{},
		durations:   newHistogram(),
		inits:       map[string]uint64{},
		initTimes:   newHistogram(),
		state:       engine.StateInitializing,
	}
}

// InitDone implements engine.Observer.
func (c *Collector) InitDone(d time.Duration, ok bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	result := "success"
	if !ok {
		result = "failure"
	}
	c.inits[result]++
	c.initTimes.observe(d.Seconds())
}

// InvocationDone implements engine.Observer.
func (c *Collector) InvocationDone(r *engine.Result) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.invocations[string(r.Kind)]++
	if r.Kind != engine.KindInitError {
		c.durations.observe(r.Duration.Seconds())
	}
}

// StateChanged implements engine.Observer.
func (c *Collector) StateChanged(s engine.State) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.state = s
}

// Throttled counts an invocation refused with 429.
func (c *Collector) Throttled() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.invocations[OutcomeThrottled]++
}

// AuthFailed counts a request refused for its credential.
func (c *Collector) AuthFailed() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.authFails++
}

// allOutcomes are always exposed, at 0 until they happen.
var allOutcomes = func() []string {
	out := make([]string, 0, len(engine.Kinds)+1)
	for _, k := range engine.Kinds {
		out = append(out, string(k))
	}
	return append(out, OutcomeThrottled)
}()

var allStates = []engine.State{engine.StateInitializing, engine.StateReady, engine.StateError, engine.StateStopping}

// snapshot is a copy of the figures, printed without holding the lock: a
// slow scraper must never hold up the engine, which updates them.
type snapshot struct {
	invocations map[string]uint64
	durations   histogram
	inits       map[string]uint64
	initTimes   histogram
	authFails   uint64
	state       engine.State
}

func (c *Collector) snapshot() snapshot {
	c.mu.Lock()
	defer c.mu.Unlock()
	return snapshot{
		invocations: maps.Clone(c.invocations),
		durations:   c.durations.clone(),
		inits:       maps.Clone(c.inits),
		initTimes:   c.initTimes.clone(),
		authFails:   c.authFails,
		state:       c.state,
	}
}

// WriteTo writes every metric in the Prometheus text format.
func (c *Collector) WriteTo(w io.Writer) (int64, error) {
	depth, peak := c.queue()
	memory, memoryOK := c.memory()
	s := c.snapshot()
	p := &printer{w: w}
	p.header("loch_lambda_runtime_info", "gauge", "Version of the runtime.")
	p.line("loch_lambda_runtime_info{version=%q} 1", c.version)

	p.header("loch_lambda_invocations_total", "counter", "Invocations by outcome.")
	for _, o := range allOutcomes {
		p.line("loch_lambda_invocations_total{outcome=%q} %d", o, s.invocations[o])
	}
	s.durations.write(p, "loch_lambda_invocation_duration_seconds", "Duration of the invocations run by the bootstrap.")

	p.header("loch_lambda_auth_failures_total", "counter", "Requests refused for their credential.")
	p.line("loch_lambda_auth_failures_total %d", s.authFails)

	p.header("loch_lambda_inits_total", "counter", "Inits of the bootstrap by result.")
	for _, r := range []string{"success", "failure"} {
		p.line("loch_lambda_inits_total{result=%q} %d", r, s.inits[r])
	}
	s.initTimes.write(p, "loch_lambda_init_duration_seconds", "Duration of the inits of the bootstrap.")

	p.header("loch_lambda_queue_depth", "gauge", "Invocations waiting.")
	p.line("loch_lambda_queue_depth %d", depth)
	p.header("loch_lambda_queue_depth_max", "gauge", "Highest number of invocations waiting since start.")
	p.line("loch_lambda_queue_depth_max %d", peak)

	if memoryOK {
		p.header("loch_lambda_memory_peak_bytes", "gauge", "Peak memory of the container (cgroup memory.peak).")
		p.line("loch_lambda_memory_peak_bytes %d", memory)
	}

	p.header("loch_lambda_state", "gauge", "Current state of the runtime (1 for the current one).")
	for _, st := range allStates {
		v := 0
		if st == s.state {
			v = 1
		}
		p.line("loch_lambda_state{state=%q} %d", st, v)
	}
	return p.n, p.err
}

// printer writes lines and keeps the first error.
type printer struct {
	w   io.Writer
	n   int64
	err error
}

func (p *printer) line(format string, args ...any) {
	if p.err != nil {
		return
	}
	n, err := fmt.Fprintf(p.w, format+"\n", args...)
	p.n += int64(n)
	p.err = err
}

func (p *printer) header(name, kind, help string) {
	p.line("# HELP %s %s", name, help)
	p.line("# TYPE %s %s", name, kind)
}

// histogram is a cumulative Prometheus histogram.
type histogram struct {
	counts []uint64 // per bucket, not cumulative
	count  uint64
	sum    float64
}

func newHistogram() *histogram { return &histogram{counts: make([]uint64, len(durationBuckets))} }

func (h *histogram) clone() histogram {
	c := *h
	c.counts = slices.Clone(h.counts)
	return c
}

func (h *histogram) observe(v float64) {
	if i, _ := slices.BinarySearch(durationBuckets, v); i < len(durationBuckets) {
		h.counts[i]++
	}
	h.count++
	h.sum += v
}

func (h *histogram) write(p *printer, name, help string) {
	p.header(name, "histogram", help)
	var cumulative uint64
	for i, le := range durationBuckets {
		cumulative += h.counts[i]
		p.line("%s_bucket{le=\"%g\"} %d", name, le, cumulative)
	}
	p.line("%s_bucket{le=\"+Inf\"} %d", name, h.count)
	p.line("%s_sum %g", name, h.sum)
	p.line("%s_count %d", name, h.count)
}
