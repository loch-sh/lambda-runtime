// Copyright 2026 The Loch.sh Authors
// SPDX-License-Identifier: Apache-2.0

package netutil

import (
	"net"
	"net/netip"
	"sync"
	"time"
)

// PerIPLimitListener returns a listener that refuses the connections of a
// source IP that already has n open ones, so that one client cannot take
// all the slots of the global cap: refused gets written on the connection
// (without reading the request), then it is closed. Loopback clients are
// exempt: they are inside the container (`docker exec … invoke`, the
// healthcheck), and still bound by the global cap.
// onRefuse, when not nil, is called for each refused connection (metrics).
func PerIPLimitListener(l net.Listener, n int, refused []byte, onRefuse func()) net.Listener {
	return &perIPListener{Listener: l, max: n, refused: refused, onRefuse: onRefuse, open: map[netip.Addr]int{}}
}

// refuseWriteTimeout bounds the write of the refusal, which runs in the
// accept loop.
const refuseWriteTimeout = 100 * time.Millisecond

type perIPListener struct {
	net.Listener
	max      int
	refused  []byte
	onRefuse func()

	mu   sync.Mutex
	open map[netip.Addr]int
}

func (l *perIPListener) Accept() (net.Conn, error) {
	for {
		c, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}
		ip, ok := remoteIP(c)
		if !ok || ip.IsLoopback() {
			return c, nil
		}
		if l.acquire(ip) {
			return &perIPConn{Conn: c, release: func() { l.release(ip) }}, nil
		}
		l.refuse(c)
	}
}

func (l *perIPListener) refuse(c net.Conn) {
	if l.onRefuse != nil {
		l.onRefuse()
	}
	if len(l.refused) > 0 {
		_ = c.SetWriteDeadline(time.Now().Add(refuseWriteTimeout))
		_, _ = c.Write(l.refused)
	}
	_ = c.Close()
}

func (l *perIPListener) acquire(ip netip.Addr) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.open[ip] >= l.max {
		return false
	}
	l.open[ip]++
	return true
}

func (l *perIPListener) release(ip netip.Addr) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.open[ip]--; l.open[ip] <= 0 {
		delete(l.open, ip)
	}
}

func remoteIP(c net.Conn) (netip.Addr, bool) {
	ap, err := netip.ParseAddrPort(c.RemoteAddr().String())
	if err != nil {
		return netip.Addr{}, false
	}
	return ap.Addr().Unmap(), true
}

type perIPConn struct {
	net.Conn
	once    sync.Once
	release func()
}

func (c *perIPConn) Close() error {
	err := c.Conn.Close()
	c.once.Do(c.release)
	return err
}
