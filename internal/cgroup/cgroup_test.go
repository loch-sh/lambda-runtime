// Copyright 2026 The Loch.sh Authors
// SPDX-License-Identifier: Apache-2.0

package cgroup

import (
	"os"
	"path/filepath"
	"testing"
)

func write(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestMemory(t *testing.T) {
	dir := t.TempDir()
	m := NewMemory(dir)
	if _, ok := m.PeakBytes(); ok {
		t.Error("missing memory.peak must not be ok")
	}
	if _, ok := m.OOMKills(); ok {
		t.Error("missing memory.events must not be ok")
	}
	write(t, dir, "memory.peak", "70254592\n")
	write(t, dir, "memory.events", "low 0\nhigh 0\nmax 12\noom 2\noom_kill 1\noom_group_kill 0\n")
	if n, ok := m.PeakBytes(); !ok || n != 70254592 {
		t.Errorf("peak = %d %v", n, ok)
	}
	if n, ok := m.OOMKills(); !ok || n != 1 {
		t.Errorf("oom kills = %d %v", n, ok)
	}
	write(t, dir, "memory.events", "low 0\n")
	if _, ok := m.OOMKills(); ok {
		t.Error("memory.events without oom_kill must not be ok")
	}
	write(t, dir, "memory.peak", "garbage")
	if _, ok := m.PeakBytes(); ok {
		t.Error("garbage must not be ok")
	}
}

func TestMB(t *testing.T) {
	for in, want := range map[int64]int64{0: 0, 1: 1, 1 << 20: 1, 1<<20 + 1: 2, 70254592: 67} {
		if got := MB(in); got != want {
			t.Errorf("MB(%d) = %d, want %d", in, got, want)
		}
	}
}

func TestResolve(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "system.slice", "docker-abc.scope"), 0o755); err != nil {
		t.Fatal(err)
	}
	self := filepath.Join(t.TempDir(), "cgroup")
	cases := map[string]string{
		"0::/\n":                              root,
		"0::/system.slice/docker-abc.scope\n": filepath.Join(root, "system.slice", "docker-abc.scope"),
		"12:memory:/x\n0::/system.slice/docker-abc.scope": filepath.Join(root, "system.slice", "docker-abc.scope"),
		"0::/missing\n":      root,
		"0::/../../etc\n":    root,
		"1:name=systemd:/\n": root,
	}
	for content, want := range cases {
		write(t, filepath.Dir(self), "cgroup", content)
		if got := Resolve(root, self); got != want {
			t.Errorf("%q: %s, want %s", content, got, want)
		}
	}
	if got := Resolve(root, filepath.Join(root, "nope")); got != root {
		t.Errorf("missing file: %s", got)
	}
}

func TestAvailable(t *testing.T) {
	dir := t.TempDir()
	m := NewMemory(dir)
	if m.Available() {
		t.Error("available without files")
	}
	write(t, dir, "memory.peak", "1")
	write(t, dir, "memory.events", "oom_kill 0\n")
	if !m.Available() {
		t.Error("not available with files")
	}
}
