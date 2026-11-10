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
	refusedCount := 0
	fl := &fakeListener{conns: make(chan net.Conn, 10)}
	l := PerIPLimitListener(fl, 2, []byte("no"), func() { refusedCount++ })
	a1, a2, a3 := &fakeConn{remote: "10.0.0.5:1"}, &fakeConn{remote: "10.0.0.5:2"}, &fakeConn{remote: "10.0.0.5:3"}
	b1 := &fakeConn{remote: "10.0.0.6:1"}
	for _, c := range []net.Conn{a1, a2, a3, b1} {
		fl.conns <- c
	}
	got1, _ := l.Accept()
	got2, _ := l.Accept()
	// The third connection of 10.0.0.5 is closed; the next accepted is b1.
	got3, _ := l.Accept()
	if !a3.closed || string(a3.written) != "no" || refusedCount != 1 || got3.RemoteAddr().String() != "10.0.0.6:1" {
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
	l := PerIPLimitListener(fl, 1, nil, nil)
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
	l := PerIPLimitListener(inner, 1, nil, nil)
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

// TestPerIPRefusalSurvivesUnreadRequest: the refusal reaches a client that
// sent its whole request first. Closing a socket with unread data sends a
// RST, which can discard the refusal before the client reads it; refuse
// half-closes and drains instead.
func TestPerIPRefusalSurvivesUnreadRequest(t *testing.T) {
	inner, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer inner.Close()
	refusal := []byte("HTTP/1.1 429 Too Many Requests\r\nConnection: close\r\nContent-Length: 0\r\n\r\n")
	l := PerIPLimitListener(inner, 1, refusal, nil).(*perIPListener)
	for i := range 20 {
		client, err := net.Dial("tcp", inner.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		server, err := inner.Accept()
		if err != nil {
			t.Fatal(err)
		}
		body := make([]byte, 64<<10)
		if _, err := client.Write(append([]byte("POST / HTTP/1.1\r\nHost: x\r\nContent-Length: 65536\r\n\r\n"), body...)); err != nil {
			t.Fatal(err)
		}
		time.Sleep(10 * time.Millisecond) // the request is in the server's buffer
		l.refuse(server)
		_ = client.SetReadDeadline(time.Now().Add(5 * time.Second))
		got, err := io.ReadAll(client)
		client.Close()
		if err != nil || string(got) != string(refusal) {
			t.Fatalf("attempt %d: got %q, %v", i, got, err)
		}
	}
}

// TestPerIPRefusalLingeringBounded: beyond maxLingering draining refusals,
// a refused connection is closed at once.
func TestPerIPRefusalLingeringBounded(t *testing.T) {
	inner, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer inner.Close()
	l := PerIPLimitListener(inner, 1, []byte("no"), nil).(*perIPListener)
	for range maxLingering {
		l.lingering <- struct{}{}
	}
	client, err := net.Dial("tcp", inner.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	server, err := inner.Accept()
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	l.refuse(server)
	_ = server.SetReadDeadline(time.Now().Add(time.Second)) // fails on an open conn
	if _, err := server.Read(make([]byte, 1)); !errors.Is(err, net.ErrClosed) {
		t.Errorf("the refused connection is not closed: %v", err)
	}
	if time.Since(start) > refuseLinger/2 {
		t.Error("the refusal waited although no lingering slot was free")
	}
}

// TestPerIPRefusalLingerEnds: a client that neither sends nor closes is
// dropped after refuseLinger, and its lingering slot is freed.
func TestPerIPRefusalLingerEnds(t *testing.T) {
	inner, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer inner.Close()
	l := PerIPLimitListener(inner, 1, []byte("no"), nil).(*perIPListener)
	client, err := net.Dial("tcp", inner.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	server, err := inner.Accept()
	if err != nil {
		t.Fatal(err)
	}
	l.refuse(server)
	if len(l.lingering) != 1 {
		t.Fatalf("lingering = %d, want 1", len(l.lingering))
	}
	deadline := time.Now().Add(4 * refuseLinger)
	for len(l.lingering) != 0 {
		if time.Now().After(deadline) {
			t.Fatal("the lingering slot was never freed")
		}
		time.Sleep(10 * time.Millisecond)
	}
}
