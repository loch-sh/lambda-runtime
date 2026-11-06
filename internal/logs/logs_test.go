// Copyright 2026 The Loch.sh Authors
// SPDX-License-Identifier: Apache-2.0

package logs

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

// syncBuffer is a bytes.Buffer safe for the relay goroutines.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not met")
		}
		time.Sleep(time.Millisecond)
	}
}

func TestRelayAndCapture(t *testing.T) {
	var out, errOut syncBuffer
	r := NewRelay(&out, &errOut)
	w, ew, err := r.Pipes()
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	defer ew.Close()

	fmt.Fprint(w, "before\n")
	waitFor(t, func() bool { return out.String() == "before\n" })

	r.StartCapture("START RequestId: a Version: $LATEST")
	// stdout and stderr are separate pipes: write one after the other so
	// that their order in the capture is deterministic.
	fmt.Fprint(ew, "oops\n")
	waitFor(t, func() bool { return errOut.String() == "oops\n" })
	fmt.Fprint(w, "partial") // no newline: END must start on a fresh line
	r.Drain(time.Second)
	waitFor(t, func() bool { return strings.Contains(out.String(), "partial") })
	r.Platform("END RequestId: a")
	tail := string(r.StopCapture())

	if want := "START RequestId: a Version: $LATEST\noops\npartial\nEND RequestId: a\n"; tail != want {
		t.Errorf("tail = %q, want %q", tail, want)
	}
	if strings.Contains(tail, "before") {
		t.Error("output before the capture must not be in the tail")
	}
	if got := out.String(); got != "before\nSTART RequestId: a Version: $LATEST\npartial\nEND RequestId: a\n" {
		t.Errorf("stdout = %q", got)
	}
	if r.StopCapture() != nil {
		t.Error("second StopCapture must return nil")
	}
	// Outside a capture, platform lines still go to stdout.
	r.Platform("x")
	if !strings.HasSuffix(out.String(), "\nx\n") {
		t.Errorf("stdout = %q", out.String())
	}
}

func TestOwnLogsAreNotCaptured(t *testing.T) {
	var out, errOut syncBuffer
	r := NewRelay(&out, &errOut)
	r.StartCapture("START")
	fmt.Fprint(r.Writer(true), `{"level":"INFO"}`+"\n")
	fmt.Fprint(r.Writer(false), "own stdout\n")
	tail := string(r.StopCapture())
	if tail != "START\n" {
		t.Errorf("tail = %q", tail)
	}
	if errOut.String() != `{"level":"INFO"}`+"\n" || !strings.Contains(out.String(), "own stdout") {
		t.Errorf("outputs = %q / %q", out.String(), errOut.String())
	}
}

func TestTailKeepsLastBytes(t *testing.T) {
	tl := newTail(8)
	tl.write([]byte("abc"))
	tl.write([]byte("defgh"))
	tl.write([]byte("ij"))
	if got := string(tl.bytes()); got != "cdefghij" {
		t.Errorf("tail = %q", got)
	}
	tl.write([]byte("0123456789"))
	if got := string(tl.bytes()); got != "23456789" {
		t.Errorf("tail = %q", got)
	}
	tl.write(nil)
	for i := range 100 {
		tl.write([]byte{byte('a' + i%26)})
	}
	if got := tl.bytes(); len(got) != 8 {
		t.Errorf("len = %d", len(got))
	}
}

func TestPipeEndsAtEOF(t *testing.T) {
	r := NewRelay(&syncBuffer{}, &syncBuffer{})
	w, ew, err := r.Pipes()
	if err != nil {
		t.Fatal(err)
	}
	w.Close()
	ew.Close()
	waitFor(t, func() bool {
		r.pipesMu.Lock()
		defer r.pipesMu.Unlock()
		return len(r.pipes) == 0
	})
	r.Drain(time.Millisecond)
}

var fixed = func() time.Time { return time.Date(2026, 11, 6, 10, 0, 0, 0, time.UTC) }

func TestTextPlatformLines(t *testing.T) {
	p := Platform{Version: "$LATEST", MemoryMB: 256, Now: fixed}
	if got := p.Start("id"); got != "START RequestId: id Version: $LATEST" {
		t.Errorf("start = %q", got)
	}
	if got := p.End("id", StatusSuccess); got != "END RequestId: id" {
		t.Errorf("end = %q", got)
	}
	if got := p.Timeout("id", 3*time.Second); got != "2026-11-06T10:00:00.000Z id Task timed out after 3.00 seconds" {
		t.Errorf("timeout = %q", got)
	}
	cases := []struct {
		r    Report
		want string
	}{
		{Report{RequestID: "id", Duration: 2130 * time.Microsecond, MaxMemoryUsedMB: 67, Status: StatusSuccess},
			"REPORT RequestId: id\tDuration: 2.13 ms\tBilled Duration: 3 ms\tMemory Size: 256 MB\tMax Memory Used: 67 MB\t"},
		{Report{RequestID: "id", Duration: 100 * time.Microsecond, InitDuration: 180240 * time.Microsecond, Status: StatusSuccess},
			"REPORT RequestId: id\tDuration: 0.10 ms\tBilled Duration: 1 ms\tMemory Size: 256 MB\tInit Duration: 180.24 ms\t"},
		{Report{RequestID: "id", Duration: 3 * time.Second, MaxMemoryUsedMB: 1, Status: StatusTimeout},
			"REPORT RequestId: id\tDuration: 3000.00 ms\tBilled Duration: 3000 ms\tMemory Size: 256 MB\tMax Memory Used: 1 MB\tStatus: timeout\t"},
		{Report{RequestID: "id", Duration: time.Millisecond, Status: StatusError, ErrorType: "Runtime.ExitError"},
			"REPORT RequestId: id\tDuration: 1.00 ms\tBilled Duration: 1 ms\tMemory Size: 256 MB\tStatus: error\tError Type: Runtime.ExitError\t"},
	}
	for _, c := range cases {
		if got := p.Report(c.r); got != c.want {
			t.Errorf("report =\n%q\nwant\n%q", got, c.want)
		}
	}
}

func TestJSONPlatformLines(t *testing.T) {
	p := Platform{JSON: true, Version: "$LATEST", MemoryMB: 128, Now: fixed}
	type rec struct {
		Time   string         `json:"time"`
		Type   string         `json:"type"`
		Record map[string]any `json:"record"`
	}
	parse := func(s string) rec {
		var r rec
		if err := json.Unmarshal([]byte(s), &r); err != nil {
			t.Fatalf("%q: %v", s, err)
		}
		return r
	}
	if r := parse(p.Start("id")); r.Type != "platform.start" || r.Record["requestId"] != "id" || r.Record["version"] != "$LATEST" || r.Time != "2026-11-06T10:00:00Z" {
		t.Errorf("start = %+v", r)
	}
	if r := parse(p.End("id", StatusTimeout)); r.Type != "platform.runtimeDone" || r.Record["status"] != "timeout" {
		t.Errorf("end = %+v", r)
	}
	if p.Timeout("id", time.Second) != "" {
		t.Error("no timeout line in JSON")
	}
	r := parse(p.Report(Report{RequestID: "id", Duration: 2131 * time.Microsecond, MaxMemoryUsedMB: 50,
		InitDuration: time.Millisecond, Status: StatusError, ErrorType: "E"}))
	m := r.Record["metrics"].(map[string]any)
	if r.Type != "platform.report" || r.Record["errorType"] != "E" || m["durationMs"] != 2.13 || m["billedDurationMs"] != 3.0 ||
		m["memorySizeMB"] != 128.0 || m["maxMemoryUsedMB"] != 50.0 || m["initDurationMs"] != 1.0 {
		t.Errorf("report = %+v", r)
	}
	if p := (Platform{}); p.now().IsZero() {
		t.Error("default clock")
	}
}
