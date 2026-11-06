// Copyright 2026 The Loch.sh Authors
// SPDX-License-Identifier: Apache-2.0

package invokeapi

import (
	"net/netip"
	"sync"
	"time"
)

const (
	// logEvery is the shortest interval between two logs for one IP.
	logEvery = time.Minute
	// maxLoggedIPs bounds the memory of the limiter.
	maxLoggedIPs = 4096
)

// logLimiter lets one log per source IP and per minute through, so that a
// caller hammering with bad credentials cannot flood the logs. Its table
// is bounded: when it is full, the oldest entry makes room, so that many
// addresses cannot silence the logs either.
type logLimiter struct {
	mu   sync.Mutex
	last map[netip.Addr]time.Time
	now  func() time.Time
}

func newLogLimiter() *logLimiter {
	return &logLimiter{last: map[netip.Addr]time.Time{}, now: time.Now}
}

// allow reports whether a log about remoteAddr may be written now.
func (l *logLimiter) allow(remoteAddr string) bool {
	ap, err := netip.ParseAddrPort(remoteAddr)
	if err != nil {
		return false
	}
	ip := ap.Addr().Unmap()
	now := l.now()
	l.mu.Lock()
	defer l.mu.Unlock()
	if last, ok := l.last[ip]; ok && now.Sub(last) < logEvery {
		return false
	}
	if len(l.last) >= maxLoggedIPs {
		l.evictOldestLocked()
	}
	l.last[ip] = now
	return true
}

func (l *logLimiter) evictOldestLocked() {
	var oldest netip.Addr
	var oldestTime time.Time
	for ip, t := range l.last {
		if !oldest.IsValid() || t.Before(oldestTime) {
			oldest, oldestTime = ip, t
		}
	}
	delete(l.last, oldest)
}
