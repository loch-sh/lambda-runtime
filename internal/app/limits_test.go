// Copyright 2026 The Loch.sh Authors
// SPDX-License-Identifier: Apache-2.0

package app

import (
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
