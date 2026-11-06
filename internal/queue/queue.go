// Copyright 2026 The Loch.sh Authors
// SPDX-License-Identifier: Apache-2.0

// Package queue is the FIFO of invocations waiting for the runtime. It is
// bounded by count and by bytes, and admission happens before a request
// body is read, so that a flood of requests is refused without being
// buffered: the bodies live in the function's own memory.
//
// The count covers the waiting requests (being read or queued), except the
// unverified ones, which have their own caps (see unverified.go). The bytes
// cover every body in memory: waiting, popped but not started, and running;
// they are released when the job gets its outcome.
package queue

import (
	"container/list"
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/loch-sh/lambda-runtime/internal/engine"
)

// Errors of admission.
var (
	// ErrFull: the queue has no room for the request (count or bytes).
	ErrFull = errors.New("queue: full")
	// ErrClosed: the runtime is shutting down. It wraps
	// engine.ErrSourceClosed, so that the engine stops popping.
	ErrClosed = fmt.Errorf("queue: closed: %w", engine.ErrSourceClosed)
	// ErrNotVerified: an unverified reservation cannot be enqueued; call
	// Verified first.
	ErrNotVerified = errors.New("queue: reservation not verified")
)

// Queue implements engine.Source.
type Queue struct {
	maxCount int
	maxBytes int64

	mu     sync.Mutex
	items  *list.List // of *Entry, front first
	count  int        // waiting requests: verified reservations and queued entries
	bytes  int64      // bytes of every body not finished yet
	peak   int        // highest count seen
	closed bool
	// ready is signalled when an entry is queued or the queue closes.
	ready chan struct{}
	// unverified accounts the reservations of requests not authenticated
	// yet (see unverified.go).
	unverified unverifiedBudget
}

// New returns a queue holding at most maxCount waiting requests and
// maxBytes of their bodies.
func New(maxCount int, maxBytes int64) *Queue {
	return &Queue{
		maxCount: maxCount, maxBytes: maxBytes, items: list.New(), ready: make(chan struct{}, 1),
		unverified: unverifiedBudget{
			UnverifiedLimits: UnverifiedLimits{Bytes: maxBytes, PerIP: maxBytes, Count: maxCount, CountPerIP: maxCount},
			byIP:             map[string]*ipUsage{},
		},
	}
}

// Reservation is room taken in the queue for a request whose body is not
// read yet. It must be either enqueued or released.
type Reservation struct {
	q     *Queue
	bytes int64
	// slot is set while the reservation holds a slot of the count;
	// bytesFreed records that its bytes were given back.
	slot, bytesFreed bool
	// unverified is set while the request is not authenticated yet; ip is
	// its source, which the unverified budget accounts.
	unverified bool
	ip         string
}

// Entry is a queued job.
type Entry struct {
	Job   *engine.Job
	res   *Reservation
	elem  *list.Element
	taken bool
}

// Reserve takes room for an authenticated request of up to n bytes.
func (q *Queue) Reserve(n int64) (*Reservation, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if err := q.admitLocked(n); err != nil {
		return nil, err
	}
	return q.takeLocked(n), nil
}

func (q *Queue) admitLocked(n int64) error {
	switch {
	case q.closed:
		return ErrClosed
	case q.count >= q.maxCount, q.bytes+n > q.maxBytes:
		return ErrFull
	}
	return nil
}

func (q *Queue) takeLocked(n int64) *Reservation {
	q.bytes += n
	r := &Reservation{q: q, bytes: n}
	r.takeSlotLocked()
	return r
}

func (r *Reservation) takeSlotLocked() {
	r.slot = true
	r.q.count++
	r.q.peak = max(r.q.peak, r.q.count)
}

// Shrink lowers the reservation to the actual body size once it is read.
func (r *Reservation) Shrink(n int64) {
	r.q.mu.Lock()
	defer r.q.mu.Unlock()
	if !r.bytesFreed && n < r.bytes {
		r.q.bytes -= r.bytes - n
		if r.unverified {
			r.q.unverified.shrink(r.ip, r.bytes-n)
		}
		r.bytes = n
	}
}

// Release gives the room back without queuing anything. It is a no-op once
// the reservation was enqueued or released.
func (r *Reservation) Release() {
	r.q.mu.Lock()
	defer r.q.mu.Unlock()
	r.releaseLocked()
}

func (r *Reservation) releaseLocked() {
	r.freeSlotLocked()
	r.freeBytesLocked()
	r.leaveUnverifiedLocked()
}

func (r *Reservation) freeSlotLocked() {
	if r.slot {
		r.slot = false
		r.q.count--
	}
}

func (r *Reservation) freeBytesLocked() {
	if !r.bytesFreed {
		r.bytesFreed = true
		r.q.bytes -= r.bytes
	}
}

// freeBytes runs when the job gets its outcome.
func (r *Reservation) freeBytes() {
	r.q.mu.Lock()
	defer r.q.mu.Unlock()
	r.freeBytesLocked()
}

// Enqueue queues job at the back. It fails with ErrClosed if the queue
// closed since the reservation (the room is then released), and with
// ErrNotVerified for a reservation not verified yet (it has no slot).
func (r *Reservation) Enqueue(job *engine.Job) (*Entry, error) {
	q := r.q
	q.mu.Lock()
	defer q.mu.Unlock()
	if r.unverified {
		return nil, ErrNotVerified
	}
	if q.closed {
		r.releaseLocked()
		return nil, ErrClosed
	}
	e := &Entry{Job: job, res: r}
	job.OnFinish(r.freeBytes)
	e.elem = q.items.PushBack(e)
	q.signalLocked()
	return e, nil
}

// Cancel removes e if the runtime has not taken it yet, and reports whether
// it did; a taken entry runs to completion.
func (q *Queue) Cancel(e *Entry) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	if e.taken || e.elem == nil {
		return false
	}
	q.items.Remove(e.elem)
	e.elem = nil
	e.res.releaseLocked()
	return true
}

// Pop implements engine.Source: it takes the front entry, waiting for one.
func (q *Queue) Pop(ctx context.Context) (*engine.Job, error) {
	for {
		q.mu.Lock()
		if front := q.items.Front(); front != nil {
			e := q.items.Remove(front).(*Entry)
			e.elem = nil
			e.taken = true
			e.res.freeSlotLocked() // its bytes stay counted until it finishes
			if q.items.Len() > 0 {
				q.signalLocked()
			}
			q.mu.Unlock()
			return e.Job, nil
		}
		closed := q.closed
		q.mu.Unlock()
		if closed {
			return nil, ErrClosed
		}
		select {
		case <-q.ready:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

func (q *Queue) signalLocked() {
	select {
	case q.ready <- struct{}{}:
	default:
	}
}

// Close refuses new requests and returns the jobs still queued, removed
// from the queue: the caller fails them, which frees their bytes.
func (q *Queue) Close() []*engine.Job {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.closed = true
	var jobs []*engine.Job
	for e := q.items.Front(); e != nil; e = q.items.Front() {
		entry := q.items.Remove(e).(*Entry)
		entry.elem = nil
		entry.res.freeSlotLocked()
		jobs = append(jobs, entry.Job)
	}
	q.signalLocked()
	return jobs
}

// Stats is a snapshot of the queue.
type Stats struct {
	// Depth counts the waiting requests, being read or queued, except
	// those not authenticated yet (see UnverifiedCount).
	Depth int
	// Peak is the highest Depth seen.
	Peak int
	// Bytes counts the bodies in memory, running jobs included.
	Bytes int64
}

// Stats returns a snapshot of the queue.
func (q *Queue) Stats() Stats {
	q.mu.Lock()
	defer q.mu.Unlock()
	return Stats{Depth: q.count, Peak: q.peak, Bytes: q.bytes}
}
