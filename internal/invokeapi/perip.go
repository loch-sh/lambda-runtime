// Copyright 2026 The Loch.sh Authors
// SPDX-License-Identifier: Apache-2.0

package invokeapi

import (
	"net/netip"
	"sync"
)

// ipLimiter caps the requests in flight (being read, queued or running)
// per source IP, so that one caller cannot take the whole queue from the
// others. Loopback callers (inside the container) are exempt.
type ipLimiter struct {
	max int

	mu       sync.Mutex
	inFlight map[netip.Addr]int
}

func newIPLimiter(max int) *ipLimiter {
	return &ipLimiter{max: max, inFlight: map[netip.Addr]int{}}
}

// isLoopback reports whether remoteAddr ("ip:port") is a loopback address.
func isLoopback(remoteAddr string) bool {
	ap, err := netip.ParseAddrPort(remoteAddr)
	return err == nil && ap.Addr().Unmap().IsLoopback()
}

// acquire takes a slot for remoteAddr ("ip:port"); release gives it back.
// It reports false when the IP is at its cap. A nil limiter or an
// unparsable address never limits.
func (l *ipLimiter) acquire(remoteAddr string) (release func(), ok bool) {
	ap, err := netip.ParseAddrPort(remoteAddr)
	if l == nil || l.max <= 0 || err != nil || isLoopback(remoteAddr) {
		return func() {}, true
	}
	ip := ap.Addr().Unmap()
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.inFlight[ip] >= l.max {
		return nil, false
	}
	l.inFlight[ip]++
	return func() {
		l.mu.Lock()
		defer l.mu.Unlock()
		if l.inFlight[ip]--; l.inFlight[ip] <= 0 {
			delete(l.inFlight, ip)
		}
	}, true
}

// remoteIP returns the IP of remoteAddr ("ip:port"), the whole string when
// it does not parse.
func remoteIP(remoteAddr string) string {
	if ap, err := netip.ParseAddrPort(remoteAddr); err == nil {
		return ap.Addr().Unmap().String()
	}
	return remoteAddr
}
