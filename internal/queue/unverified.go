// Copyright 2026 The Loch.sh Authors
// SPDX-License-Identifier: Apache-2.0

package queue

// A SigV4 request whose payload hash is not in a header can only be
// authenticated once its body is read. Its reservation is "unverified"
// until then. It holds the bytes of its body but no slot of the queue, and
// it is accounted in a smaller budget, globally and per source IP: a
// caller with a valid access key id but no secret cannot starve the queue
// with bodies that will be refused, or never sent. The request takes its
// slot once verified, and is refused then if the queue is full.

// UnverifiedLimits bound the reservations of unverified requests.
type UnverifiedLimits struct {
	// Bytes bounds their bytes; PerIP those of one source IP.
	Bytes, PerIP int64
	// Count bounds their number; CountPerIP that of one source IP.
	Count, CountPerIP int
}

// ipUsage is what the unverified reservations of one source IP hold.
type ipUsage struct {
	bytes int64
	count int
}

// unverifiedBudget accounts the unverified reservations, against the caps
// of the embedded UnverifiedLimits. Guarded by Queue.mu.
type unverifiedBudget struct {
	UnverifiedLimits
	bytes int64
	count int
	byIP  map[string]*ipUsage
}

func (b *unverifiedBudget) fits(ip string, n int64) bool {
	u := b.byIP[ip]
	if u == nil {
		u = &ipUsage{}
	}
	return b.bytes+n <= b.Bytes && u.bytes+n <= b.PerIP &&
		b.count < b.Count && u.count < b.CountPerIP
}

func (b *unverifiedBudget) add(ip string, n int64) {
	u := b.byIP[ip]
	if u == nil {
		u = &ipUsage{}
		b.byIP[ip] = u
	}
	b.bytes += n
	b.count++
	u.bytes += n
	u.count++
}

// shrink takes n bytes off ip's account.
func (b *unverifiedBudget) shrink(ip string, n int64) {
	b.bytes -= n
	b.byIP[ip].bytes -= n
}

// remove takes a reservation of n bytes off ip's account.
func (b *unverifiedBudget) remove(ip string, n int64) {
	b.shrink(ip, n)
	b.count--
	u := b.byIP[ip]
	if u.count--; u.count == 0 {
		delete(b.byIP, ip)
	}
}

// SetUnverifiedLimits sets the budget of unverified reservations (by
// default, the whole queue).
func (q *Queue) SetUnverifiedLimits(l UnverifiedLimits) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.unverified.UnverifiedLimits = l
}

// ReserveUnverified takes room for a request of up to n bytes from ip that
// is not authenticated yet: bytes, but no slot. Call Verified once it is.
func (q *Queue) ReserveUnverified(n int64, ip string) (*Reservation, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	switch {
	case q.closed:
		return nil, ErrClosed
	case q.bytes+n > q.maxBytes, !q.unverified.fits(ip, n):
		return nil, ErrFull
	}
	q.bytes += n
	q.unverified.add(ip, n)
	return &Reservation{q: q, bytes: n, unverified: true, ip: ip}, nil
}

// Verified moves the reservation out of the unverified budget and takes
// its slot in the queue. When the queue is closed (ErrClosed) or full
// (ErrFull), it releases the reservation and fails. It is a no-op for a
// verified reservation.
func (r *Reservation) Verified() error {
	q := r.q
	q.mu.Lock()
	defer q.mu.Unlock()
	if !r.unverified {
		return nil
	}
	if q.closed {
		r.releaseLocked()
		return ErrClosed
	}
	if q.count >= q.maxCount {
		r.releaseLocked()
		return ErrFull
	}
	r.leaveUnverifiedLocked()
	r.takeSlotLocked()
	return nil
}

// leaveUnverifiedLocked takes the reservation out of the unverified
// budget, once.
func (r *Reservation) leaveUnverifiedLocked() {
	if r.unverified {
		r.q.unverified.remove(r.ip, r.bytes)
		r.unverified = false
	}
}

// UnverifiedBytes returns the bytes held by unverified reservations.
func (q *Queue) UnverifiedBytes() int64 {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.unverified.bytes
}

// UnverifiedCount returns the number of unverified reservations.
func (q *Queue) UnverifiedCount() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.unverified.count
}
