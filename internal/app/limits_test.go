// Copyright 2026 The Loch.sh Authors
// SPDX-License-Identifier: Apache-2.0

package app

import (
	"bytes"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/loch-sh/lambda-runtime/internal/config"
	"github.com/loch-sh/lambda-runtime/internal/queue"
)

func TestUnverifiedLimits(t *testing.T) {
	cases := []struct {
		rt   config.Runtime
		want queue.UnverifiedLimits
	}{
		// Defaults: 16 requests, 32 MiB, 6 MiB bodies.
		{config.Runtime{QueueSize: 16, QueueBytes: 32 << 20, MaxPayload: 6 << 20},
			queue.UnverifiedLimits{Bytes: 8 << 20, PerIP: 6 << 20, Count: 4, CountPerIP: 1}},
		// A larger queue lets an IP behind a NAT upload several at once.
		{config.Runtime{QueueSize: 64, QueueBytes: 512 << 20, MaxPayload: 6 << 20},
			queue.UnverifiedLimits{Bytes: 128 << 20, PerIP: 6 << 20, Count: 16, CountPerIP: 4}},
		// A tiny queue still admits one, of one maximum body.
		{config.Runtime{QueueSize: 1, QueueBytes: 6 << 20, MaxPayload: 6 << 20},
			queue.UnverifiedLimits{Bytes: 6 << 20, PerIP: 6 << 20, Count: 1, CountPerIP: 1}},
	}
	for _, c := range cases {
		if got := unverifiedLimits(c.rt); got != c.want {
			t.Errorf("%+v: got %+v, want %+v", c.rt, got, c.want)
		}
	}
}

func TestConnsPerIP(t *testing.T) {
	for maxConns, want := range map[int]int{64: 8, 768: 96, 1000: 125, 32: 4, 16: 4, 2: 2, 1: 1} {
		if got := connsPerIP(maxConns); got != want {
			t.Errorf("connsPerIP(%d) = %d, want %d", maxConns, got, want)
		}
	}
}

// TestOneIPCanFillTheQueue links config.DefaultMaxConns and connsPerIP:
// with the default connection cap, one source IP can hold queue size + 1
// requests in flight, until the cap of 768.
func TestOneIPCanFillTheQueue(t *testing.T) {
	for q := 0; config.ConnsPerIPShare*(q+1) <= config.DefaultMaxConns(1<<20); q++ {
		if got := connsPerIP(config.DefaultMaxConns(q)); got < q+1 {
			t.Errorf("queue size %d: %d connections per IP", q, got)
		}
	}
	// The default cap leaves the margin under 1024 open files.
	if got := config.DefaultMaxConns(1<<20) + config.FileMargin; got > 1024 {
		t.Errorf("default cap + margin = %d", got)
	}
}

func TestWarnLowFileLimit(t *testing.T) {
	const maxConns = 136
	for _, c := range []struct {
		name  string
		limit func() (uint64, error)
		warn  string // "" when nothing is logged
	}{
		{"enough", func() (uint64, error) { return 1024, nil }, ""},
		{"exactly enough", func() (uint64, error) { return maxConns + config.FileMargin, nil }, ""},
		{"too low", func() (uint64, error) { return maxConns + config.FileMargin - 1, nil }, "open files limit is low"},
		{"unreadable", func() (uint64, error) { return 0, errors.New("no") }, "cannot read"},
	} {
		var out bytes.Buffer
		warnLowFileLimit(maxConns, c.limit, slog.New(slog.NewTextHandler(&out, nil)))
		if got := out.String(); (c.warn == "") != (got == "") || !strings.Contains(got, c.warn) {
			t.Errorf("%s: logged %q", c.name, got)
		}
	}
	if n, err := openFilesLimit(); err != nil || n == 0 {
		t.Errorf("openFilesLimit = %d, %v", n, err)
	}
}
