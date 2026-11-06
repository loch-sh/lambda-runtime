// Copyright 2026 The Loch.sh Authors
// SPDX-License-Identifier: Apache-2.0

// Package release tests the release tooling (the Makefile).
package release

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// runMake runs make in the repository root and returns its combined output.
func runMake(t *testing.T, env []string, args ...string) (string, error) {
	t.Helper()
	cmd := exec.Command("make", append([]string{"-s", "-C", "../.."}, args...)...)
	cmd.Env = append(os.Environ(), env...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func requireMake(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("make"); err != nil {
		t.Skip("make not installed")
	}
}

// TestUntrustedInputsAreNeverExecuted feeds shell and make injections
// through VERSION, SIGN and MINISIGN_KEY, on the command line and in the
// environment (how a CI passes a tag), and checks that none ran.
func TestUntrustedInputsAreNeverExecuted(t *testing.T) {
	requireMake(t)
	dir := t.TempDir()
	marker := func(n string) string { return filepath.Join(dir, n) }
	payloads := []string{
		"v1.0.0$(shell touch " + marker("make-shell") + ")",
		"v1.0.0`touch " + marker("backtick") + "`",
		"v1.0.0'$(touch " + marker("quote-break") + ")'",
		"v1.0.0;touch " + marker("semicolon"),
		"v1.0.0$$(touch " + marker("dollar-dollar") + ")",
	}
	for _, p := range payloads {
		for _, target := range []string{"release-check", "build", "release", "reproducible"} {
			_, _ = runMake(t, nil, "-n", target, "RELEASE_GIT_CHECK=0", "VERSION="+p)
		}
		_, _ = runMake(t, nil, "release-check", "RELEASE_GIT_CHECK=0", "VERSION="+p)
		_, _ = runMake(t, []string{"VERSION=" + p}, "release-check", "RELEASE_GIT_CHECK=0")
		_, _ = runMake(t, nil, "release-check", "RELEASE_GIT_CHECK=0", "VERSION=v1.0.0", "SIGN="+p)
		_, _ = runMake(t, []string{"SIGN=" + p}, "release-check", "RELEASE_GIT_CHECK=0", "VERSION=v1.0.0")
		_, _ = runMake(t, nil, "sign", "SIGN=minisign", "MINISIGN_KEY="+p)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		t.Errorf("injection executed: %s", e.Name())
	}
}

func TestReleaseCheck(t *testing.T) {
	requireMake(t)
	cases := []struct {
		args []string
		want string // "" when it must pass
	}{
		{[]string{"VERSION=v1.2.3"}, ""},
		{[]string{"VERSION=v1.2.3-rc.1"}, ""},
		{[]string{"VERSION=1.2.3"}, "VERSION must look like"},
		{[]string{"VERSION=v1.2.3 x"}, "VERSION must look like"},
		{[]string{"VERSION=v1.2.3", "SIGN=gpg"}, "SIGN must be"},
		{[]string{"VERSION=v1.2.3", "REQUIRE_SIGNATURE=1"}, "REQUIRE_SIGNATURE=1"},
		{[]string{"VERSION=v1.2.3", "REQUIRE_SIGNATURE=1", "SIGN=cosign"}, ""},
	}
	for _, c := range cases {
		out, err := runMake(t, nil, append([]string{"release-check", "RELEASE_GIT_CHECK=0"}, c.args...)...)
		switch {
		case c.want == "" && err != nil:
			t.Errorf("%v: %v %s", c.args, err, out)
		case c.want != "" && (err == nil || !strings.Contains(out, c.want)):
			t.Errorf("%v: want %q, got %v %s", c.args, c.want, err, out)
		}
	}
	// With the git checks, a commit that does not carry the tag is refused.
	if out, err := runMake(t, nil, "release-check", "VERSION=v0.0.0-not-a-tag"); err == nil {
		t.Errorf("untagged commit accepted: %s", out)
	}
}
