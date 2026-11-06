// Copyright 2026 The Loch.sh Authors
// SPDX-License-Identifier: Apache-2.0

package engine

import (
	"context"
	"time"

	"github.com/loch-sh/lambda-runtime/internal/process"
	"github.com/loch-sh/lambda-runtime/internal/runtimeapi"
)

// loop holds the state of the engine goroutine. Nothing in it is shared:
// only Run's goroutine touches it.
type loop struct {
	*Engine

	// The bootstrap.
	child     *process.Child
	childDone <-chan process.ExitStatus
	// generation is the Machine generation of the current bootstrap.
	generation uint64
	startedAt  time.Time
	// killing is set when we killed the bootstrap: its exit is expected.
	killing  bool
	initDone bool
	// initToReport is the init duration, until the next REPORT carries it.
	initToReport time.Duration
	// suppressed is set when init outlived InitTimeout: a job may then
	// start, its timeout running while init completes.
	suppressed bool
	// idle is set while the bootstrap waits in /next.
	idle bool
	// initFail is set in the error state.
	initFail     *initFailure
	initFailures int
	// crashes counts the exits after init without a result posted since.
	crashes int
	// sourceClosed is set once the Source is closed: no more Pop.
	sourceClosed bool

	// Invocations.
	current *invocation
	// pending was popped from the Source but not started yet.
	pending   *Job
	popCancel context.CancelFunc
	popped    chan popResult
	// draining is set while pending events are processed before an exit
	// or a timeout: no new invocation may start meanwhile.
	draining bool

	initTimer, invokeTimer, restartTimer, graceTimer, termTimer timer
	// pendingTimer expires the pending job at its Request.Expires.
	pendingTimer timer

	stopping    bool
	terminating bool
	finished    bool
}

type popResult struct {
	job *Job
	err error
}

func newLoop(e *Engine) *loop {
	return &loop{Engine: e, popped: make(chan popResult, 1)}
}

// run is the event loop.
func (l *loop) run(ctx context.Context) {
	stop := ctx.Done()
	for !l.finished {
		select {
		case <-stop:
			stop = nil
			l.beginShutdown()
		case ev := <-l.deps.Events:
			l.onEvent(ev)
		case st := <-l.childDone:
			l.onExit(st)
		case p := <-l.popped:
			l.onPopped(p)
		case <-l.initTimer.C:
			l.initTimer.stop()
			l.onInitTimeout()
		case <-l.invokeTimer.C:
			l.invokeTimer.stop()
			l.onTimeout()
		case <-l.pendingTimer.C:
			l.pendingTimer.stop()
			l.expirePending()
		case <-l.restartTimer.C:
			l.restartTimer.stop()
			l.startChild()
		case <-l.graceTimer.C:
			l.graceTimer.stop()
			l.terminate()
		case <-l.termTimer.C:
			l.termTimer.stop()
			l.signalChild(syscallKill)
		case f := <-l.testHook:
			f(l)
		}
		l.dispatch()
	}
	l.failPopped()
}

// onEvent handles what the bootstrap did on the Runtime API.
func (l *loop) onEvent(ev runtimeapi.Event) {
	if ev.Generation != l.generation {
		l.log.Debug("engine: event of a previous bootstrap ignored", "kind", ev.Kind)
		return
	}
	switch ev.Kind {
	case runtimeapi.EventIdle:
		l.idle = true
		if ev.Initial && !l.initDone {
			l.onInitDone()
		}
	case runtimeapi.EventResult:
		l.onResult(ev.Result)
	case runtimeapi.EventInitError:
		l.onInitError(ev.InitError)
	case runtimeapi.EventLost:
		// The bootstrap's /next went away with an invocation: it is broken.
		l.log.Warn("engine: invocation lost by the bootstrap, restarting it", "request_id", ev.LostID)
		l.killForRestart()
	}
}

// drainEvents handles the Runtime API events already sent, so that a
// result posted just before an exit or a timeout wins over it.
func (l *loop) drainEvents() {
	l.draining = true
	defer func() { l.draining = false }()
	for {
		select {
		case ev := <-l.deps.Events:
			l.onEvent(ev)
		default:
			return
		}
	}
}

// timer is a stoppable timer whose channel is nil while it is not armed,
// so that it can sit in a select.
type timer struct {
	t *time.Timer
	C <-chan time.Time
}

func (t *timer) set(d time.Duration) {
	t.stop()
	t.t = time.NewTimer(d)
	t.C = t.t.C
}

func (t *timer) stop() {
	if t.t != nil {
		t.t.Stop()
		t.t, t.C = nil, nil
	}
}
