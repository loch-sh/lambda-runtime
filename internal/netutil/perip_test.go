// Copyright 2026 The Loch.sh Authors
// SPDX-License-Identifier: Apache-2.0

package netutil

import (
	"errors"
	"io"
	"net"
	"testing"
	"time"
)

// fakeListener hands out the connections pushed into it.
type fakeListener struct {
	conns chan net.Conn
}

func (f *fakeListener) Accept() (net.Conn, error) {
	c, ok := <-f.conns
	if !ok {
		return nil, net.ErrClosed
	}
	return c, nil
}
func (f *fakeListener) Close() error   { return nil }
func (f *fakeListener) Addr() net.Addr { return &net.TCPAddr{} }

// fakeConn has a chosen remote address and records Close.
type fakeConn struct {
	net.Conn
	remote  string
	closed  bool
	written []byte
}

func (c *fakeConn) Write(b []byte) (int, error) {
	c.written = append(c.written, b...)
	return len(b), nil
}
func (c *fakeConn) SetWriteDeadline(time.Time) error { return nil }

func (c *fakeConn) RemoteAddr() net.Addr {
	a, _ := net.ResolveTCPAddr("tcp", c.remote)
	return a
}
func (c *fakeConn) Close() error { c.closed = true; return nil }

func TestPerIPLimit(t *testing.T) {
	fl := &fakeListener{conns: make(chan net.Conn, 10)}
	l := PerIPLimitListener(fl, 2, []byte("no"))
	a1, a2, a3 := &fakeConn{remote: "10.0.0.5:1"}, &fakeConn{remote: "10.0.0.5:2"}, &fakeConn{remote: "10.0.0.5:3"}
	b1 := &fakeConn{remote: "10.0.0.6:1"}
	for _, c := range []net.Conn{a1, a2, a3, b1} {
		fl.conns <- c
	}
	got1, _ := l.Accept()
	got2, _ := l.Accept()
	// The third connection of 10.0.0.5 is closed; the next accepted is b1.
	got3, _ := l.Accept()
	if !a3.closed || string(a3.written) != "no" || got3.RemoteAddr().String() != "10.0.0.6:1" {
		t.Fatalf("a3 closed=%v, accepted %s", a3.closed, got3.RemoteAddr())
	}
	// Closing one (twice) frees one slot for 10.0.0.5.
	got1.Close()
	got1.Close()
	a4 := &fakeConn{remote: "10.0.0.5:4"}
	fl.conns <- a4
	if c, _ := l.Accept(); c.RemoteAddr().String() != "10.0.0.5:4" || a4.closed {
		t.Errorf("slot not freed")
	}
	_ = got2
}

func TestPerIPLoopbackExempt(t *testing.T) {
	fl := &fakeListener{conns: make(chan net.Conn, 10)}
	l := PerIPLimitListener(fl, 1, nil)
	for i := range 5 {
		c := &fakeConn{remote: "127.0.0.1:" + string(rune('1'+i))}
		fl.conns <- c
		if got, err := l.Accept(); err != nil || got.(*fakeConn).closed {
			t.Fatalf("loopback connection %d refused", i)
		}
	}
	close(fl.conns)
	if _, err := l.Accept(); !errors.Is(err, net.ErrClosed) {
		t.Errorf("err = %v", err)
	}
}

func TestPerIPRealListener(t *testing.T) {
	inner, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	l := PerIPLimitListener(inner, 1, nil)
	defer l.Close()
	go func() {
		c, err := net.Dial("tcp", inner.Addr().String())
		if err == nil {
			time.Sleep(50 * time.Millisecond)
			c.Close()
		}
	}()
	c, err := l.Accept()
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, c)
	c.Close()
}
