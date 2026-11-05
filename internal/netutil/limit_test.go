// Copyright 2026 The Loch.sh Authors
// SPDX-License-Identifier: Apache-2.0

package netutil

import (
	"errors"
	"net"
	"testing"
	"time"
)

func TestLimitListener(t *testing.T) {
	inner, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	l := LimitListener(inner, 2)
	accepted := make(chan net.Conn, 3)
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				close(accepted)
				return
			}
			accepted <- c
		}
	}()
	var clients []net.Conn
	for range 3 {
		c, err := net.Dial("tcp", inner.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		clients = append(clients, c)
	}
	first := <-accepted
	<-accepted
	select {
	case <-accepted:
		t.Fatal("third connection accepted while two are open")
	case <-time.After(100 * time.Millisecond):
	}
	// Closing one (twice: idempotent) frees exactly one slot.
	first.Close()
	first.Close()
	select {
	case c := <-accepted:
		c.Close()
	case <-time.After(5 * time.Second):
		t.Fatal("third connection not accepted after a slot was freed")
	}
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	if _, ok := <-accepted; ok {
		t.Error("accept loop must end after Close")
	}
	if _, err := l.Accept(); !errors.Is(err, net.ErrClosed) {
		t.Errorf("accept after close: %v", err)
	}
}
