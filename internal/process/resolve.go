// Copyright 2026 The Loch.sh Authors
// SPDX-License-Identifier: Apache-2.0

//go:build unix

package process

import (
	"errors"
	"os/exec"
	"path/filepath"
	"strings"
)

// resolve returns the path of the program to execute. A bare name is
// looked up in PATH; a relative path ("./main", "bin/bootstrap") is
// resolved against dir, where the child will run.
func resolve(args []string, dir string) (string, error) {
	if len(args) == 0 || args[0] == "" {
		return "", errors.New("empty command")
	}
	name := args[0]
	if dir != "" && strings.Contains(name, "/") && !filepath.IsAbs(name) {
		name = filepath.Join(dir, name)
	}
	return exec.LookPath(name)
}
