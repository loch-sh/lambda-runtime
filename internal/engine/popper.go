// Copyright 2026 The Loch.sh Authors
// SPDX-License-Identifier: Apache-2.0

package engine

import (
	"context"
	"errors"
	"time"
)

// ErrSourceClosed is what a Source returns from Pop once it is closed;
// the engine then stops taking jobs. Sources wrap it or return it as is.
var ErrSourceClosed = errors.New("engine: source closed")

// startPopping waits for the next job in the background. At most one Pop
// runs at a time: a new one starts only after the previous one returned,
// so a job popped despite a cancellation is never overwritten.
func (l *loop) startPopping() {
	if l.popCancel != nil || l.sourceClosed {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	l.popCancel = cancel
	go func() {
		job, err := l.deps.Source.Pop(ctx)
		l.popped <- popResult{job, err}
	}()
}

// stopPopping cancels the running Pop; its result still arrives.
func (l *loop) stopPopping() {
	if l.popCancel != nil {
		l.popCancel()
	}
}

func (l *loop) onPopped(p popResult) {
	l.stopPopping() // releases the Pop's context
	l.popCancel = nil
	switch {
	case errors.Is(p.err, ErrSourceClosed):
		l.sourceClosed = true
	case p.err != nil:
		// Cancelled by us.
	case l.stopping:
		p.job.Fail(ErrShuttingDown)
	default:
		l.setPending(p.job)
	}
}

// setPending keeps job until it can start, at most until its Expires.
func (l *loop) setPending(job *Job) {
	l.pending = job
	if !job.Expires.IsZero() {
		l.pendingTimer.set(time.Until(job.Expires))
	}
}

// takePending returns the pending job and forgets it.
func (l *loop) takePending() *Job {
	job := l.pending
	l.pending = nil
	l.pendingTimer.stop()
	return job
}

// expirePending fails the pending job that waited past its Expires, as the
// queue would have if it had stayed there.
func (l *loop) expirePending() {
	if l.pending != nil {
		l.takePending().Fail(ErrQueueTimeout)
	}
}

// failPopped settles a Pop still running when the loop ends: the job it
// may have taken is answered, never lost.
func (l *loop) failPopped() {
	if l.popCancel == nil {
		return
	}
	l.popCancel()
	if p := <-l.popped; p.job != nil {
		p.job.Fail(ErrShuttingDown)
	}
	l.popCancel = nil
}
