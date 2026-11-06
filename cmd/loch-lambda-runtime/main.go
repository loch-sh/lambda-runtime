// Copyright 2026 The Loch.sh Authors
// SPDX-License-Identifier: Apache-2.0

// Command loch-lambda-runtime runs an AWS Lambda function in a container:
// it serves the Lambda Runtime API to the function's bootstrap and the
// Lambda Invoke API to callers. See README.md.
package main

import (
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/loch-sh/lambda-runtime/internal/app"
	"github.com/loch-sh/lambda-runtime/internal/config"
	"github.com/loch-sh/lambda-runtime/internal/invokecli"
	"github.com/loch-sh/lambda-runtime/internal/process"
)

// version is set at build time with -ldflags "-X main.version=…".
var version = "dev"

func main() {
	// First of all, for every subcommand: a non-dumpable process keeps the
	// function's code (same uid) out of its /proc/<pid>/environ and memory.
	hardenErr := process.Harden()
	os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr, hardenErr))
}

// run executes the command line and returns the exit code. hardenErr is
// the result of process.Harden, which main ran first.
func run(args []string, stdin io.Reader, stdout, stderr io.Writer, hardenErr error) int {
	src := config.DefaultSources()
	src.Output = stderr
	cfg, err := config.Parse(args, src)
	switch {
	case errors.Is(err, config.ErrHelp):
		return 0
	case err != nil:
		fmt.Fprintln(stderr, "loch-lambda-runtime:", err)
		return 2
	}
	switch cfg.Command {
	case config.CommandVersion:
		fmt.Fprintln(stdout, version)
		return 0
	case config.CommandRun:
		return app.Run(cfg, app.Env{Stdout: stdout, Stderr: stderr, Version: version, HardenErr: hardenErr})
	case config.CommandHealth:
		return app.Health(cfg, stdout)
	default:
		return invokecli.Run(cfg, stdin, stdout, stderr)
	}
}
