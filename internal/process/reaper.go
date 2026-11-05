// Copyright 2026 The Loch.sh Authors
// SPDX-License-Identifier: Apache-2.0

//go:build unix

package process

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"runtime"
	"sync"
	"syscall"
)

// ErrClosed is returned by Start after Close.
var ErrClosed = errors.New("process: reaper closed")

// Reaper starts children and reaps every terminated process.
type Reaper struct {
	log       *slog.Logger
	signals   chan os.Signal
	stop      chan struct{}
	stopped   chan struct{}
	closeOnce sync.Once

	mu       sync.Mutex
	closed   bool
	children map[int]*Child
	// orphans counts reaped processes that were not started by Start.
	orphans int
}

// NewReaper starts reaping. It makes the process a child subreaper when it
// is not PID 1 (Linux only), so that orphans are still reaped here.
func NewReaper(log *slog.Logger) *Reaper {
	r := &Reaper{
		log:      log,
		children: map[int]*Child{},
		signals:  make(chan os.Signal, 1),
		stop:     make(chan struct{}),
		stopped:  make(chan struct{}),
	}
	if os.Getpid() != 1 {
		if err := becomeSubreaper(); err != nil {
			log.Warn("process: cannot become a child subreaper; orphans will not be reaped here", "error", err)
		}
	}
	signal.Notify(r.signals, syscall.SIGCHLD)
	go r.loop()
	return r
}

// Close stops reaping after a last pass; Start fails afterwards. It is
// safe to call several times.
func (r *Reaper) Close() {
	r.closeOnce.Do(func() {
		signal.Stop(r.signals)
		close(r.stop)
		<-r.stopped
		r.mu.Lock()
		r.closed = true
		r.mu.Unlock()
		r.reapAll()
	})
}

// Orphans returns the number of reaped processes not started by Start.
func (r *Reaper) Orphans() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.orphans
}

// Start starts a child in a new process group.
func (r *Reaper) Start(spec Spec) (*Child, error) {
	path, err := resolve(spec.Args, spec.Dir)
	if err != nil {
		return nil, err
	}
	devNull, err := os.Open(os.DevNull)
	if err != nil {
		return nil, err
	}
	defer devNull.Close()
	files := []uintptr{devNull.Fd(), fdOr(spec.Stdout, devNull), fdOr(spec.Stderr, devNull)}

	// The lock keeps the reaper from collecting the child before it is
	// registered.
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return nil, ErrClosed
	}
	pid, err := syscall.ForkExec(path, spec.Args, &syscall.ProcAttr{
		Env:   spec.Env,
		Dir:   spec.Dir,
		Files: files,
		Sys:   &syscall.SysProcAttr{Setpgid: true},
	})
	// The descriptors were passed as integers: keep their files alive.
	runtime.KeepAlive(spec.Stdout)
	runtime.KeepAlive(spec.Stderr)
	if err != nil {
		return nil, fmt.Errorf("start %s: %w", spec.Args[0], err)
	}
	if spec.OOMScoreAdj != 0 {
		if err := setOOMScoreAdj(pid, spec.OOMScoreAdj); err != nil {
			r.log.Warn("process: oom_score_adj not set", "pid", pid, "error", err)
		}
	}
	c := &Child{pid: pid, done: make(chan ExitStatus, 1)}
	r.children[pid] = c
	return c, nil
}

func fdOr(f, def *os.File) uintptr {
	if f == nil {
		return def.Fd()
	}
	return f.Fd()
}

func (r *Reaper) loop() {
	defer close(r.stopped)
	for {
		select {
		case <-r.stop:
			return
		case <-r.signals:
			r.reapAll()
		}
	}
}

// reapAll collects every terminated process without blocking; one SIGCHLD
// may stand for several exits.
func (r *Reaper) reapAll() {
	r.mu.Lock()
	defer r.mu.Unlock()
	for {
		var ws syscall.WaitStatus
		pid, err := syscall.Wait4(-1, &ws, syscall.WNOHANG, nil)
		if errors.Is(err, syscall.EINTR) {
			continue
		}
		if err != nil || pid <= 0 {
			return
		}
		status := statusOf(ws)
		c, ok := r.children[pid]
		if !ok {
			r.orphans++
			r.log.Debug("process: orphan reaped", "pid", pid, "status", status.String())
			continue
		}
		delete(r.children, pid)
		c.done <- status
		close(c.done)
	}
}
