// Copyright 2026 The Loch.sh Authors
// SPDX-License-Identifier: Apache-2.0

// Package engine runs the Lambda runtime (the image's bootstrap) and feeds
// it one invocation at a time, the Lambda execution model:
//
//   - it starts the bootstrap at once and tracks its init;
//   - it takes a job from its Source only when the bootstrap waits in /next;
//   - it enforces the function timeout, detects exits and OOM kills, and
//     re-inits the bootstrap whenever it is gone;
//   - it classifies every result with Lambda's precedence: posted init
//     error > posted response or error > response too large > timeout >
//     OOM > exit.
//
// All the state lives in one goroutine (Run); the other goroutines talk to
// it through channels.
package engine

import (
	"context"
	"log/slog"
	"math"
	"sync/atomic"
	"time"

	"github.com/loch-sh/lambda-runtime/internal/cgroup"
	"github.com/loch-sh/lambda-runtime/internal/logs"
	"github.com/loch-sh/lambda-runtime/internal/process"
	"github.com/loch-sh/lambda-runtime/internal/runtimeapi"
)

// State is the health of the runtime.
type State string

// States.
const (
	StateInitializing State = "initializing"
	StateReady        State = "ready"
	StateError        State = "error"
	StateStopping     State = "stopping"
)

// Observer is told about what the engine does (metrics). Its methods are
// called from the engine goroutine and must not block.
type Observer interface {
	InitDone(d time.Duration, ok bool)
	InvocationDone(r *Result)
	StateChanged(s State)
}

type nopObserver struct{}

func (nopObserver) InitDone(time.Duration, bool) {}
func (nopObserver) InvocationDone(*Result)       {}
func (nopObserver) StateChanged(State)           {}

// Options configures an Engine.
type Options struct {
	// Command, Env and Dir describe the bootstrap.
	Command []string
	Env     []string
	Dir     string

	FunctionARN string
	// Timeout is the function timeout.
	Timeout time.Duration
	// InitTimeout is the init budget; beyond it, init goes on and its time
	// is charged to the first invocation ("suppressed init").
	InitTimeout time.Duration
	// ShutdownGrace is given to the running invocation on shutdown.
	ShutdownGrace time.Duration
	// MaxResponse is the response limit, quoted in the too-large error.
	MaxResponse int64

	Platform logs.Platform
	Memory   cgroup.Memory
	Log      *slog.Logger
	Observer Observer
}

// Dependencies are the components the engine drives.
type Dependencies struct {
	Machine *runtimeapi.Machine
	// Events is the channel the Machine reports to.
	Events <-chan runtimeapi.Event
	Reaper *process.Reaper
	Relay  *logs.Relay
	Source Source
}

// Timing constants of the lifecycle.
const (
	// termGrace separates SIGTERM from SIGKILL at shutdown.
	termGrace = 500 * time.Millisecond
	// drainWait bounds the wait for the bootstrap's last log lines.
	drainWait = 20 * time.Millisecond
	// maxBackoff caps the delay between failed inits.
	maxBackoff = 30 * time.Second
)

// Engine runs the bootstrap. Create it with New, then call Run.
type Engine struct {
	opts  Options
	deps  Dependencies
	log   *slog.Logger
	obs   Observer
	state atomic.Value // State
	// self is our pid, the root of the descendants killed before a re-init.
	self int
	// testHook runs functions inside the loop goroutine; nil (never
	// ready) outside tests.
	testHook chan func(*loop)
	// restartAt is when a delayed re-init happens (unix ns), 0 otherwise.
	restartAt atomic.Int64
}

// New returns an engine; nothing starts before Run.
func New(opts Options, deps Dependencies, self int) *Engine {
	e := &Engine{opts: opts, deps: deps, log: opts.Log, obs: opts.Observer, self: self}
	if e.obs == nil {
		e.obs = nopObserver{}
	}
	e.state.Store(StateInitializing)
	return e
}

// RetryAfter advises callers refused meanwhile: the seconds left before a
// delayed re-init, else 1. Safe from any goroutine.
func (e *Engine) RetryAfter() int {
	at := e.restartAt.Load()
	if at == 0 {
		return 1
	}
	return max(1, int(math.Ceil(time.Until(time.Unix(0, at)).Seconds())))
}

// State returns the current state; safe from any goroutine.
func (e *Engine) State() State { return e.state.Load().(State) }

// Run starts the bootstrap and serves jobs until ctx ends, then shuts
// down: the running invocation gets ShutdownGrace to finish, the bootstrap
// gets SIGTERM, then SIGKILL. Run returns once every descendant is gone.
func (e *Engine) Run(ctx context.Context) {
	l := newLoop(e)
	l.startChild()
	l.run(ctx)
}

func (e *Engine) setState(s State) {
	if e.State() != s {
		e.state.Store(s)
		e.obs.StateChanged(s)
	}
}
