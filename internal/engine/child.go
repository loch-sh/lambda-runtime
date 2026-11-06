// Copyright 2026 The Loch.sh Authors
// SPDX-License-Identifier: Apache-2.0

package engine

import (
	"syscall"
	"time"

	"github.com/loch-sh/lambda-runtime/internal/process"
	"github.com/loch-sh/lambda-runtime/internal/runtimeapi"
)

const (
	syscallTerm = syscall.SIGTERM
	syscallKill = syscall.SIGKILL
	// oomScoreAdj makes the kernel pick the bootstrap, not PID 1, on OOM.
	oomScoreAdj = 1000
)

// startChild starts a new bootstrap and its init.
func (l *loop) startChild() {
	l.restartAt.Store(0)
	l.generation = l.deps.Machine.Reset()
	l.initDone, l.suppressed, l.idle, l.initFail = false, false, false, nil
	if !l.stopping {
		l.setState(StateInitializing)
	}
	child, err := l.spawn()
	if err != nil {
		l.log.Error("engine: bootstrap not started", "error", err)
		l.deps.Machine.Close()
		l.failInit(startInitFailure(err))
		l.scheduleRestart(restartInitFailed)
		return
	}
	l.child, l.childDone = child, child.Done()
	l.startedAt = time.Now()
	l.initTimer.set(l.opts.InitTimeout)
	l.log.Info("engine: bootstrap started", "pid", child.Pid())
}

// spawn starts the bootstrap process with its output relayed.
func (l *loop) spawn() (*process.Child, error) {
	stdout, stderr, err := l.deps.Relay.Pipes()
	if err != nil {
		return nil, err
	}
	defer stdout.Close()
	defer stderr.Close()
	return l.deps.Reaper.Start(process.Spec{
		Args:        l.opts.Command,
		Env:         l.opts.Env,
		Dir:         l.opts.Dir,
		Stdout:      stdout,
		Stderr:      stderr,
		OOMScoreAdj: oomScoreAdj,
	})
}

func (l *loop) onInitDone() {
	l.initDone = true
	l.initToReport = time.Since(l.startedAt)
	l.initTimer.stop()
	l.initFailures = 0
	l.obs.InitDone(l.initToReport, true)
	l.log.Info("engine: init done", "duration_ms", l.initToReport.Milliseconds())
	if !l.stopping {
		l.setState(StateReady)
	}
}

// onInitTimeout lets init go on, as Lambda does: its time is charged to the
// first invocation, which may start now.
func (l *loop) onInitTimeout() {
	if l.initDone || l.child == nil {
		return
	}
	l.suppressed = true
	l.log.Warn("engine: init is longer than the init timeout; it goes on, charged to the first invocation",
		"init_timeout", l.opts.InitTimeout.String())
}

// onInitError handles a posted /init/error: error state, and the bootstrap
// is killed (the AWS clients exit by themselves anyway).
func (l *loop) onInitError(f *runtimeapi.InitError) {
	l.log.Warn("engine: init error posted", "error_type", f.ErrorType)
	l.failInit(&initFailure{errorType: f.ErrorType, body: f.Body})
	if l.current != nil {
		l.finish(l.initFail.result(l.current.job.RequestID))
	}
	l.killForRestart()
}

// failInit enters the error state.
func (l *loop) failInit(f *initFailure) {
	l.initFail = f
	l.initTimer.stop()
	l.obs.InitDone(time.Since(l.startedAt), false)
	if !l.stopping {
		l.setState(StateError)
	}
}

// onExit handles the end of the bootstrap.
func (l *loop) onExit(status process.ExitStatus) {
	l.drainEvents()
	l.child, l.childDone, l.idle = nil, nil, false
	l.deps.Machine.Close()
	l.initTimer.stop()
	// Expected: we killed it (timeout, init error, lost invocation) or we
	// are stopping it.
	expected := l.killing || l.stopping
	l.killing = false
	l.log.Info("engine: bootstrap exited", "status", status.String(), "expected", expected)

	if !l.initDone && l.initFail == nil && !expected {
		l.failInit(exitInitFailure(status))
	}
	if cur := l.current; cur != nil {
		switch {
		case l.initFail != nil:
			l.finish(l.initFail.result(cur.job.RequestID))
		case cur.oomKilled(l.opts.Memory):
			l.finish(oomResult(cur.job.RequestID, status))
		default:
			l.finish(exitResult(cur.job.RequestID, status))
		}
	}
	l.killDescendants()
	if l.stopping {
		l.finished = true
		return
	}
	reason := restartCrashed
	switch {
	case l.initFail != nil:
		reason = restartInitFailed
	case expected:
		reason = restartKilled
	}
	l.scheduleRestart(reason)
}

// restartReason tells why the bootstrap is re-inited.
type restartReason int

const (
	// restartInitFailed: init failed; the error state holds until the
	// re-init, after a backoff.
	restartInitFailed restartReason = iota
	// restartKilled: we killed it (timeout, init error, lost invocation);
	// re-init at once.
	restartKilled
	// restartCrashed: it exited on its own after init, during or between
	// invocations. The first crash re-inits at once; consecutive ones back
	// off. Only a result posted by the bootstrap resets the count: a
	// function that crashes on every call is re-inited at most every 30 s.
	restartCrashed
)

// scheduleRestart re-inits the bootstrap, at once or after a backoff.
// While a delayed re-init waits, the state is error after a failed init,
// initializing otherwise.
func (l *loop) scheduleRestart(reason restartReason) {
	var delay time.Duration
	switch reason {
	case restartInitFailed:
		l.initFailures++
		delay = backoff(l.initFailures)
	case restartCrashed:
		l.crashes++
		if l.crashes > 1 {
			delay = backoff(l.crashes - 1)
		}
	}
	if delay == 0 {
		l.startChild()
		return
	}
	if reason != restartInitFailed {
		l.setState(StateInitializing)
	}
	l.log.Info("engine: re-init scheduled", "delay", delay.String())
	l.restartTimer.set(delay)
	l.restartAt.Store(time.Now().Add(delay).UnixNano())
}

// backoff returns 1s, 2s, 4s, … up to maxBackoff for the n-th failure.
func backoff(n int) time.Duration {
	if n > 6 {
		return maxBackoff
	}
	return min(time.Second<<(n-1), maxBackoff)
}

// killForRestart kills the bootstrap; onExit then re-inits it.
func (l *loop) killForRestart() {
	l.deps.Machine.Close()
	l.idle = false
	if l.child != nil {
		l.killing = true
		l.signalChild(syscallKill)
	}
}

func (l *loop) signalChild(sig syscall.Signal) {
	if l.child == nil {
		return
	}
	if err := l.child.SignalGroup(sig); err != nil {
		l.log.Warn("engine: signal not sent", "signal", sig.String(), "error", err)
	}
}

// killDescendants removes what the bootstrap left behind (daemonized
// processes that escaped its group) before the next one starts.
func (l *loop) killDescendants() {
	n, err := process.KillDescendants(l.self)
	if err != nil {
		l.log.Warn("engine: leftover processes not all killed", "error", err)
	}
	if n > 0 {
		l.log.Info("engine: leftover processes killed", "count", n)
	}
}
