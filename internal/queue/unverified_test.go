// Copyright 2026 The Loch.sh Authors
// SPDX-License-Identifier: Apache-2.0

package queue

import (
	"errors"
	"testing"
)

func TestUnverifiedBudget(t *testing.T) {
	q := New(10, 1000)
	q.SetUnverifiedLimits(UnverifiedLimits{Bytes: 300, PerIP: 200, Count: 10, CountPerIP: 10})

	a, err := q.ReserveUnverified(200, "10.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	// Per-IP cap.
	if _, err := q.ReserveUnverified(1, "10.0.0.1"); !errors.Is(err, ErrFull) {
		t.Errorf("per-IP: %v", err)
	}
	// Global unverified cap.
	b, err := q.ReserveUnverified(100, "10.0.0.2")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := q.ReserveUnverified(1, "10.0.0.3"); !errors.Is(err, ErrFull) {
		t.Errorf("global: %v", err)
	}
	// Authenticated requests still fit in the main budget.
	if _, err := q.Reserve(500); err != nil {
		t.Errorf("verified request refused: %v", err)
	}
	// Verified moves the bytes out of the unverified budget (shrunk first).
	a.Shrink(150)
	if got := q.UnverifiedBytes(); got != 250 {
		t.Errorf("unverified after shrink = %d", got)
	}
	if err := a.Verified(); err != nil {
		t.Fatal(err)
	}
	if err := a.Verified(); err != nil {
		t.Errorf("second Verified: %v", err)
	}
	if got := q.UnverifiedBytes(); got != 100 {
		t.Errorf("unverified after verify = %d", got)
	}
	if _, err := q.ReserveUnverified(200, "10.0.0.1"); err != nil {
		t.Errorf("room not given back: %v", err)
	}
	// A release of an unverified reservation frees both budgets.
	b.Release()
	if got := q.UnverifiedBytes(); got != 200 {
		t.Errorf("unverified after release = %d", got)
	}
	q.Close()
	if _, err := q.ReserveUnverified(1, "x"); !errors.Is(err, ErrClosed) {
		t.Errorf("closed: %v", err)
	}
}

// TestUnverifiedTakeNoSlot: requests that will never be verified (a valid
// access key id without the secret, a body never sent) must not fill the
// queue; their number is capped, globally and per IP.
func TestUnverifiedTakeNoSlot(t *testing.T) {
	q := New(2, 1000)
	q.SetUnverifiedLimits(UnverifiedLimits{Bytes: 1000, PerIP: 1000, Count: 3, CountPerIP: 2})
	reserve := func(ip string) *Reservation {
		t.Helper()
		r, err := q.ReserveUnverified(1, ip)
		if err != nil {
			t.Fatalf("%s: %v", ip, err)
		}
		return r
	}
	firstA, secondA, fromB := reserve("a"), reserve("a"), reserve("b")

	t.Run("count caps", func(t *testing.T) {
		if _, err := q.ReserveUnverified(1, "c"); !errors.Is(err, ErrFull) {
			t.Errorf("global count: %v", err)
		}
		if s := q.Stats(); s.Depth != 0 || s.Bytes != 3 || q.UnverifiedCount() != 3 {
			t.Errorf("unverified requests hold slots: %+v, %d unverified", s, q.UnverifiedCount())
		}
		if _, err := firstA.Enqueue(nil); !errors.Is(err, ErrNotVerified) {
			t.Errorf("unverified enqueued: %v", err)
		}
	})

	// Authenticated callers still get every slot.
	one, err := q.Reserve(1)
	if err != nil {
		t.Fatal(err)
	}
	two, err := q.Reserve(1)
	if err != nil {
		t.Fatal(err)
	}

	t.Run("verified into a full queue", func(t *testing.T) {
		if err := firstA.Verified(); !errors.Is(err, ErrFull) {
			t.Errorf("verified into a full queue: %v", err)
		}
		if s := q.Stats(); s.Depth != 2 || s.Bytes != 4 || q.UnverifiedCount() != 2 {
			t.Errorf("refused reservation not released: %+v, %d unverified", s, q.UnverifiedCount())
		}
		// Per-IP count cap: "a" holds secondA only, so one more fits.
		extraA := reserve("a")
		if _, err := q.ReserveUnverified(1, "a"); !errors.Is(err, ErrFull) {
			t.Errorf("per-IP count: %v", err)
		}
		extraA.Release()
	})

	t.Run("verified with room", func(t *testing.T) {
		one.Release()
		if err := secondA.Verified(); err != nil {
			t.Errorf("verified with room: %v", err)
		}
		if s := q.Stats(); s.Depth != 2 {
			t.Errorf("depth = %d", s.Depth)
		}
	})

	t.Run("verified after close", func(t *testing.T) {
		two.Release()
		q.Close()
		if err := fromB.Verified(); !errors.Is(err, ErrClosed) {
			t.Errorf("verified after close: %v", err)
		}
		if q.UnverifiedCount() != 0 || q.UnverifiedBytes() != 0 {
			t.Errorf("unverified left: %d, %d bytes", q.UnverifiedCount(), q.UnverifiedBytes())
		}
	})
}
