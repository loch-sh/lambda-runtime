// Copyright 2026 The Loch.sh Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestRunExitCodes(t *testing.T) {
	cases := []struct {
		args       []string
		code       int
		stdout     string
		stderrPart string
	}{
		{[]string{"version"}, 0, "dev\n", ""},
		{[]string{"-h"}, 0, "", "usage:"},
		{nil, 2, "", "missing handler"},
		{[]string{"--nope"}, 2, "", "flag provided but not defined"},
		{[]string{"invoke", "--invoke-addr", "127.0.0.1:1"}, 1, "", "invoke:"},
	}
	for _, c := range cases {
		var out, errOut bytes.Buffer
		if code := run(c.args, strings.NewReader(`{}`), &out, &errOut, nil); code != c.code {
			t.Errorf("%q: code = %d, want %d (stderr %q)", c.args, code, c.code, errOut.String())
		}
		if out.String() != c.stdout || !strings.Contains(errOut.String(), c.stderrPart) {
			t.Errorf("%q: stdout = %q, stderr = %q", c.args, out.String(), errOut.String())
		}
	}
}

func TestHealthUnreachable(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := run([]string{"health", "--health-addr", "127.0.0.1:1"}, nil, &out, &errOut, nil); code != 1 {
		t.Errorf("code = %d", code)
	}
	if !strings.Contains(out.String(), "unreachable") {
		t.Errorf("stdout = %q", out.String())
	}
}
