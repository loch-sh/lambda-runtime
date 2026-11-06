// Copyright 2026 The Loch.sh Authors
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"flag"
	"fmt"
	"strings"
	"time"
)

// invokeMargin is added to the function timeout and the queue wait to get
// the default deadline of the invoke subcommand.
const invokeMargin = 5 * time.Second

// setting is one LOCH_RUNTIME_* option, settable by flag or environment.
type setting struct{ flag, env, help string }

var settings = []setting{
	{"invoke-addr", "LOCH_RUNTIME_INVOKE_ADDR", "listen address of the Invoke API"},
	{"runtime-api-addr", "LOCH_RUNTIME_API_ADDR", "loopback listen address of the Runtime API"},
	{"health-addr", "LOCH_RUNTIME_HEALTH_ADDR", "loopback listen address of the health endpoint used by the health subcommand"},
	{"bootstrap", "LOCH_RUNTIME_BOOTSTRAP", "program run with the handler in the shortcut form"},
	{"workdir", "LOCH_RUNTIME_WORKDIR", "working directory of the child (default: the current one, i.e. the image WORKDIR)"},
	{"init-timeout", "LOCH_RUNTIME_INIT_TIMEOUT", "maximum init duration (Go duration or seconds)"},
	{"shutdown-grace", "LOCH_RUNTIME_SHUTDOWN_GRACE", "time given to the running invocation on SIGTERM"},
	{"max-payload", "LOCH_RUNTIME_MAX_PAYLOAD", "maximum request and response size (bytes, or KiB/MiB/GiB suffix)"},
	{"queue-size", "LOCH_RUNTIME_QUEUE_SIZE", "maximum number of waiting invocations"},
	{"queue-bytes", "LOCH_RUNTIME_QUEUE_BYTES", "maximum total size of the waiting invocations"},
	{"queue-timeout", "LOCH_RUNTIME_QUEUE_TIMEOUT", "maximum time an invocation waits in the queue"},
	{"max-conns", "LOCH_RUNTIME_MAX_CONNS", "maximum number of open connections to the Invoke API"},
	{"auth", "LOCH_RUNTIME_AUTH", "on (default) or off"},
	{"token-file", "LOCH_RUNTIME_TOKEN_FILE", "file holding the bearer token"},
	{"access-key-file", "LOCH_RUNTIME_ACCESS_KEY_FILE", "file holding <access key id>:<secret> for SigV4"},
	{"log-level", "LOCH_RUNTIME_LOG_LEVEL", "debug, info, warn or error"},
}

// rawValues maps an environment variable name to its raw, trimmed value
// (flag or environment), "" when unset.
type rawValues map[string]string

// parsedFlags is the result of the flag parsing.
type parsedFlags struct {
	positional []string
	values     rawValues
	// Options of the invoke subcommand.
	timeout string
	logTail bool
}

// parseFlags parses the flags of cmd and completes them with the
// environment.
func parseFlags(args []string, cmd Command, src Sources) (*parsedFlags, error) {
	fs := flag.NewFlagSet("loch-lambda-runtime "+string(cmd), flag.ContinueOnError)
	fs.SetOutput(src.Output)
	fs.Usage = func() { printUsage(fs) }
	ptrs := make(map[string]*string, len(settings))
	for _, s := range settings {
		ptrs[s.env] = fs.String(s.flag, "", s.help+" ("+s.env+")")
	}
	out := &parsedFlags{}
	if cmd == CommandInvoke {
		fs.StringVar(&out.timeout, "timeout", "", "deadline of the call (default: function timeout + queue timeout + 5s)")
		fs.BoolVar(&out.logTail, "log-tail", false, "include the last 4 KB of the invocation logs")
	}
	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	setByFlag := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { setByFlag[f.Name] = true })

	out.positional = fs.Args()
	out.values = make(rawValues, len(settings))
	for _, s := range settings {
		v := *ptrs[s.env]
		if !setByFlag[s.flag] {
			v, _ = src.Env.Lookup(s.env)
		}
		out.values[s.env] = strings.TrimSpace(v)
	}
	return out, nil
}

// invokeOptions validates the options of the invoke subcommand.
func (f *parsedFlags) invokeOptions(defaultTimeout time.Duration) (Invoke, error) {
	opts := Invoke{Timeout: defaultTimeout, LogTail: f.logTail}
	if f.timeout == "" {
		return opts, nil
	}
	d, err := parseDuration(strings.TrimSpace(f.timeout), maxSettingDuration)
	if err == nil && d == 0 {
		err = fmt.Errorf("%q must be positive", f.timeout)
	}
	if err != nil {
		return opts, fmt.Errorf("--timeout: %w", err)
	}
	opts.Timeout = d
	return opts, nil
}

func printUsage(fs *flag.FlagSet) {
	fmt.Fprintln(fs.Output(), Usage)
	fmt.Fprintln(fs.Output(), "\nflags (LOCH_RUNTIME_* settings can also be set by the environment variable shown):")
	fs.VisitAll(func(f *flag.Flag) {
		fmt.Fprintf(fs.Output(), "  --%s\t%s\n", f.Name, f.Usage)
	})
}
