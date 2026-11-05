// Copyright 2026 The Loch.sh Authors
// SPDX-License-Identifier: Apache-2.0

// Package netutil holds small networking helpers shared by the servers.
package netutil

import (
	"net"
	"sync"
)

// LimitListener returns a listener accepting at most n simultaneous
// connections: Accept blocks while n connections are open, so excess
// clients wait in the kernel backlog instead of consuming memory here.
func LimitListener(l net.Listener, n int) net.Listener {
	return &limitListener{Listener: l, slots: make(chan struct{}, n), done: make(chan struct{})}
}

type limitListener struct {
	net.Listener
	slots     chan struct{}
	done      chan struct{}
	closeOnce sync.Once
}

func (l *limitListener) Accept() (net.Conn, error) {
	select {
	case l.slots <- struct{}{}:
	case <-l.done:
		return nil, net.ErrClosed
	}
	c, err := l.Listener.Accept()
	if err != nil {
		<-l.slots
		return nil, err
	}
	return &limitConn{Conn: c, release: func() { <-l.slots }}, nil
}

func (l *limitListener) Close() error {
	err := l.Listener.Close()
	l.closeOnce.Do(func() { close(l.done) })
	return err
}

type limitConn struct {
	net.Conn
	once    sync.Once
	release func()
}

func (c *limitConn) Close() error {
	err := c.Conn.Close()
	c.once.Do(c.release)
	return err
}
