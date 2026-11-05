// Copyright 2026 The Loch.sh Authors
// SPDX-License-Identifier: Apache-2.0

//go:build unix

package process

import (
	"bytes"
	"errors"
	"strconv"
	"syscall"
	"time"
)

const (
	// maxKillRounds bounds KillDescendants against a fork bomb outrunning it.
	maxKillRounds = 10
	// killWait bounds the wait for the signalled processes to die, per round.
	killWait = time.Second
)

// procInfo is what KillDescendants needs to know about a process.
type procInfo struct {
	ppid  int
	state byte
}

// alive reports whether the process can still run: zombies (Z) and dead
// processes (X) only wait to be reaped, and kill() on them "succeeds"
// without effect.
func (p procInfo) alive() bool { return p.state != 'Z' && p.state != 'X' }

// KillDescendants SIGKILLs every live descendant of self, including those
// that left the child's process group with setsid or setpgid (orphans are
// reparented to us: we are PID 1 or a subreaper), and waits for them to
// die. It returns how many distinct processes it signalled.
//
// Processes entered with `docker exec` are spared: they are not our
// descendants (their parent is outside the PID namespace, ppid 0), so a
// concurrent `loch-lambda-runtime invoke` or healthcheck survives, unlike
// with kill(-1). A pid is signalled right after it was seen as our
// descendant; reusing it in between would need the whole pid space to
// wrap around within microseconds, so pidfds are not worth their
// complexity here. It is a no-op where /proc is not available.
func KillDescendants(self int) (int, error) {
	signalled := map[int]bool{}
	for range maxKillRounds {
		table, err := processTable()
		if err != nil {
			return len(signalled), err
		}
		live := liveDescendants(self, table)
		if len(live) == 0 {
			return len(signalled), nil
		}
		for _, pid := range live {
			if !signalled[pid] && syscall.Kill(pid, syscall.SIGKILL) == nil {
				signalled[pid] = true
			}
		}
		if err := waitGone(self); err != nil {
			return len(signalled), err
		}
	}
	return len(signalled), errors.New("process: descendants keep appearing")
}

// waitGone polls until self has no live descendant, at most killWait. New
// descendants are left to the next round.
func waitGone(self int) error {
	deadline := time.Now().Add(killWait)
	for delay := time.Millisecond; time.Now().Before(deadline); delay = min(2*delay, 50*time.Millisecond) {
		table, err := processTable()
		if err != nil {
			return err
		}
		if len(liveDescendants(self, table)) == 0 {
			return nil
		}
		time.Sleep(delay)
	}
	return nil
}

// liveDescendants returns the live pids whose ancestry reaches root, root
// excluded.
func liveDescendants(root int, table map[int]procInfo) []int {
	children := map[int][]int{}
	for pid, info := range table {
		if pid != root {
			children[info.ppid] = append(children[info.ppid], pid)
		}
	}
	var out []int
	stack := []int{root}
	seen := map[int]bool{root: true}
	for len(stack) > 0 {
		pid := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		for _, c := range children[pid] {
			if seen[c] {
				continue
			}
			seen[c] = true
			stack = append(stack, c)
			if table[c].alive() {
				out = append(out, c)
			}
		}
	}
	return out
}

// parseStat extracts the state and the parent pid from the content of
// /proc/<pid>/stat: "pid (comm) state ppid …". comm may contain spaces and
// parentheses, so the fields are read after the last ')'.
func parseStat(stat []byte) (procInfo, bool) {
	end := bytes.LastIndexByte(stat, ')')
	if end < 0 {
		return procInfo{}, false
	}
	fields := bytes.Fields(stat[end+1:])
	if len(fields) < 2 || len(fields[0]) != 1 {
		return procInfo{}, false
	}
	ppid, err := strconv.Atoi(string(fields[1]))
	if err != nil {
		return procInfo{}, false
	}
	return procInfo{ppid: ppid, state: fields[0][0]}, true
}
