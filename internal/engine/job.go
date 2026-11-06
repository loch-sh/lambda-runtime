// Copyright 2026 The Loch.sh Authors
// SPDX-License-Identifier: Apache-2.0

package engine

import (
	"context"
	"errors"
	"time"
)

// Errors of the jobs the engine does not run.
var (
	// ErrShuttingDown: the runtime is stopping.
	ErrShuttingDown = errors.New("engine: the runtime is shutting down")
	// ErrQueueTimeout: the job waited past Request.Expires for the
	// bootstrap to be ready.
	ErrQueueTimeout = errors.New("engine: the job waited too long to start")
)

// Request is what an invocation carries to the runtime.
type Request struct {
	RequestID     string
	Payload       []byte
	TraceID       string
	ClientContext string
	TenantID      string
	// Expires, when set, is when a job not started yet stops waiting.
	Expires time.Time
}

// Kind classifies the result of an invocation (and labels the metrics).
type Kind string

// Result kinds.
const (
	KindSuccess   Kind = "success"
	KindError     Kind = "error"
	KindTimeout   Kind = "timeout"
	KindExit      Kind = "exit"
	KindOOM       Kind = "oom"
	KindTooLarge  Kind = "too_large"
	KindInitError Kind = "init_error"
)

// Kinds lists every result kind, for the metrics.
var Kinds = []Kind{KindSuccess, KindError, KindTimeout, KindExit, KindOOM, KindTooLarge, KindInitError}

// Result is the answer to an invocation. Every kind but KindSuccess is a
// function error (X-Amz-Function-Error: Unhandled).
type Result struct {
	RequestID   string
	Kind        Kind
	Payload     []byte
	ContentType string
	// ErrorType is the errorType of a failed invocation.
	ErrorType string
	Duration  time.Duration
	// LogTail is the end of the invocation's logs, START to REPORT.
	LogTail []byte
}

// FunctionError reports whether the result is a function error.
func (r *Result) FunctionError() bool { return r.Kind != KindSuccess }

// Job is a Request waiting for its Result.
type Job struct {
	Request
	done chan jobOutcome
	// onFinish runs once, when the job gets its outcome.
	onFinish func()
}

type jobOutcome struct {
	result *Result
	err    error
}

// NewJob returns a job for req.
func NewJob(req Request) *Job {
	return &Job{Request: req, done: make(chan jobOutcome, 1)}
}

// Wait returns the result of the job, or ctx's error if ctx ends first
// (the result is then dropped when it comes).
func (j *Job) Wait(ctx context.Context) (*Result, error) {
	select {
	case o := <-j.done:
		return o.result, o.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// Finish ends the job with r; for the components that answer jobs
// themselves (tests, fakes).
func Finish(j *Job, r *Result) { j.succeed(r) }

// Fail ends the job with err without running it.
func (j *Job) Fail(err error) { j.finish(jobOutcome{err: err}) }

func (j *Job) succeed(r *Result) { j.finish(jobOutcome{result: r}) }

// OnFinish sets a function run once when the job gets its outcome (the
// queue releases the memory budget of its body). Call it before the job
// is shared.
func (j *Job) OnFinish(f func()) { j.onFinish = f }

// finish never blocks: done has room for the single outcome, and later
// outcomes are dropped.
func (j *Job) finish(o jobOutcome) {
	select {
	case j.done <- o:
		if j.onFinish != nil {
			j.onFinish()
		}
	default:
	}
}

// Source is where the engine takes its jobs from: the invocation queue.
type Source interface {
	// Pop blocks until a job is available or ctx ends. The job returned is
	// the engine's: its client can no longer withdraw it.
	Pop(ctx context.Context) (*Job, error)
}
