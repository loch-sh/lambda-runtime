// Copyright 2026 The Loch.sh Authors
// SPDX-License-Identifier: Apache-2.0

//go:build unix

// Package process starts the Lambda runtime as a child process and carries
// out the duties of PID 1: every child is put in its own process group so
// that it can be killed with all its descendants, and every terminated
// process, orphans included, is reaped.
//
// The Reaper owns all the children of the process: os/exec must not be
// used alongside it, because the Reaper collects exit statuses with
// wait4(-1).
package process

import (
	"errors"
	"fmt"
	"os"
	"syscall"
)

// Spec describes a child to start.
type Spec struct {
	// Args is the command. Args[0] is resolved with PATH when it has no
	// slash, and against Dir when it is a relative path such as ./main.
	Args []string
	Env  []string
	// Dir is the working directory; "" keeps ours.
	Dir string
	// Stdout and Stderr receive the output of the child; nil means
	// /dev/null.
	Stdout, Stderr *os.File
	// OOMScoreAdj, when non-zero, is written to the child's oom_score_adj
	// right after it starts (Linux only), so that the kernel OOM-kills it
	// rather than PID 1. Processes it forks before the write keep the
	// previous value; a bootstrap execs, it does not fork, so this window
	// does not matter in practice.
	OOMScoreAdj int
}

// ExitStatus is how a child ended.
type ExitStatus struct {
	Code   int
	Signal syscall.Signal
	// Signaled is true when the child was killed by Signal.
	Signaled bool
}

// String formats the status like os/exec: "exit status 1",
// "signal: killed".
func (s ExitStatus) String() string {
	if s.Signaled {
		return "signal: " + s.Signal.String()
	}
	return fmt.Sprintf("exit status %d", s.Code)
}

func statusOf(ws syscall.WaitStatus) ExitStatus {
	if ws.Signaled() {
		return ExitStatus{Signal: ws.Signal(), Signaled: true, Code: -1}
	}
	return ExitStatus{Code: ws.ExitStatus()}
}

// Child is a started process, leader of its own process group.
type Child struct {
	pid  int
	done chan ExitStatus
}

// Pid returns the process id, which is also the process group id.
func (c *Child) Pid() int { return c.pid }

// Done delivers the exit status once the child was reaped, then is closed.
func (c *Child) Done() <-chan ExitStatus { return c.done }

// SignalGroup sends sig to the whole process group of the child,
// descendants included, even after the leader was reaped. A group that no
// longer exists is not an error. It must not be called once a newer child
// was started: the group id is the leader's pid, which the kernel may
// reuse after the whole group is gone.
func (c *Child) SignalGroup(sig syscall.Signal) error {
	err := syscall.Kill(-c.pid, sig)
	if errors.Is(err, syscall.ESRCH) {
		return nil
	}
	return err
}

// Harden makes this process non-dumpable (Linux), so that a child running
// under the same uid can neither read /proc/1/environ nor /proc/1/mem.
func Harden() error { return disableDumping() }
