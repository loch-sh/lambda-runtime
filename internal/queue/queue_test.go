// Copyright 2026 The Loch.sh Authors
// SPDX-License-Identifier: Apache-2.0

package queue

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/loch-sh/lambda-runtime/internal/engine"
)

func job(id string) *engine.Job { return engine.NewJob(engine.Request{RequestID: id}) }

func enqueue(t *testing.T, q *Queue, id string, n int64) *Entry {
	t.Helper()
	r, err := q.Reserve(n)
	if err != nil {
		t.Fatal(err)
	}
	e, err := r.Enqueue(job(id))
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func popJob(t *testing.T, q *Queue) *engine.Job {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	j, err := q.Pop(ctx)
	if err != nil {
		t.Fatal(err)
	}
	return j
}

// pop pops a job and finishes it at once.
func pop(t *testing.T, q *Queue) string {
	t.Helper()
	j := popJob(t, q)
	engine.Finish(j, &engine.Result{})
	return j.RequestID
}

func TestFIFO(t *testing.T) {
	q := New(10, 1000)
	for _, id := range []string{"a", "b", "c"} {
		enqueue(t, q, id, 10)
	}
	var popped []*engine.Job
	for _, want := range []string{"a", "b", "c"} {
		j := popJob(t, q)
		if j.RequestID != want {
			t.Errorf("pop = %s, want %s", j.RequestID, want)
		}
		popped = append(popped, j)
	}
	// Popped jobs leave the count but keep their bytes until they finish.
	if s := q.Stats(); s.Depth != 0 || s.Bytes != 30 || s.Peak != 3 {
		t.Errorf("stats = %+v", s)
	}
	for _, j := range popped {
		engine.Finish(j, &engine.Result{})
	}
	if s := q.Stats(); s.Bytes != 0 {
		t.Errorf("stats after finish = %+v", s)
	}
}

func TestFullByCountAndBytes(t *testing.T) {
	q := New(2, 100)
	r1, _ := q.Reserve(10)
	if _, err := q.Reserve(95); !errors.Is(err, ErrFull) {
		t.Errorf("bytes: %v", err)
	}
	r2, _ := q.Reserve(10)
	if _, err := q.Reserve(1); !errors.Is(err, ErrFull) {
		t.Errorf("count: %v", err)
	}
	// Releasing gives the room back, twice is harmless.
	r1.Release()
	r1.Release()
	if _, err := q.Reserve(80); err != nil {
		t.Errorf("after release: %v", err)
	}
	// Shrinking frees bytes, never grows.
	r2.Shrink(1)
	r2.Shrink(50)
	if s := q.Stats(); s.Bytes != 81 || s.Depth != 2 {
		t.Errorf("stats = %+v", s)
	}
}

func TestCancel(t *testing.T) {
	q := New(10, 1000)
	a := enqueue(t, q, "a", 1)
	b := enqueue(t, q, "b", 1)
	if !q.Cancel(b) {
		t.Error("cancel of a queued entry must succeed")
	}
	if q.Cancel(b) {
		t.Error("second cancel must report false")
	}
	if got := pop(t, q); got != "a" {
		t.Errorf("pop = %s", got)
	}
	if q.Cancel(a) {
		t.Error("cancel of a taken entry must report false")
	}
	if s := q.Stats(); s.Depth != 0 {
		t.Errorf("stats = %+v", s)
	}
}

func TestPopWaitsAndHonorsContext(t *testing.T) {
	q := New(10, 1000)
	got := make(chan string, 1)
	go func() {
		j, err := q.Pop(context.Background())
		if err == nil {
			got <- j.RequestID
		}
	}()
	time.Sleep(20 * time.Millisecond)
	enqueue(t, q, "late", 1)
	select {
	case id := <-got:
		if id != "late" {
			t.Errorf("pop = %s", id)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("pop did not wake up")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := q.Pop(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v", err)
	}
}

func TestClose(t *testing.T) {
	q := New(10, 1000)
	enqueue(t, q, "a", 1)
	enqueue(t, q, "b", 1)
	pending, _ := q.Reserve(1)
	jobs := q.Close()
	if len(jobs) != 2 || jobs[0].RequestID != "a" {
		t.Errorf("jobs = %v", jobs)
	}
	for _, j := range jobs {
		j.Fail(engine.ErrShuttingDown)
	}
	pending.Release()
	if _, err := q.Reserve(1); !errors.Is(err, ErrClosed) {
		t.Errorf("reserve after close: %v", err)
	}
	if _, err := pending.Enqueue(job("c")); !errors.Is(err, ErrClosed) {
		t.Errorf("enqueue after close: %v", err)
	}
	if _, err := q.Pop(context.Background()); !errors.Is(err, ErrClosed) {
		t.Errorf("pop after close: %v", err)
	}
	if s := q.Stats(); s.Depth != 0 || s.Bytes != 0 {
		t.Errorf("stats = %+v", s)
	}
}

func TestConcurrentProducersAndConsumer(t *testing.T) {
	q := New(1000, 1<<20)
	var wg sync.WaitGroup
	for i := range 200 {
		wg.Go(func() {
			r, err := q.Reserve(int64(i))
			if err != nil {
				t.Error(err)
				return
			}
			if _, err := r.Enqueue(job("x")); err != nil {
				t.Error(err)
			}
		})
	}
	for range 200 {
		pop(t, q)
	}
	wg.Wait()
	if s := q.Stats(); s.Depth != 0 || s.Bytes != 0 {
		t.Errorf("stats = %+v", s)
	}
}

func TestRunningJobBytesCountAgainstTheBudget(t *testing.T) {
	q := New(10, 100)
	enqueue(t, q, "running", 80)
	j := popJob(t, q)
	// The running job still holds 80 bytes: a 30-byte request does not fit.
	if _, err := q.Reserve(30); !errors.Is(err, ErrFull) {
		t.Errorf("reserve while running: %v", err)
	}
	engine.Finish(j, &engine.Result{})
	if _, err := q.Reserve(30); err != nil {
		t.Errorf("reserve after finish: %v", err)
	}
}
