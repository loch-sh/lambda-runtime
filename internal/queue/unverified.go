// Copyright 2026 The Loch.sh Authors
// SPDX-License-Identifier: Apache-2.0

package queue

// A SigV4 request whose payload hash is not in a header can only be
// authenticated once its body is read. Its reservation is "unverified"
// until then, and accounted in a smaller budget, globally and per source
// IP, so that a caller with a valid access key id but no secret cannot
// starve the queue with bodies that will be refused.

// UnverifiedLimits bound the bytes reserved by unverified requests.
type UnverifiedLimits struct {
	// Bytes bounds them all; PerIP bounds those of one source IP.
	Bytes, PerIP int64
}

type unverifiedBudget struct {
	max, perIP int64
	total      int64
	byIP       map[string]int64
}

func (b *unverifiedBudget) fits(ip string, n int64) bool {
	return b.total+n <= b.max && b.byIP[ip]+n <= b.perIP
}

func (b *unverifiedBudget) add(ip string, n int64) {
	b.total += n
	b.byIP[ip] += n
}

// shrink takes n bytes off ip's account ("" is not unverified).
func (b *unverifiedBudget) shrink(ip string, n int64) {
	if ip == "" {
		return
	}
	b.total -= n
	if b.byIP[ip] -= n; b.byIP[ip] <= 0 {
		delete(b.byIP, ip)
	}
}

// SetUnverifiedLimits sets the budget of unverified reservations (by
// default, the whole byte budget).
func (q *Queue) SetUnverifiedLimits(l UnverifiedLimits) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.unverified.max, q.unverified.perIP = l.Bytes, l.PerIP
}

// ReserveUnverified takes room for a request of up to n bytes from ip that
// is not authenticated yet; call Verified once it is.
func (q *Queue) ReserveUnverified(n int64, ip string) (*Reservation, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if err := q.admitLocked(n); err != nil {
		return nil, err
	}
	if !q.unverified.fits(ip, n) {
		return nil, ErrFull
	}
	r := q.takeLocked(n)
	r.unverifiedIP = ip
	q.unverified.add(ip, n)
	return r, nil
}

// Verified moves the reservation out of the unverified budget.
func (r *Reservation) Verified() {
	r.q.mu.Lock()
	defer r.q.mu.Unlock()
	r.verifiedLocked()
}

// verifiedLocked gives the bytes back to the unverified budget, once.
func (r *Reservation) verifiedLocked() {
	if r.unverifiedIP != "" {
		r.q.unverified.shrink(r.unverifiedIP, r.bytes)
		r.unverifiedIP = ""
	}
}

// UnverifiedBytes returns the bytes held by unverified reservations.
func (q *Queue) UnverifiedBytes() int64 {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.unverified.total
}
