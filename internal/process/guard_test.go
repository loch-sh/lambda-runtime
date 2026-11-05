// Copyright 2026 The Loch.sh Authors
// SPDX-License-Identifier: Apache-2.0

package process

import (
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"
)

// TestNoOtherOSExec guards the Reaper: it collects every child with
// wait4(-1), so an exec.Cmd started anywhere else in the binary would lose
// its exit status (ECHILD). Only resolve.go may import os/exec, for
// LookPath.
func TestNoOtherOSExec(t *testing.T) {
	root := filepath.Join("..", "..")
	fset := token.NewFileSet()
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return err
		}
		f, err := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
		if err != nil {
			return err
		}
		for _, imp := range f.Imports {
			if imp.Path.Value == `"os/exec"` && filepath.Base(path) != "resolve.go" {
				t.Errorf("%s imports os/exec: use process.Reaper to start processes", path)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
