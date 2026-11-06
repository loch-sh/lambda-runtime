// Copyright 2026 The Loch.sh Authors
// SPDX-License-Identifier: Apache-2.0

// Package cgroup reads the memory accounting of the container's own
// cgroup v2 (mounted read-only at /sys/fs/cgroup). Every reader degrades
// gracefully: a missing file yields ok=false, never an error to handle.
package cgroup

import (
	"bufio"
	"bytes"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// DefaultRoot is where Docker mounts the container's cgroup.
const DefaultRoot = "/sys/fs/cgroup"

// Memory reads the memory files of one cgroup directory.
type Memory struct{ root string }

// NewMemory returns a reader of the cgroup at root.
func NewMemory(root string) Memory { return Memory{root: root} }

// PeakBytes returns memory.peak, the highest usage since the cgroup was
// created (kernel 5.19+): the container's lifetime peak, init and earlier
// invocations included. The cgroup is mounted read-only, so it cannot be
// reset per invocation.
func (m Memory) PeakBytes() (int64, bool) {
	b, err := os.ReadFile(filepath.Join(m.root, "memory.peak"))
	if err != nil {
		return 0, false
	}
	n, err := strconv.ParseInt(string(bytes.TrimSpace(b)), 10, 64)
	return n, err == nil
}

// OOMKills returns the oom_kill counter of memory.events: how many
// processes the kernel killed for lack of memory in this cgroup.
func (m Memory) OOMKills() (int64, bool) {
	f, err := os.Open(filepath.Join(m.root, "memory.events"))
	if err != nil {
		return 0, false
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		key, value, ok := bytes.Cut(sc.Bytes(), []byte(" "))
		if ok && string(key) == "oom_kill" {
			n, err := strconv.ParseInt(string(bytes.TrimSpace(value)), 10, 64)
			return n, err == nil
		}
	}
	return 0, false
}

// MB converts bytes to megabytes (MiB), rounded up, as Lambda reports them.
func MB(n int64) int64 { return (n + 1<<20 - 1) >> 20 }

// SelfCgroupFile lists the cgroups of the process.
const SelfCgroupFile = "/proc/self/cgroup"

// Resolve returns the directory of our own cgroup v2: root joined with the
// path of the "0::<path>" line of selfFile. With a private cgroup
// namespace (Docker's default) that path is "/" and root is the answer;
// with cgroupns=host or nested cgroups it is a subdirectory. It falls back
// to root when the line is missing or the directory does not exist.
func Resolve(root, selfFile string) string {
	b, err := os.ReadFile(selfFile)
	if err != nil {
		return root
	}
	for line := range strings.SplitSeq(string(b), "\n") {
		path, ok := strings.CutPrefix(line, "0::")
		if !ok {
			continue
		}
		dir := filepath.Join(root, filepath.Clean("/"+path))
		if info, err := os.Stat(dir); err == nil && info.IsDir() {
			return dir
		}
		return root
	}
	return root
}

// Available reports whether the memory accounting files can be read.
func (m Memory) Available() bool {
	_, peak := m.PeakBytes()
	_, events := m.OOMKills()
	return peak && events
}
