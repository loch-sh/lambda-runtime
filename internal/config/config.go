// Copyright 2026 The Loch.sh Authors
// SPDX-License-Identifier: Apache-2.0

// Package config parses the command line and the environment of
// loch-lambda-runtime. Every value is validated up front: an invalid
// configuration fails immediately with a message naming the setting, and
// all the errors are reported at once.
package config

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/loch-sh/lambda-runtime/internal/cgroup"
)

// Command is the subcommand selected on the command line.
type Command string

// Subcommands.
const (
	CommandRun     Command = "run"
	CommandHealth  Command = "health"
	CommandVersion Command = "version"
	CommandInvoke  Command = "invoke"
)

// Usage is the synopsis shown in errors and by -h.
const Usage = `usage:
  loch-lambda-runtime [run] [flags] -- <command> [args...]
  loch-lambda-runtime [run] [flags] <handler>     (= /lambda-entrypoint.sh <handler>)
  loch-lambda-runtime health [flags]
  loch-lambda-runtime invoke [flags] [--timeout <s>] [--log-tail] [--watch-stdin] < event.json
  loch-lambda-runtime version`

// ErrHelp is returned when -h or --help was requested.
var ErrHelp = flag.ErrHelp

// Config is the complete, validated configuration.
type Config struct {
	Command Command
	Runtime Runtime
	// ChildArgs is the command run as the Lambda runtime; ChildArgs[0] is
	// the program.
	ChildArgs []string
	// ChildEnv is the environment of the child, AWS variables included.
	ChildEnv []string
	// Warnings are notes about the configuration, to log at startup.
	Warnings []string
	Function Function
	// Invoke holds the options of the invoke subcommand.
	Invoke Invoke
}

// Invoke holds the options of the invoke subcommand.
type Invoke struct {
	// Timeout bounds the whole call: queue wait and execution.
	Timeout time.Duration
	LogTail bool
	// WatchStdin reads the event as the first JSON value on stdin, then
	// takes stdin closing as the caller going away (docker exec closes the
	// process's stdin when its client disconnects).
	WatchStdin bool
}

// Env abstracts the process environment for tests.
type Env interface {
	Lookup(key string) (string, bool)
	Environ() []string
}

// OSEnv is the real process environment.
type OSEnv struct{}

// Lookup implements Env.
func (OSEnv) Lookup(key string) (string, bool) { return os.LookupEnv(key) }

// Environ implements Env.
func (OSEnv) Environ() []string { return os.Environ() }

// Sources groups the inputs of Parse so tests can replace each of them.
type Sources struct {
	Env Env
	// CgroupMemoryMax is the path of the cgroup v2 memory.max file.
	CgroupMemoryMax string
	Now             func() time.Time
	// Output receives the usage text of -h and flag errors.
	Output io.Writer
}

// DefaultSources reads the real process environment and cgroup.
func DefaultSources() Sources {
	return Sources{
		Env:             OSEnv{},
		CgroupMemoryMax: filepath.Join(cgroup.Resolve(cgroup.DefaultRoot, cgroup.SelfCgroupFile), "memory.max"),
		Now:             time.Now,
		Output:          os.Stderr,
	}
}

// Parse builds the configuration from the arguments (without the program
// name) and the sources. Flags take precedence over the environment.
func Parse(args []string, src Sources) (*Config, error) {
	cmd, args := splitCommand(args)
	if cmd == CommandVersion {
		if len(args) > 0 {
			return nil, fmt.Errorf("version takes no arguments\n%s", Usage)
		}
		return &Config{Command: cmd}, nil
	}

	flagArgs, command, hasSeparator := splitAtSeparator(args)
	parsed, err := parseFlags(flagArgs, cmd, src)
	if err != nil {
		return nil, err
	}

	cfg := &Config{Command: cmd}
	var errs []error
	if cmd != CommandHealth {
		cfg.Function, errs = parseFunction(src)
	}
	rt, rtErrs := parseRuntime(parsed.values, src.Env, cfg.Function.MemoryMB)
	cfg.Runtime = rt
	errs = append(errs, rtErrs...)

	switch cmd {
	case CommandRun:
		errs = append(errs, cfg.completeRun(parsed.positional, command, hasSeparator, src.Env)...)
	case CommandHealth, CommandInvoke:
		if hasSeparator || len(parsed.positional) > 0 {
			errs = append(errs, fmt.Errorf("%s takes no command", cmd))
		}
		if cmd == CommandInvoke {
			cfg.Invoke, err = parsed.invokeOptions(cfg.Function.Timeout + cfg.Runtime.QueueTimeout + invokeMargin)
			errs = appendIf(errs, err)
		}
	}
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	return cfg, nil
}

// completeRun resolves what only the run command needs: the child command
// and environment, the authentication requirement and the warnings.
func (cfg *Config) completeRun(positional, command []string, hasSeparator bool, env Env) []error {
	var errs []error
	var err error
	cfg.ChildArgs, err = childCommand(positional, command, hasSeparator, cfg.Runtime.Bootstrap)
	errs = appendIf(errs, err)
	errs = append(errs, cfg.Runtime.Auth.validate()...)
	cfg.ChildEnv, cfg.Warnings = childEnv(env.Environ(), cfg.Function, cfg.Runtime.RuntimeAPIAddr)
	if w := cfg.Function.memoryWarning(); w != "" {
		cfg.Warnings = append(cfg.Warnings, w)
	}
	return errs
}

// splitCommand removes a leading subcommand; without one, it is run.
func splitCommand(args []string) (Command, []string) {
	if len(args) > 0 {
		switch c := Command(args[0]); c {
		case CommandRun, CommandHealth, CommandVersion, CommandInvoke:
			return c, args[1:]
		}
	}
	return CommandRun, args
}

// splitAtSeparator splits args at the first "--". No flag takes "--" as
// its value, so the first one always is the separator.
func splitAtSeparator(args []string) (flags, command []string, found bool) {
	for i, a := range args {
		if a == "--" {
			return args[:i], args[i+1:], true
		}
	}
	return args, nil, false
}

// childCommand resolves the command of the Lambda runtime: everything
// after "--", or the shortcut "<bootstrap> <handler>".
func childCommand(positional, command []string, hasSeparator bool, bootstrap string) ([]string, error) {
	switch {
	case hasSeparator && len(positional) > 0:
		return nil, fmt.Errorf("unexpected argument %q before --\n%s", positional[0], Usage)
	case hasSeparator && (len(command) == 0 || command[0] == ""):
		return nil, fmt.Errorf("missing command after --\n%s", Usage)
	case hasSeparator:
		return command, nil
	case len(positional) == 1 && positional[0] != "":
		return []string{bootstrap, positional[0]}, nil
	case len(positional) == 0:
		return nil, fmt.Errorf("missing handler or command\n%s", Usage)
	default:
		return nil, fmt.Errorf("several arguments without --: use -- <command> [args...]\n%s", Usage)
	}
}

func appendIf(errs []error, err error) []error {
	if err != nil {
		return append(errs, err)
	}
	return errs
}
