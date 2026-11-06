// Copyright 2026 The Loch.sh Authors
// SPDX-License-Identifier: Apache-2.0

// Package logs relays the output of the Lambda runtime to the container's
// output, writes the Lambda platform lines (START, END, REPORT), and
// captures the logs of the running invocation for X-Amz-Log-Result.
package logs

import (
	"io"
	"os"
	"sync"
	"sync/atomic"
	"time"
)

// TailSize is how much of an invocation's logs X-Amz-Log-Result carries.
const TailSize = 4096

// readBufferSize is the chunk size of the relay: output is forwarded as
// soon as it is read, without line buffering.
const readBufferSize = 32 << 10

// Relay forwards the child's stdout and stderr to ours and captures them
// while an invocation runs. It is safe for concurrent use.
//
// Writes to the container output happen under the relay's lock: if the
// log driver applied backpressure, the engine would wait on it too. Docker
// drains container output continuously; bound its volume with the log
// driver options (max-size, max-file).
type Relay struct {
	mu      sync.Mutex
	stdout  *stream
	stderr  *stream
	capture *tail // nil when no invocation is captured

	pipesMu sync.Mutex
	pipes   map[*pipe]struct{}
}

// stream is one output of the container. midLine tracks whether it ends
// mid-line, so that our own lines always start on a fresh one.
type stream struct {
	w       io.Writer
	midLine bool
}

func (s *stream) write(p []byte) {
	if len(p) == 0 {
		return
	}
	_, _ = s.w.Write(p)
	s.midLine = p[len(p)-1] != '\n'
}

// writeLine writes line on a line of its own.
func (s *stream) writeLine(line []byte) {
	if s.midLine {
		s.write([]byte{'\n'})
	}
	s.write(line)
}

// NewRelay returns a relay writing to stdout and stderr.
func NewRelay(stdout, stderr io.Writer) *Relay {
	return &Relay{
		stdout: &stream{w: stdout},
		stderr: &stream{w: stderr},
		pipes:  map[*pipe]struct{}{},
	}
}

// Pipes creates the stdout and stderr pipes of a new child and starts
// relaying them. The caller passes the write ends to the child and closes
// them on its side once the child is started.
func (r *Relay) Pipes() (stdout, stderr *os.File, err error) {
	outR, outW, err := os.Pipe()
	if err != nil {
		return nil, nil, err
	}
	errR, errW, err := os.Pipe()
	if err != nil {
		outR.Close()
		outW.Close()
		return nil, nil, err
	}
	r.relay(outR, r.stdout)
	r.relay(errR, r.stderr)
	return outW, errW, nil
}

// pipe is the read end of a child's output, with what Drain needs to know.
type pipe struct {
	f *os.File
	// reading is set while the relay is blocked reading it.
	reading atomic.Bool
	// reads counts the reads that returned, to notice one that returned
	// between two checks of Drain.
	reads atomic.Uint64
}

func (r *Relay) relay(f *os.File, dst *stream) {
	p := &pipe{f: f}
	r.pipesMu.Lock()
	r.pipes[p] = struct{}{}
	r.pipesMu.Unlock()
	go func() {
		defer func() {
			r.pipesMu.Lock()
			delete(r.pipes, p)
			r.pipesMu.Unlock()
			f.Close()
		}()
		buf := make([]byte, readBufferSize)
		for {
			p.reading.Store(true)
			n, err := f.Read(buf)
			p.reading.Store(false)
			p.reads.Add(1)
			if n > 0 {
				r.mu.Lock()
				dst.write(buf[:n])
				if r.capture != nil {
					r.capture.write(buf[:n])
				}
				r.mu.Unlock()
			}
			if err != nil {
				return
			}
		}
	}()
}

// Writer returns a writer for our own logs on dst (Stderr or Stdout):
// they are serialized with the child's output and start on a fresh line,
// but are never captured in an invocation's tail.
func (r *Relay) Writer(stderr bool) io.Writer {
	dst := r.stdout
	if stderr {
		dst = r.stderr
	}
	return writerFunc(func(p []byte) (int, error) {
		r.mu.Lock()
		defer r.mu.Unlock()
		dst.writeLine(p)
		return len(p), nil
	})
}

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }

// StartCapture starts capturing the output for an invocation, beginning
// with the platform line start.
func (r *Relay) StartCapture(start string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.capture = newTail(TailSize)
	r.platformLocked(start)
}

// Platform writes a platform line (END, REPORT, …) to stdout, and to the
// capture when one is running.
func (r *Relay) Platform(line string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.platformLocked(line)
}

func (r *Relay) platformLocked(line string) {
	b := []byte(line + "\n")
	r.stdout.writeLine(b)
	if r.capture != nil {
		r.capture.writeLine(b)
	}
}

// StopCapture ends the capture and returns its last TailSize bytes.
func (r *Relay) StopCapture() []byte {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.capture == nil {
		return nil
	}
	out := r.capture.bytes()
	r.capture = nil
	return out
}

// Drain waits, at most max, until the output the child already wrote is
// relayed: a runtime prints its logs before it posts the response, but
// the relay may not have read them yet when the response arrives. The
// pipes must look idle twice, drainSettle apart, with no read returning in
// between: this closes the window where a read has returned but its reader
// has not flagged it yet. It is best effort and returns at once where pipe
// sizes cannot be queried.
func (r *Relay) Drain(max time.Duration) {
	deadline := time.Now().Add(max)
	for time.Now().Before(deadline) {
		before, ok := r.idle()
		if ok {
			time.Sleep(drainSettle)
			if after, ok := r.idle(); ok && after == before {
				break
			}
			continue
		}
		time.Sleep(drainSettle)
	}
	// Let a reader that just got data finish forwarding it.
	r.mu.Lock()
	r.mu.Unlock() // empty critical section: a barrier
}

// drainSettle separates the two idle checks of Drain.
const drainSettle = 200 * time.Microsecond

// idle reports whether every pipe is empty and its reader waiting, with
// the total of the reads that returned so far.
func (r *Relay) idle() (uint64, bool) {
	r.pipesMu.Lock()
	defer r.pipesMu.Unlock()
	var reads uint64
	for p := range r.pipes {
		reads += p.reads.Load()
		n, ok := pending(p.f)
		if !ok {
			continue
		}
		if n > 0 || !p.reading.Load() {
			return 0, false
		}
	}
	return reads, true
}
