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
// LookPath. It scans the sources of the binary (cmd and internal), skipping
// the directories the go command ignores (testdata, ".x", "_x").
func TestNoOtherOSExec(t *testing.T) {
	fset := token.NewFileSet()
	for _, root := range []string{filepath.Join("..", "..", "cmd"), filepath.Join("..", "..", "internal")} {
		checkNoOSExec(t, fset, root)
	}
}

func checkNoOSExec(t *testing.T, fset *token.FileSet, root string) {
	t.Helper()
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if name := d.Name(); path != root && (name == "testdata" || strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_")) {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
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
