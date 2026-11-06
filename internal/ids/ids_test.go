// Copyright 2026 The Loch.sh Authors
// SPDX-License-Identifier: Apache-2.0

package ids

import (
	"regexp"
	"strings"
	"testing"
	"time"
)

func TestRequestID(t *testing.T) {
	a, b := NewRequestID(), NewRequestID()
	uuid := regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	if a == b || !uuid.MatchString(a) {
		t.Errorf("ids = %s %s", a, b)
	}
}

func TestTraceID(t *testing.T) {
	id := NewTraceID(time.Unix(0x5759e988, 0))
	if !strings.HasPrefix(id, "Root=1-5759e988-") || !strings.HasSuffix(id, ";Sampled=0") || !ValidTraceID(id) {
		t.Errorf("trace = %s", id)
	}
	if id := NewTraceID(time.Unix(1, 0)); !strings.HasPrefix(id, "Root=1-00000001-") {
		t.Errorf("padded trace = %s", id)
	}
	for _, ok := range []string{
		"Root=1-5759e988-bd862e3fe1be46a994272793",
		"Root=1-5759e988-bd862e3fe1be46a994272793;Parent=53995c3f42cd8ad8;Sampled=1",
		"Root=1-5759e988-bd862e3fe1be46a994272793;Parent=53995c3f42cd8ad8;Sampled=1;Lineage=a87bd80c:1",
	} {
		if !ValidTraceID(ok) {
			t.Errorf("%q must be valid", ok)
		}
	}
	for _, bad := range []string{"", "Root=1-x", "Root=1-5759e988-bd862e3fe1be46a994272793;Evil=1", "Root=1-5759e988-bd862e3fe1be46a994272793\r\nX: y"} {
		if ValidTraceID(bad) {
			t.Errorf("%q must be invalid", bad)
		}
	}
}
