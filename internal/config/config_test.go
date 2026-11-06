// Copyright 2026 The Loch.sh Authors
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

type mapEnv map[string]string

func (m mapEnv) Lookup(k string) (string, bool) { v, ok := m[k]; return v, ok }
func (m mapEnv) Environ() []string {
	out := make([]string, 0, len(m))
	for k, v := range m {
		out = append(out, k+"="+v)
	}
	slices.Sort(out)
	return out
}

func sources(t *testing.T, env mapEnv, memoryMax string) Sources {
	t.Helper()
	if env == nil {
		env = mapEnv{}
	}
	if _, ok := env["LOCH_RUNTIME_AUTH"]; !ok {
		env["LOCH_RUNTIME_AUTH"] = "off"
	}
	path := ""
	if memoryMax != "" {
		path = filepath.Join(t.TempDir(), "memory.max")
		if err := os.WriteFile(path, []byte(memoryMax), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return Sources{
		Env:             env,
		CgroupMemoryMax: path,
		Now:             func() time.Time { return time.Date(2026, 11, 6, 10, 0, 0, 0, time.UTC) },
		Output:          io.Discard,
	}
}

func envValue(env []string, key string) (string, int) {
	val, n := "", 0
	for _, kv := range env {
		if k, v, _ := strings.Cut(kv, "="); k == key {
			val, n = v, n+1
		}
	}
	return val, n
}

func TestShortcutAndDefaults(t *testing.T) {
	cfg, err := Parse([]string{"index.handler"}, sources(t, mapEnv{"PATH": "/bin", "FOO": "bar"}, ""))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Command != CommandRun {
		t.Errorf("command = %q", cfg.Command)
	}
	if want := []string{"/lambda-entrypoint.sh", "index.handler"}; !slices.Equal(cfg.ChildArgs, want) {
		t.Errorf("child args = %q, want %q", cfg.ChildArgs, want)
	}
	if cfg.Runtime.InvokeAddr != DefaultInvokeAddr || cfg.Runtime.RuntimeAPIAddr != DefaultRuntimeAPIAddr {
		t.Errorf("addrs = %s %s", cfg.Runtime.InvokeAddr, cfg.Runtime.RuntimeAPIAddr)
	}
	if cfg.Runtime.InitTimeout != 10*time.Second || cfg.Runtime.MaxPayload != 6<<20 || cfg.Function.Timeout != 300*time.Second {
		t.Errorf("defaults: init=%s payload=%d timeout=%s", cfg.Runtime.InitTimeout, cfg.Runtime.MaxPayload, cfg.Function.Timeout)
	}
	fn := cfg.Function
	if fn.Name != "function" || fn.Version != "$LATEST" || fn.Region != "us-east-1" || fn.MemoryMB != 3008 || fn.MemorySource != MemoryFromDefault {
		t.Errorf("function = %+v", fn)
	}
	if fn.ARN() != "arn:aws:lambda:us-east-1:000000000000:function:function" {
		t.Errorf("arn = %s", fn.ARN())
	}
	if !strings.HasPrefix(fn.LogStreamName, "2026/11/06/[$LATEST]") || len(fn.LogStreamName) != len("2026/11/06/[$LATEST]")+32 {
		t.Errorf("log stream = %s", fn.LogStreamName)
	}
	for key, want := range map[string]string{
		"FOO":                             "bar",
		"PATH":                            "/bin",
		"AWS_LAMBDA_RUNTIME_API":          "127.0.0.1:9001",
		"AWS_LAMBDA_FUNCTION_NAME":        "function",
		"AWS_LAMBDA_FUNCTION_VERSION":     "$LATEST",
		"AWS_LAMBDA_FUNCTION_MEMORY_SIZE": "3008",
		"AWS_LAMBDA_LOG_GROUP_NAME":       "/aws/lambda/function",
		"AWS_REGION":                      "us-east-1",
		"AWS_DEFAULT_REGION":              "us-east-1",
	} {
		got, n := envValue(cfg.ChildEnv, key)
		if n != 1 || got != want {
			t.Errorf("child env %s = %q (%d times), want %q", key, got, n, want)
		}
	}
}

func TestSeparatorCommand(t *testing.T) {
	cfg, err := Parse([]string{"--queue-size", "4", "--", "/var/task/bootstrap", "--flag", "x"}, sources(t, mapEnv{}, ""))
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"/var/task/bootstrap", "--flag", "x"}; !slices.Equal(cfg.ChildArgs, want) {
		t.Errorf("child args = %q", cfg.ChildArgs)
	}
	if cfg.Runtime.QueueSize != 4 {
		t.Errorf("queue size = %d", cfg.Runtime.QueueSize)
	}
	// "run" prefix and a single argument after -- is not the shortcut.
	cfg, err = Parse([]string{"run", "--", "index.handler"}, sources(t, mapEnv{}, ""))
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(cfg.ChildArgs, []string{"index.handler"}) {
		t.Errorf("child args = %q", cfg.ChildArgs)
	}
}

func TestEnvironmentAndFlagPrecedence(t *testing.T) {
	env := mapEnv{
		"LOCH_RUNTIME_QUEUE_SIZE":         "8",
		"LOCH_RUNTIME_INIT_TIMEOUT":       "2.5",
		"LOCH_RUNTIME_SHUTDOWN_GRACE":     "1m",
		"LOCH_RUNTIME_MAX_PAYLOAD":        "1MiB",
		"LOCH_RUNTIME_BOOTSTRAP":          "/custom-entry",
		"AWS_LAMBDA_FUNCTION_NAME":        "resize",
		"AWS_LAMBDA_FUNCTION_TIMEOUT":     "3",
		"AWS_LAMBDA_FUNCTION_MEMORY_SIZE": "256",
		"AWS_REGION":                      "eu-west-3",
		"AWS_LAMBDA_RUNTIME_API":          "evil:1",
	}
	cfg, err := Parse([]string{"--queue-size=2", "app.handler"}, sources(t, env, "134217728"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Runtime.QueueSize != 2 {
		t.Errorf("flag must win over env: queue size = %d", cfg.Runtime.QueueSize)
	}
	if cfg.Runtime.InitTimeout != 2500*time.Millisecond || cfg.Runtime.ShutdownGrace != time.Minute || cfg.Runtime.MaxPayload != 1<<20 {
		t.Errorf("init=%s grace=%s payload=%d", cfg.Runtime.InitTimeout, cfg.Runtime.ShutdownGrace, cfg.Runtime.MaxPayload)
	}
	if cfg.ChildArgs[0] != "/custom-entry" {
		t.Errorf("bootstrap = %q", cfg.ChildArgs[0])
	}
	fn := cfg.Function
	if fn.Name != "resize" || fn.Timeout != 3*time.Second || fn.MemoryMB != 256 || fn.MemorySource != MemoryFromEnv || fn.Region != "eu-west-3" {
		t.Errorf("function = %+v", fn)
	}
	if v, n := envValue(cfg.ChildEnv, "AWS_LAMBDA_RUNTIME_API"); n != 1 || v != "127.0.0.1:9001" {
		t.Errorf("AWS_LAMBDA_RUNTIME_API = %q (%d)", v, n)
	}
	if v, _ := envValue(cfg.ChildEnv, "AWS_LAMBDA_FUNCTION_MEMORY_SIZE"); v != "256" {
		t.Errorf("memory env = %q", v)
	}
}

func TestMemoryFromCgroup(t *testing.T) {
	cfg, err := Parse([]string{"h"}, sources(t, mapEnv{}, "268435456\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Function.MemoryMB != 256 || cfg.Function.MemorySource != MemoryFromCgroup {
		t.Errorf("memory = %d (%s)", cfg.Function.MemoryMB, cfg.Function.MemorySource)
	}
	if v, _ := envValue(cfg.ChildEnv, "AWS_LAMBDA_FUNCTION_MEMORY_SIZE"); v != "256" {
		t.Errorf("memory env = %q", v)
	}
	cfg, err = Parse([]string{"h"}, sources(t, mapEnv{}, "max\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Function.MemoryMB != DefaultMemoryMB || cfg.Function.MemorySource != MemoryFromDefault {
		t.Errorf("unlimited cgroup: memory = %d (%s)", cfg.Function.MemoryMB, cfg.Function.MemorySource)
	}
}

func TestSubcommands(t *testing.T) {
	cfg, err := Parse([]string{"version"}, sources(t, mapEnv{}, ""))
	if err != nil || cfg.Command != CommandVersion {
		t.Fatalf("version: %v %v", cfg, err)
	}
	cfg, err = Parse([]string{"health", "--invoke-addr", "127.0.0.1:9999"}, sources(t, mapEnv{}, ""))
	if err != nil || cfg.Command != CommandHealth || cfg.Runtime.InvokeAddr != "127.0.0.1:9999" {
		t.Fatalf("health: %+v %v", cfg, err)
	}
	if _, err := Parse([]string{"health", "x"}, sources(t, mapEnv{}, "")); err == nil {
		t.Error("health with an argument must fail")
	}
	if _, err := Parse([]string{"version", "x"}, sources(t, mapEnv{}, "")); err == nil {
		t.Error("version with an argument must fail")
	}
	if _, err := Parse([]string{"-h"}, sources(t, mapEnv{}, "")); !errors.Is(err, ErrHelp) {
		t.Errorf("-h: %v", err)
	}
}

func TestInvalid(t *testing.T) {
	cases := []struct {
		name string
		args []string
		env  mapEnv
		want string
	}{
		{"no handler", nil, nil, "missing handler"},
		{"empty after separator", []string{"--"}, nil, "missing command after --"},
		{"args before separator", []string{"x", "--", "y"}, nil, "unexpected argument"},
		{"two args without separator", []string{"a", "b"}, nil, "several arguments"},
		{"unknown flag", []string{"--nope", "h"}, nil, "flag provided but not defined"},
		{"public runtime api", []string{"h"}, mapEnv{"LOCH_RUNTIME_API_ADDR": "0.0.0.0:9001"}, "loopback"},
		{"hostname runtime api", []string{"h"}, mapEnv{"LOCH_RUNTIME_API_ADDR": "localhost:9001"}, "loopback"},
		{"ipv6 runtime api", []string{"h"}, mapEnv{"LOCH_RUNTIME_API_ADDR": "[::1]:9001"}, "IPv4"},
		{"bad invoke addr", []string{"h"}, mapEnv{"LOCH_RUNTIME_INVOKE_ADDR": "8080"}, "host:port"},
		{"bad port", []string{"h"}, mapEnv{"LOCH_RUNTIME_INVOKE_ADDR": ":0"}, "invalid port"},
		{"bad duration", []string{"h"}, mapEnv{"LOCH_RUNTIME_INIT_TIMEOUT": "soon"}, "LOCH_RUNTIME_INIT_TIMEOUT"},
		{"zero init timeout", []string{"h"}, mapEnv{"LOCH_RUNTIME_INIT_TIMEOUT": "0"}, "must be positive"},
		{"negative grace", []string{"h"}, mapEnv{"LOCH_RUNTIME_SHUTDOWN_GRACE": "-1s"}, "out of range"},
		{"bad size", []string{"h"}, mapEnv{"LOCH_RUNTIME_MAX_PAYLOAD": "6MB"}, "not a size"},
		{"tiny size", []string{"h"}, mapEnv{"LOCH_RUNTIME_MAX_PAYLOAD": "10"}, "outside"},
		{"huge size", []string{"h"}, mapEnv{"LOCH_RUNTIME_MAX_PAYLOAD": "99GiB"}, "LOCH_RUNTIME_MAX_PAYLOAD"},
		{"bad queue", []string{"h"}, mapEnv{"LOCH_RUNTIME_QUEUE_SIZE": "-1"}, "outside"},
		{"bad level", []string{"h"}, mapEnv{"LOCH_RUNTIME_LOG_LEVEL": "trace"}, "LOG_LEVEL"},
		{"bad name", []string{"h"}, mapEnv{"AWS_LAMBDA_FUNCTION_NAME": "a/b"}, "AWS_LAMBDA_FUNCTION_NAME"},
		{"bad timeout", []string{"h"}, mapEnv{"AWS_LAMBDA_FUNCTION_TIMEOUT": "0"}, "AWS_LAMBDA_FUNCTION_TIMEOUT"},
		{"bad memory", []string{"h"}, mapEnv{"AWS_LAMBDA_FUNCTION_MEMORY_SIZE": "lots"}, "AWS_LAMBDA_FUNCTION_MEMORY_SIZE"},
		{"bad region", []string{"h"}, mapEnv{"AWS_REGION": "eu west"}, "AWS_REGION"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			env := c.env
			if env == nil {
				env = mapEnv{}
			}
			_, err := Parse(c.args, sources(t, env, ""))
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("err = %v, want it to contain %q", err, c.want)
			}
		})
	}
}

func TestAllErrorsReported(t *testing.T) {
	_, err := Parse([]string{"h"}, sources(t, mapEnv{
		"LOCH_RUNTIME_QUEUE_SIZE":     "x",
		"AWS_LAMBDA_FUNCTION_TIMEOUT": "y",
	}, ""))
	if err == nil || !strings.Contains(err.Error(), "QUEUE_SIZE") || !strings.Contains(err.Error(), "FUNCTION_TIMEOUT") {
		t.Errorf("err = %v", err)
	}
}

func TestAuth(t *testing.T) {
	on := mapEnv{"LOCH_RUNTIME_AUTH": "on"}
	if _, err := Parse([]string{"h"}, sources(t, on, "")); err == nil || !strings.Contains(err.Error(), "authentication is on") {
		t.Errorf("auth without credential: %v", err)
	}
	cfg, err := Parse([]string{"h"}, sources(t, mapEnv{
		"LOCH_RUNTIME_AUTH":            "on",
		"LOCH_RUNTIME_TOKEN_FILE":      "/run/secrets/token",
		"LOCH_RUNTIME_ACCESS_KEY_FILE": "/run/secrets/keys",
	}, ""))
	if err != nil {
		t.Fatal(err)
	}
	want := Auth{Enabled: true, TokenFile: "/run/secrets/token", AccessKeyFile: "/run/secrets/keys"}
	if cfg.Runtime.Auth != want {
		t.Errorf("auth = %+v", cfg.Runtime.Auth)
	}
	// Default is on.
	if _, err := Parse([]string{"h"}, sources(t, mapEnv{"LOCH_RUNTIME_AUTH": ""}, "")); err == nil {
		t.Error("authentication must be on by default")
	}
	// Health and invoke do not require a credential.
	if _, err := Parse([]string{"health"}, sources(t, on, "")); err != nil {
		t.Errorf("health: %v", err)
	}
	for _, bad := range []mapEnv{
		{"LOCH_RUNTIME_AUTH": "maybe"},
		{"LOCH_RUNTIME_TOKEN_FILE": "token"},
	} {
		if _, err := Parse([]string{"h"}, sources(t, bad, "")); err == nil {
			t.Errorf("%v must fail", bad)
		}
	}
}

func TestQueueBytes(t *testing.T) {
	cases := []struct {
		env    mapEnv
		memory string
		want   int64
	}{
		{mapEnv{}, "", 32 << 20}, // no memory known: cap
		{mapEnv{"AWS_LAMBDA_FUNCTION_MEMORY_SIZE": "128"}, "", 16 << 20}, // memory/8
		{mapEnv{"AWS_LAMBDA_FUNCTION_MEMORY_SIZE": "1024"}, "", 32 << 20},
		{mapEnv{"AWS_LAMBDA_FUNCTION_MEMORY_SIZE": "32"}, "", 6 << 20}, // never below one payload
		{mapEnv{"LOCH_RUNTIME_QUEUE_BYTES": "64MiB"}, "", 64 << 20},
	}
	for _, c := range cases {
		cfg, err := Parse([]string{"h"}, sources(t, c.env, c.memory))
		if err != nil {
			t.Fatal(err)
		}
		if cfg.Runtime.QueueBytes != c.want {
			t.Errorf("%v: queue bytes = %d, want %d", c.env, cfg.Runtime.QueueBytes, c.want)
		}
	}
	if _, err := Parse([]string{"h"}, sources(t, mapEnv{"LOCH_RUNTIME_QUEUE_BYTES": "1MiB"}, "")); err == nil {
		t.Error("queue bytes below the max payload must fail")
	}
}

func TestInvokeSubcommand(t *testing.T) {
	env := mapEnv{"AWS_LAMBDA_FUNCTION_TIMEOUT": "10", "LOCH_RUNTIME_QUEUE_TIMEOUT": "20s"}
	cfg, err := Parse([]string{"invoke", "--log-tail"}, sources(t, env, ""))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Command != CommandInvoke || !cfg.Invoke.LogTail || cfg.Invoke.Timeout != 35*time.Second {
		t.Errorf("invoke = %+v", cfg.Invoke)
	}
	cfg, err = Parse([]string{"invoke", "--timeout", "3"}, sources(t, env, ""))
	if err != nil || cfg.Invoke.Timeout != 3*time.Second || cfg.Invoke.LogTail {
		t.Errorf("invoke --timeout: %+v %v", cfg, err)
	}
	for _, args := range [][]string{
		{"invoke", "--timeout", "0"},
		{"invoke", "--timeout", "never"},
		{"invoke", "x"},
		{"invoke", "--", "x"},
		{"run", "--log-tail", "h"},
	} {
		if _, err := Parse(args, sources(t, env, "")); err == nil {
			t.Errorf("%q must fail", args)
		}
	}
}

func TestMaxConns(t *testing.T) {
	cfg, err := Parse([]string{"--max-conns", "8", "h"}, sources(t, nil, ""))
	if err != nil || cfg.Runtime.MaxConns != 8 {
		t.Fatalf("max conns: %+v %v", cfg, err)
	}
	if _, err := Parse([]string{"--max-conns", "0", "h"}, sources(t, nil, "")); err == nil {
		t.Error("max conns 0 must fail")
	}
}

// TestDefaultMaxConns: 8 × (queue size + 1) by default, so that one source
// IP (an eighth of the connections) can fill the queue; at most 768.
func TestDefaultMaxConns(t *testing.T) {
	for _, c := range []struct {
		queueSize string
		want      int
	}{{"", 136}, {"0", 8}, {"63", 512}, {"94", 760}, {"95", 768}, {"96", 768}, {"100000", 768}} {
		args := []string{"h"}
		if c.queueSize != "" {
			args = []string{"--queue-size", c.queueSize, "h"}
		}
		cfg, err := Parse(args, sources(t, nil, ""))
		if err != nil || cfg.Runtime.MaxConns != c.want {
			t.Errorf("queue size %q: max conns %d, want %d (%v)", c.queueSize, cfg.Runtime.MaxConns, c.want, err)
		}
	}
	// An explicit value is kept, beyond the default cap too.
	cfg, err := Parse([]string{"--queue-size", "16", "--max-conns", "2000", "h"}, sources(t, nil, ""))
	if err != nil || cfg.Runtime.MaxConns != 2000 {
		t.Errorf("explicit max conns: %+v %v", cfg.Runtime.MaxConns, err)
	}
}

func TestChildEnvironmentRules(t *testing.T) {
	env := mapEnv{
		"LOCH_RUNTIME_TOKEN_FILE":    "/run/secrets/token",
		"AWS_LAMBDA_MAX_CONCURRENCY": "4",
		"AWS_REGION":                 "eu-west-3",
		"LAMBDA_TASK_ROOT":           "/custom",
	}
	cfg, err := Parse([]string{"h"}, sources(t, env, "268435456"))
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"AWS_LAMBDA_MAX_CONCURRENCY", "LOCH_RUNTIME_TOKEN_FILE", "LOCH_RUNTIME_AUTH"} {
		if _, n := envValue(cfg.ChildEnv, key); n != 0 {
			t.Errorf("%s must be removed", key)
		}
	}
	for key, want := range map[string]string{
		"AWS_DEFAULT_REGION":             "eu-west-3",
		"AWS_LAMBDA_INITIALIZATION_TYPE": "on-demand",
		"AWS_XRAY_CONTEXT_MISSING":       "LOG_ERROR",
		"LAMBDA_TASK_ROOT":               "/custom",
		"LAMBDA_RUNTIME_DIR":             "/var/runtime",
	} {
		if got, n := envValue(cfg.ChildEnv, key); n != 1 || got != want {
			t.Errorf("%s = %q (%d times), want %q", key, got, n, want)
		}
	}
	for _, key := range []string{"AWS_EXECUTION_ENV", "AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY", "AWS_SESSION_TOKEN"} {
		if _, n := envValue(cfg.ChildEnv, key); n != 0 {
			t.Errorf("%s must never be invented", key)
		}
	}
	joined := strings.Join(cfg.Warnings, "\n")
	if !strings.Contains(joined, "AWS_LAMBDA_MAX_CONCURRENCY removed") || !strings.Contains(joined, "256 MB") {
		t.Errorf("warnings = %q", cfg.Warnings)
	}
}

func TestWorkdirAndResponseLimit(t *testing.T) {
	cfg, err := Parse([]string{"--workdir", "/var/task", "h"}, sources(t, nil, ""))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Runtime.Workdir != "/var/task" || cfg.Runtime.MaxResponse() != 6291556 {
		t.Errorf("workdir = %q, max response = %d", cfg.Runtime.Workdir, cfg.Runtime.MaxResponse())
	}
	if _, err := Parse([]string{"--workdir", "task", "h"}, sources(t, nil, "")); err == nil {
		t.Error("relative workdir must fail")
	}
}

func TestEmptyInheritedVariablesAreDefaulted(t *testing.T) {
	cfg, err := Parse([]string{"h"}, sources(t, mapEnv{"AWS_REGION": " ", "AWS_LAMBDA_FUNCTION_NAME": ""}, ""))
	if err != nil {
		t.Fatal(err)
	}
	for key, want := range map[string]string{"AWS_REGION": "us-east-1", "AWS_LAMBDA_FUNCTION_NAME": "function"} {
		if got, n := envValue(cfg.ChildEnv, key); n != 1 || got != want {
			t.Errorf("%s = %q (%d times), want %q", key, got, n, want)
		}
	}
}

func TestLargeQueueBytes(t *testing.T) {
	for v, want := range map[string]int64{"2GiB": 2 << 30, "3GiB": 3 << 30, "64GiB": 64 << 30} {
		cfg, err := Parse([]string{"h"}, sources(t, mapEnv{"LOCH_RUNTIME_QUEUE_BYTES": v}, ""))
		if err != nil || cfg.Runtime.QueueBytes != want {
			t.Errorf("%s: %v %v", v, cfg, err)
		}
	}
	for _, v := range []string{"65GiB", "9223372036854775807GiB"} {
		if _, err := Parse([]string{"h"}, sources(t, mapEnv{"LOCH_RUNTIME_QUEUE_BYTES": v}, "")); err == nil {
			t.Errorf("%s must fail", v)
		}
	}
}

// TestEverySettingIsRead guards against a typo between the flag table and
// parseRuntime: each setting given an invalid value must be reported.
func TestEverySettingIsRead(t *testing.T) {
	invalid := map[string]string{
		"LOCH_RUNTIME_INVOKE_ADDR":       "nope",
		"LOCH_RUNTIME_API_ADDR":          "nope",
		"LOCH_RUNTIME_HEALTH_ADDR":       "0.0.0.0:8081",
		"LOCH_RUNTIME_BOOTSTRAP":         "relative",
		"LOCH_RUNTIME_WORKDIR":           "relative",
		"LOCH_RUNTIME_INIT_TIMEOUT":      "nope",
		"LOCH_RUNTIME_SHUTDOWN_GRACE":    "nope",
		"LOCH_RUNTIME_MAX_PAYLOAD":       "nope",
		"LOCH_RUNTIME_QUEUE_SIZE":        "nope",
		"LOCH_RUNTIME_QUEUE_BYTES":       "nope",
		"LOCH_RUNTIME_QUEUE_TIMEOUT":     "nope",
		"LOCH_RUNTIME_MAX_CONNS":         "nope",
		"LOCH_RUNTIME_AUTH":              "nope",
		"LOCH_RUNTIME_ALLOW_BROWSER":     "nope",
		"LOCH_RUNTIME_TOKEN_FILE":        "relative",
		"LOCH_RUNTIME_TOKEN_SHA256_FILE": "relative",
		"LOCH_RUNTIME_ACCESS_KEY_FILE":   "relative",
		"LOCH_RUNTIME_LOG_LEVEL":         "nope",
	}
	if len(invalid) != len(settings) {
		t.Fatalf("%d settings, %d tested", len(settings), len(invalid))
	}
	for _, s := range settings {
		v, ok := invalid[s.env]
		if !ok {
			t.Errorf("setting %s not covered", s.env)
			continue
		}
		_, err := Parse([]string{"h"}, sources(t, mapEnv{s.env: v}, ""))
		if err == nil || !strings.Contains(err.Error(), s.env) {
			t.Errorf("%s=%q: err = %v", s.env, v, err)
		}
	}
}

func TestParseDurationForms(t *testing.T) {
	for v, want := range map[string]time.Duration{"10": 10 * time.Second, "2.5": 2500 * time.Millisecond, "1m30s": 90 * time.Second, "0": 0} {
		if d, err := parseDuration(v, time.Hour); err != nil || d != want {
			t.Errorf("%s = %s %v", v, d, err)
		}
	}
	for _, v := range []string{"NaN", "Inf", "-1", "2h", "7201", "x"} {
		if _, err := parseDuration(v, time.Hour); err == nil {
			t.Errorf("%s must fail", v)
		}
	}
}

const testToken = "0123456789abcdef0123456789abcdef"

func TestCredentialsFromEnvironment(t *testing.T) {
	env := mapEnv{
		"LOCH_RUNTIME_AUTH":       "on",
		"LOCH_RUNTIME_TOKEN":      testToken,
		"LOCH_RUNTIME_ACCESS_KEY": "AKIDEXAMPLE:wJalrXUtnFEMI/K7MDENG",
	}
	cfg, err := Parse([]string{"h"}, sources(t, env, ""))
	if err != nil {
		t.Fatal(err)
	}
	a := cfg.Runtime.Auth
	if string(a.Token) != testToken || string(a.AccessKey) != "AKIDEXAMPLE:wJalrXUtnFEMI/K7MDENG" {
		t.Errorf("auth = %+v", a)
	}
	// Never printed.
	if s := fmt.Sprintf("%v %s %+v", a.Token, a.AccessKey, a); strings.Contains(s, testToken) || strings.Contains(s, "wJalr") {
		t.Errorf("secret printed: %s", s)
	}
	if a.Token.LogValue().String() != "[redacted]" || Secret("").String() != "" {
		t.Error("LogValue must redact")
	}
	// Removed from the child environment.
	for _, key := range SecretEnv {
		if _, n := envValue(cfg.ChildEnv, key); n != 0 {
			t.Errorf("%s leaked to the child", key)
		}
	}
}

func TestCredentialErrors(t *testing.T) {
	cases := []struct {
		env  mapEnv
		want string
	}{
		{mapEnv{"LOCH_RUNTIME_TOKEN": testToken, "LOCH_RUNTIME_TOKEN_FILE": "/t"}, "several sources"},
		{mapEnv{"LOCH_RUNTIME_TOKEN_SHA256": strings.Repeat("a", 64), "LOCH_RUNTIME_TOKEN_SHA256_FILE": "/h"}, "several sources"},
		{mapEnv{"LOCH_RUNTIME_TOKEN_SHA256": "abc"}, "64 hexadecimal"},
		{mapEnv{"LOCH_RUNTIME_TOKEN_SHA256": strings.Repeat("z", 64)}, "64 hexadecimal"},
		{mapEnv{"LOCH_RUNTIME_ACCESS_KEY": "a:b", "LOCH_RUNTIME_ACCESS_KEY_FILE": "/k"}, "both set"},
		{mapEnv{"LOCH_RUNTIME_TOKEN": "short"}, "at least 32"},
		{mapEnv{"LOCH_RUNTIME_TOKEN": testToken + " x"}, "printable"},
		{mapEnv{"LOCH_RUNTIME_ACCESS_KEY": "nocolon"}, "<access key id>:<secret>"},
		{mapEnv{"LOCH_RUNTIME_ACCESS_KEY": ":secret"}, "<access key id>:<secret>"},
	}
	for _, c := range cases {
		_, err := Parse([]string{"h"}, sources(t, c.env, ""))
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%v: err = %v, want %q", c.env, err, c.want)
		}
		if err != nil && strings.Contains(err.Error(), testToken) {
			t.Errorf("secret in error: %v", err)
		}
	}
	id, secret, err := ParseAccessKey(" id:sec ")
	if err != nil || id != "id" || secret != "sec" {
		t.Errorf("ParseAccessKey = %q %q %v", id, secret, err)
	}
}

func TestLogFormat(t *testing.T) {
	for v, want := range map[string]bool{"": false, "Text": false, "JSON": true, "json": true} {
		cfg, err := Parse([]string{"h"}, sources(t, mapEnv{"AWS_LAMBDA_LOG_FORMAT": v}, ""))
		if err != nil || cfg.Function.LogJSON != want {
			t.Errorf("%q: %v %v", v, cfg, err)
		}
	}
	if _, err := Parse([]string{"h"}, sources(t, mapEnv{"AWS_LAMBDA_LOG_FORMAT": "xml"}, "")); err == nil {
		t.Error("xml must fail")
	}
}

func TestSecretNeverPrinted(t *testing.T) {
	a := Auth{Token: testToken, AccessKey: "id:" + testToken}
	b, err := json.Marshal(a)
	if err != nil {
		t.Fatal(err)
	}
	for _, out := range []string{fmt.Sprintf("%#v", a), fmt.Sprintf("%+v", a), fmt.Sprint(a), string(b)} {
		if strings.Contains(out, testToken) {
			t.Errorf("secret printed: %s", out)
		}
	}
}

func TestChildSeesTrimmedValues(t *testing.T) {
	cfg, err := Parse([]string{"h"}, sources(t, mapEnv{"AWS_LAMBDA_FUNCTION_NAME": " fn ", "AWS_REGION": "eu-west-3 "}, ""))
	if err != nil {
		t.Fatal(err)
	}
	if v, _ := envValue(cfg.ChildEnv, "AWS_LAMBDA_FUNCTION_NAME"); v != "fn" || cfg.Function.Name != "fn" {
		t.Errorf("name: child %q, runtime %q", v, cfg.Function.Name)
	}
	if v, _ := envValue(cfg.ChildEnv, "AWS_REGION"); v != "eu-west-3" {
		t.Errorf("region: child %q", v)
	}
}

// TestSettingsLandInTheirField sets every setting to a distinct valid
// value, by environment then by flag, and checks the destination field.
func TestSettingsLandInTheirField(t *testing.T) {
	values := map[string]string{
		"invoke-addr":       "127.0.0.1:8081",
		"runtime-api-addr":  "127.0.0.2:9002",
		"health-addr":       "127.0.0.3:8082",
		"bootstrap":         "/b",
		"workdir":           "/w",
		"init-timeout":      "11",
		"shutdown-grace":    "12",
		"max-payload":       "2048",
		"queue-size":        "13",
		"queue-bytes":       "4096",
		"queue-timeout":     "14",
		"max-conns":         "15",
		"auth":              "off",
		"allow-browser":     "true",
		"token-file":        "/t",
		"token-sha256-file": "/h",
		"access-key-file":   "/k",
		"log-level":         "debug",
	}
	check := func(t *testing.T, rt Runtime) {
		t.Helper()
		want := Runtime{
			InvokeAddr: "127.0.0.1:8081", RuntimeAPIAddr: "127.0.0.2:9002", HealthAddr: "127.0.0.3:8082", Bootstrap: "/b", Workdir: "/w",
			InitTimeout: 11 * time.Second, ShutdownGrace: 12 * time.Second, MaxPayload: 2048, QueueSize: 13,
			QueueBytes: 4096, QueueTimeout: 14 * time.Second, MaxConns: 15, AllowBrowser: true, LogLevel: "debug",
			Auth: Auth{Enabled: false, TokenFile: "/t", AccessKeyFile: "/k"},
		}
		if rt != want {
			t.Errorf("runtime =\n%+v\nwant\n%+v", rt, want)
		}
	}
	env := mapEnv{}
	var args []string
	for _, s := range settings {
		v, ok := values[s.flag]
		if !ok {
			t.Fatalf("no test value for --%s", s.flag)
		}
		if s.flag == "token-sha256-file" {
			continue // another bearer source than token-file: checked below
		}
		env[s.env] = v
		args = append(args, "--"+s.flag+"="+v)
	}
	cfg, err := Parse([]string{"h"}, sources(t, env, ""))
	if err != nil {
		t.Fatal(err)
	}
	check(t, cfg.Runtime)

	// The same values by flag win over different environment values.
	other := mapEnv{}
	for _, s := range settings {
		if s.flag != "token-sha256-file" {
			other[s.env] = "garbage that would fail"
		}
	}
	cfg, err = Parse(append(args, "h"), sources(t, other, ""))
	if err != nil {
		t.Fatal(err)
	}
	check(t, cfg.Runtime)

	cfg, err = Parse([]string{"--token-sha256-file", values["token-sha256-file"], "h"}, sources(t, nil, ""))
	if err != nil || cfg.Runtime.Auth.TokenSHA256File != "/h" {
		t.Errorf("token-sha256-file: %+v %v", cfg, err)
	}
}

func TestTokenSHA256(t *testing.T) {
	sum := strings.Repeat("ab", 32)
	cfg, err := Parse([]string{"h"}, sources(t, mapEnv{"LOCH_RUNTIME_AUTH": "on", "LOCH_RUNTIME_TOKEN_SHA256": sum}, ""))
	if err != nil {
		t.Fatal(err)
	}
	a := cfg.Runtime.Auth
	if a.TokenSHA256 != sum || !a.HasBearer() || a.HasAccessKey() {
		t.Errorf("auth = %+v", a)
	}
	if _, n := envValue(cfg.ChildEnv, "LOCH_RUNTIME_TOKEN_SHA256"); n != 0 {
		t.Error("hash passed to the child")
	}
	got, err := ParseTokenSHA256(sum)
	if err != nil || got[0] != 0xab {
		t.Errorf("parse = %x %v", got, err)
	}
}

// TestSwitches: on/true and off/false, in any case; browsers refused by
// default.
func TestSwitches(t *testing.T) {
	for v, want := range map[string]bool{"on": true, "TRUE": true, "true": true, "off": false, "False": false} {
		cfg, err := Parse([]string{"h"}, sources(t, mapEnv{"LOCH_RUNTIME_ALLOW_BROWSER": v, "LOCH_RUNTIME_AUTH": "off"}, ""))
		if err != nil || cfg.Runtime.AllowBrowser != want {
			t.Errorf("%s: %v %v", v, cfg.Runtime.AllowBrowser, err)
		}
	}
	cfg, err := Parse([]string{"h"}, sources(t, mapEnv{"LOCH_RUNTIME_AUTH": "false"}, ""))
	if err != nil || cfg.Runtime.Auth.Enabled || cfg.Runtime.AllowBrowser {
		t.Errorf("defaults: %+v %v", cfg.Runtime, err)
	}
}
