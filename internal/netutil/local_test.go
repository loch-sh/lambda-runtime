// Copyright 2026 The Loch.sh Authors
// SPDX-License-Identifier: Apache-2.0

package netutil

import "testing"

func TestLocalAddr(t *testing.T) {
	for in, want := range map[string]string{
		"0.0.0.0:8080":  "127.0.0.1:8080",
		":8080":         "127.0.0.1:8080",
		"[::]:8080":     "127.0.0.1:8080",
		"10.0.0.2:8080": "10.0.0.2:8080",
		"garbage":       "garbage",
	} {
		if got := LocalAddr(in); got != want {
			t.Errorf("LocalAddr(%q) = %q, want %q", in, got, want)
		}
	}
}
