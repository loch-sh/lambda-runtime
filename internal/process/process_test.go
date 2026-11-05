// Copyright 2026 The Loch.sh Authors
// SPDX-License-Identifier: Apache-2.0

//go:build unix

package process

import (
	"bufio"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

var testLog = slog.New(slog.DiscardHandler)

// The test binary doubles as the child: LOCH_TEST_CHILD selects a behavior.
func TestMain(m *testing.M) {
	switch os.Getenv("LOCH_TEST_CHILD") {
	case "":
		os.Exit(m.Run())
	case "exit":
		code, _ := strconv.Atoi(os.Getenv("LOCH_TEST_CODE"))
		fmt.Println("out:" + os.Getenv("LOCH_TEST_VALUE"))
		fmt.Fprintln(os.Stderr, "err:"+strings.Join(os.Args[1:], ","))
		os.Exit(code)
	case "pwd":
		wd, _ := os.Getwd()
		fmt.Println(wd)
	case "escape":
		// A grandchild that leaves the process group, then sleeps.
		pid, err := syscall.ForkExec(os.Args[0], []string{os.Args[0]}, &syscall.ProcAttr{
			Env:   []string{"LOCH_TEST_CHILD=sleep"},
			Files: []uintptr{0, 2, 2},
			Sys:   &syscall.SysProcAttr{Setsid: true},
		})
		if err != nil {
			os.Exit(3)
		}
		fmt.Println(pid)
		time.Sleep(time.Minute)
	case "sleep":
		fmt.Println("ready")
		time.Sleep(time.Minute)
	case "family":
		// Starts a grandchild in the same process group, prints its pid,
		// then sleeps.
		pid, err := syscall.ForkExec(os.Args[0], []string{os.Args[0]}, &syscall.ProcAttr{
			Env:   []string{"LOCH_TEST_CHILD=sleep"},
			Files: []uintptr{0, 2, 2},
		})
		if err != nil {
			os.Exit(3)
		}
		fmt.Println(pid)
		time.Sleep(time.Minute)
	}
	os.Exit(0)
}

func startHelper(t *testing.T, r *Reaper, mode string, extraEnv ...string) (*Child, *bufio.Reader, *bufio.Reader) {
	t.Helper()
	outR, outW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	errR, errW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	env := append([]string{"LOCH_TEST_CHILD=" + mode}, extraEnv...)
	c, err := r.Start(Spec{Args: []string{os.Args[0], "a", "b"}, Env: env, Stdout: outW, Stderr: errW})
	outW.Close()
	errW.Close()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = c.SignalGroup(syscall.SIGKILL)
		outR.Close()
		errR.Close()
	})
	return c, bufio.NewReader(outR), bufio.NewReader(errR)
}

func waitExit(t *testing.T, c *Child) ExitStatus {
	t.Helper()
	select {
	case st := <-c.Done():
		return st
	case <-time.After(10 * time.Second):
		t.Fatal("child not reaped")
		return ExitStatus{}
	}
}

func readLine(t *testing.T, r *bufio.Reader) string {
	t.Helper()
	line, err := r.ReadString('\n')
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	return strings.TrimSpace(line)
}

func TestExitCodeOutputAndEnv(t *testing.T) {
	r := NewReaper(testLog)
	defer r.Close()
	c, out, errOut := startHelper(t, r, "exit", "LOCH_TEST_CODE=7", "LOCH_TEST_VALUE=hello")
	if got := readLine(t, out); got != "out:hello" {
		t.Errorf("stdout = %q", got)
	}
	if got := readLine(t, errOut); got != "err:a,b" {
		t.Errorf("stderr = %q", got)
	}
	st := waitExit(t, c)
	if st.Code != 7 || st.Signaled || st.String() != "exit status 7" {
		t.Errorf("status = %+v %q", st, st)
	}
	if _, ok := <-c.Done(); ok {
		t.Error("Done must be closed after the status")
	}
}

func TestSignalGroupKillsDescendants(t *testing.T) {
	r := NewReaper(testLog)
	defer r.Close()
	c, out, _ := startHelper(t, r, "family")
	gcPid, err := strconv.Atoi(readLine(t, out))
	if err != nil {
		t.Fatal(err)
	}
	if err := c.SignalGroup(syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	st := waitExit(t, c)
	if !st.Signaled || st.Signal != syscall.SIGKILL || st.String() != "signal: killed" {
		t.Errorf("status = %+v %q", st, st)
	}
	// The grandchild was in the same group: it must be gone too.
	deadline := time.Now().Add(5 * time.Second)
	for syscall.Kill(gcPid, 0) == nil {
		if time.Now().After(deadline) {
			t.Fatalf("grandchild %d survived the group kill", gcPid)
		}
		time.Sleep(10 * time.Millisecond)
	}
	// Signalling a group that no longer exists is not an error.
	if err := c.SignalGroup(syscall.SIGTERM); err != nil {
		t.Errorf("signal after exit: %v", err)
	}
}

func TestOwnProcessGroup(t *testing.T) {
	r := NewReaper(testLog)
	defer r.Close()
	c, out, _ := startHelper(t, r, "sleep")
	readLine(t, out)
	pgid, err := syscall.Getpgid(c.Pid())
	if err != nil {
		t.Fatal(err)
	}
	if pgid != c.Pid() || pgid == syscall.Getpgrp() {
		t.Errorf("pgid = %d, pid = %d, ours = %d", pgid, c.Pid(), syscall.Getpgrp())
	}
	_ = c.SignalGroup(syscall.SIGTERM)
	if st := waitExit(t, c); st.Signal != syscall.SIGTERM {
		t.Errorf("status = %+v", st)
	}
}

func TestOrphansAreCounted(t *testing.T) {
	r := NewReaper(testLog)
	defer r.Close()
	// A process started behind the reaper's back is reaped as an orphan.
	pid, err := syscall.ForkExec(os.Args[0], []string{os.Args[0]}, &syscall.ProcAttr{
		Env:   []string{"LOCH_TEST_CHILD=exit"},
		Files: []uintptr{0, nullFd(t), nullFd(t)},
	})
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for r.Orphans() == 0 {
		if time.Now().After(deadline) {
			t.Fatalf("orphan %d not reaped", pid)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func nullFd(t *testing.T) uintptr {
	f, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	return f.Fd()
}

func TestStartErrors(t *testing.T) {
	r := NewReaper(testLog)
	defer r.Close()
	for _, args := range [][]string{nil, {""}, {"/nonexistent/bootstrap"}, {"no-such-program-xyz"}} {
		if _, err := r.Start(Spec{Args: args}); err == nil {
			t.Errorf("Start(%q) must fail", args)
		}
	}
	// A path that exists but is not executable fails at exec time.
	f, err := os.CreateTemp(t.TempDir(), "notexec")
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
	if _, err := r.Start(Spec{Args: []string{f.Name()}}); err == nil {
		t.Error("non-executable file must fail")
	}
}

func TestNilOutputsGoToDevNull(t *testing.T) {
	r := NewReaper(testLog)
	defer r.Close()
	c, err := r.Start(Spec{Args: []string{os.Args[0]}, Env: []string{"LOCH_TEST_CHILD=exit"}})
	if err != nil {
		t.Fatal(err)
	}
	if st := waitExit(t, c); st.Code != 0 {
		t.Errorf("status = %+v", st)
	}
}

func TestWorkingDirectory(t *testing.T) {
	r := NewReaper(testLog)
	defer r.Close()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	outR, outW, _ := os.Pipe()
	defer outR.Close()
	c, err := r.Start(Spec{Args: []string{os.Args[0]}, Env: []string{"LOCH_TEST_CHILD=pwd"}, Dir: dir, Stdout: outW})
	outW.Close()
	if err != nil {
		t.Fatal(err)
	}
	if got := readLine(t, bufio.NewReader(outR)); got != dir {
		t.Errorf("cwd = %q, want %q", got, dir)
	}
	waitExit(t, c)
	if _, err := r.Start(Spec{Args: []string{os.Args[0]}, Dir: "/nonexistent-dir"}); err == nil {
		t.Error("missing working directory must fail")
	}
}

func TestOOMScoreAdj(t *testing.T) {
	r := NewReaper(testLog)
	defer r.Close()
	outR, outW, _ := os.Pipe()
	defer outR.Close()
	c, err := r.Start(Spec{Args: []string{os.Args[0]}, Env: []string{"LOCH_TEST_CHILD=sleep"}, Stdout: outW, OOMScoreAdj: 1000})
	outW.Close()
	if err != nil {
		t.Fatal(err)
	}
	defer c.SignalGroup(syscall.SIGKILL)
	readLine(t, bufio.NewReader(outR))
	if runtime.GOOS == "linux" {
		b, err := os.ReadFile("/proc/" + strconv.Itoa(c.Pid()) + "/oom_score_adj")
		if err != nil || strings.TrimSpace(string(b)) != "1000" {
			t.Errorf("oom_score_adj = %q %v", b, err)
		}
	}
}

func TestStartAfterCloseAndDoubleClose(t *testing.T) {
	r := NewReaper(testLog)
	r.Close()
	r.Close()
	if _, err := r.Start(Spec{Args: []string{os.Args[0]}}); !errors.Is(err, ErrClosed) {
		t.Errorf("start after close: %v", err)
	}
}

func TestManySimultaneousExits(t *testing.T) {
	r := NewReaper(testLog)
	defer r.Close()
	var children []*Child
	for range 20 {
		c, err := r.Start(Spec{Args: []string{os.Args[0]}, Env: []string{"LOCH_TEST_CHILD=exit"}})
		if err != nil {
			t.Fatal(err)
		}
		children = append(children, c)
	}
	for _, c := range children {
		waitExit(t, c)
	}
}

func TestRelativeProgramResolvedAgainstDir(t *testing.T) {
	dir := t.TempDir()
	self, err := os.ReadFile(os.Args[0])
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "main"), self, 0o755); err != nil {
		t.Fatal(err)
	}
	r := NewReaper(testLog)
	defer r.Close()
	c, err := r.Start(Spec{Args: []string{"./main"}, Env: []string{"LOCH_TEST_CHILD=exit"}, Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	if st := waitExit(t, c); st.Code != 0 {
		t.Errorf("status = %+v", st)
	}
}

func TestHarden(t *testing.T) {
	if err := Harden(); err != nil {
		t.Error(err)
	}
}

func TestParseStat(t *testing.T) {
	cases := map[string]procInfo{
		"42 (node) S 1 42 42 0 -1": {ppid: 1, state: 'S'},
		"7 (a b) (c)) R 3 7 7 0":   {ppid: 3, state: 'R'},
		"9 (x) Z 0 9":              {ppid: 0, state: 'Z'},
	}
	for in, want := range cases {
		if got, ok := parseStat([]byte(in)); !ok || got != want {
			t.Errorf("%q: %+v %v", in, got, ok)
		}
	}
	for _, bad := range []string{"", "1 (x", "1 (x) S", "1 (x) S nope", "1 (x) SS 1"} {
		if _, ok := parseStat([]byte(bad)); ok {
			t.Errorf("%q must fail", bad)
		}
	}
}

func TestLiveDescendants(t *testing.T) {
	// 1 -> 10 -> 11 -> 12; 1 -> 20 (zombie); 1 -> 21 (dead);
	// 30 has ppid 0 (docker exec); 31 child of 30.
	table := map[int]procInfo{
		1: {0, 'S'}, 10: {1, 'S'}, 11: {10, 'R'}, 12: {11, 'D'}, 20: {1, 'Z'}, 21: {1, 'X'},
		30: {0, 'S'}, 31: {30, 'S'},
	}
	got := liveDescendants(1, table)
	slices.Sort(got)
	if want := []int{10, 11, 12}; !slices.Equal(got, want) {
		t.Errorf("descendants = %v, want %v", got, want)
	}
	if got := liveDescendants(11, table); !slices.Equal(got, []int{12}) {
		t.Errorf("descendants of 11 = %v", got)
	}
}

func TestKillManyDescendantsExactCount(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("needs /proc")
	}
	r := NewReaper(testLog)
	defer r.Close()
	for range 20 {
		c, out, _ := startHelper(t, r, "sleep")
		readLine(t, out)
		_ = c
	}
	n, err := KillDescendants(os.Getpid())
	if err != nil || n != 20 {
		t.Fatalf("killed %d, err %v; want exactly 20", n, err)
	}
	// Nothing left: a second call signals nobody.
	if n, err := KillDescendants(os.Getpid()); err != nil || n != 0 {
		t.Errorf("second call: %d %v", n, err)
	}
}

func TestKillDescendants(t *testing.T) {
	r := NewReaper(testLog)
	defer r.Close()
	// A child in its own group, with a grandchild that escapes the group.
	c, out, _ := startHelper(t, r, "escape")
	gcPid, err := strconv.Atoi(readLine(t, out))
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "linux" {
		_ = syscall.Kill(gcPid, syscall.SIGKILL)
		if n, err := KillDescendants(os.Getpid()); n != 0 || err != nil {
			t.Errorf("without /proc: %d %v", n, err)
		}
		t.Skip("no /proc: KillDescendants is a no-op")
	}
	n, err := KillDescendants(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	waitExit(t, c)
	if n != 2 {
		t.Errorf("killed %d processes, want exactly the child and the escaped grandchild", n)
	}
	deadline := time.Now().Add(5 * time.Second)
	for syscall.Kill(gcPid, 0) == nil {
		if time.Now().After(deadline) {
			t.Fatalf("escaped grandchild %d survived", gcPid)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
