// Copyright 2026 The Loch.sh Authors
// SPDX-License-Identifier: Apache-2.0

package engine

import (
	"time"

	"github.com/loch-sh/lambda-runtime/internal/cgroup"
	"github.com/loch-sh/lambda-runtime/internal/logs"
	"github.com/loch-sh/lambda-runtime/internal/runtimeapi"
)

// invocation is the job in progress.
type invocation struct {
	job      *Job
	started  time.Time
	deadline time.Time
	// delivered is set once the bootstrap received the event.
	delivered bool
	// oomBefore is the cgroup oom_kill counter when the job started.
	oomBefore int64
	oomKnown  bool
}

// oomKilled reports whether the invocation ended because of the OOM
// killer: the cgroup counted a new OOM kill since it started. The victim
// may be a descendant (a shell bootstrap that does not exec, an
// AWS_LAMBDA_EXEC_WRAPPER), so the exit status of the bootstrap itself is
// not required to be SIGKILL.
func (inv *invocation) oomKilled(mem cgroup.Memory) bool {
	if !inv.oomKnown {
		return false
	}
	after, ok := mem.OOMKills()
	return ok && after > inv.oomBefore
}

// dispatch moves jobs forward after any change; it is idempotent.
func (l *loop) dispatch() {
	switch {
	case l.draining:
		return
	case l.stopping:
		if l.current == nil && !l.terminating {
			l.graceTimer.stop()
			l.terminate()
		}
		return
	case l.initFail != nil:
		// Error state: every job gets the init error at once.
		if l.pending != nil {
			job := l.takePending()
			l.complete(job, l.initFail.result(job.RequestID))
		}
		l.startPopping()
		return
	case l.current != nil:
		if !l.current.delivered && l.idle {
			l.deliver(l.current)
		}
		return
	case l.pending != nil:
		if l.canStart() {
			l.begin(l.takePending())
		}
		return
	case l.canStart():
		l.startPopping()
	}
}

// canStart reports whether a job may start: the bootstrap waits in /next,
// or its init outlived the init timeout.
func (l *loop) canStart() bool {
	if l.child == nil || l.killing {
		return false
	}
	return (l.initDone && l.idle) || (!l.initDone && l.suppressed)
}

// begin starts job: its timeout runs from now (time spent in the queue
// does not count), and so does its log capture.
func (l *loop) begin(job *Job) {
	now := time.Now()
	inv := &invocation{job: job, started: now, deadline: now.Add(l.opts.Timeout)}
	inv.oomBefore, inv.oomKnown = l.opts.Memory.OOMKills()
	l.current = inv
	l.deps.Relay.StartCapture(l.opts.Platform.Start(job.RequestID))
	l.invokeTimer.set(l.opts.Timeout)
	if l.idle {
		l.deliver(inv)
	}
}

// deliver hands inv to the bootstrap parked in /next.
func (l *loop) deliver(inv *invocation) {
	l.idle = false
	err := l.deps.Machine.Deliver(l.generation, &runtimeapi.Invocation{
		ID:            inv.job.RequestID,
		Payload:       inv.job.Payload,
		Deadline:      inv.deadline,
		FunctionARN:   l.opts.FunctionARN,
		TraceID:       inv.job.TraceID,
		ClientContext: inv.job.ClientContext,
		TenantID:      inv.job.TenantID,
	})
	if err != nil {
		// The /next went away; the next one gets it.
		l.log.Debug("engine: delivery postponed", "request_id", inv.job.RequestID, "error", err)
		return
	}
	inv.delivered = true
}

// onResult handles a response or error posted by the bootstrap.
func (l *loop) onResult(res runtimeapi.Result) {
	cur := l.current
	if cur == nil || cur.job.RequestID != res.ID {
		l.log.Warn("engine: result for an invocation that is not running ignored")
		return
	}
	switch res.Outcome {
	case runtimeapi.OutcomeSuccess:
		contentType := res.ContentType
		if contentType == "" {
			contentType = "application/json"
		}
		l.finish(&Result{Kind: KindSuccess, Payload: res.Body, ContentType: contentType})
	case runtimeapi.OutcomeFunctionError:
		l.finish(&Result{Kind: KindError, Payload: res.Body, ContentType: "application/json", ErrorType: res.ErrorType})
	case runtimeapi.OutcomeResponseTooLarge:
		l.finish(tooLargeResult(res.ID, l.opts.MaxResponse))
	default:
		l.log.Error("engine: unknown result outcome", "outcome", int(res.Outcome))
		l.finish(errorResult(res.ID, KindError, "Runtime.Unknown", "The runtime posted an unknown result"))
	}
	l.crashes = 0
}

// onTimeout ends the invocation that ran out of time and kills the
// bootstrap, which is re-inited before the next invocation.
func (l *loop) onTimeout() {
	l.drainEvents()
	cur := l.current
	if cur == nil {
		return
	}
	l.deps.Relay.Drain(drainWait) // the function's last lines first
	if line := l.opts.Platform.Timeout(cur.job.RequestID, l.opts.Timeout); line != "" {
		l.deps.Relay.Platform(line)
	}
	l.finish(timeoutResult(cur.job.RequestID, l.opts.Timeout))
	l.killForRestart()
}

// finish completes the current invocation with res: END and REPORT lines,
// log tail, then the answer to the job.
func (l *loop) finish(res *Result) {
	cur := l.current
	l.current = nil
	l.invokeTimer.stop()
	res.RequestID = cur.job.RequestID
	res.Duration = time.Since(cur.started)
	res.LogTail = l.writeFooter(res)
	l.complete(cur.job, res)
}

// writeFooter writes the END and REPORT lines of res and returns the
// captured logs of the invocation. Max Memory Used is the peak of the
// whole container since it started: the cgroup is read-only, its peak
// cannot be reset per invocation.
func (l *loop) writeFooter(res *Result) []byte {
	relay, platform := l.deps.Relay, l.opts.Platform
	relay.Drain(drainWait) // also after an exit: the pipes may hold its last lines
	status := platformStatus(res.Kind)
	relay.Platform(platform.End(res.RequestID, status))
	report := logs.Report{
		RequestID:    res.RequestID,
		Duration:     res.Duration,
		InitDuration: l.initToReport,
		Status:       status,
		ErrorType:    res.ErrorType,
	}
	l.initToReport = 0
	if peak, ok := l.opts.Memory.PeakBytes(); ok {
		report.MaxMemoryUsedMB = cgroup.MB(peak)
	}
	relay.Platform(platform.Report(report))
	return relay.StopCapture()
}

// complete answers job and reports it.
func (l *loop) complete(job *Job, res *Result) {
	res.RequestID = job.RequestID // for the jobs answered without running
	job.succeed(res)
	l.obs.InvocationDone(res)
	l.log.Debug("engine: invocation done", "request_id", res.RequestID, "kind", res.Kind,
		"duration_ms", res.Duration.Milliseconds())
}

func platformStatus(k Kind) logs.Status {
	switch k {
	case KindSuccess:
		return logs.StatusSuccess
	case KindTimeout:
		return logs.StatusTimeout
	}
	return logs.StatusError
}
