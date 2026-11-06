// Copyright 2026 The Loch.sh Authors
// SPDX-License-Identifier: Apache-2.0

package engine

// beginShutdown stops taking jobs and gives the running invocation
// ShutdownGrace to finish; dispatch terminates as soon as none runs.
func (l *loop) beginShutdown() {
	l.stopping = true
	l.setState(StateStopping)
	l.log.Info("engine: shutting down")
	l.stopPopping()
	l.restartTimer.stop()
	l.initTimer.stop()
	if l.pending != nil {
		l.takePending().Fail(ErrShuttingDown)
	}
	if l.current != nil {
		l.graceTimer.set(l.opts.ShutdownGrace)
	}
}

// terminate stops the bootstrap: SIGTERM to its group, SIGKILL after
// termGrace. Run ends when it is reaped (onExit).
func (l *loop) terminate() {
	l.terminating = true
	if l.child == nil {
		l.killDescendants()
		l.finished = true
		return
	}
	l.deps.Machine.Close()
	l.signalChild(syscallTerm)
	l.termTimer.set(termGrace)
}
