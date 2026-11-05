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

	"github.com/loch-sh/lambda-runtime/internal/config"
)

// version is set at build time with -ldflags "-X main.version=…".
var version = "dev"

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// run executes the command line and returns the exit code.
func run(args []string, stdout, stderr io.Writer) int {
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
	default:
		fmt.Fprintf(stderr, "loch-lambda-runtime: %s is not implemented yet\n", cfg.Command)
		return 1
	}
}
