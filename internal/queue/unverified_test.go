// Copyright 2026 The Loch.sh Authors
// SPDX-License-Identifier: Apache-2.0

package queue

import (
	"errors"
	"testing"
)

func TestUnverifiedBudget(t *testing.T) {
	q := New(10, 1000)
	q.SetUnverifiedLimits(UnverifiedLimits{Bytes: 300, PerIP: 200})

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
	a.Verified()
	a.Verified()
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
