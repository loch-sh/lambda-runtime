// Copyright 2026 The Loch.sh Authors
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"strconv"
	"strings"
)

// Variables of the child environment with a special treatment.
const (
	envRuntimeAPI     = "AWS_LAMBDA_RUNTIME_API"
	envMaxConcurrency = "AWS_LAMBDA_MAX_CONCURRENCY"
	// ownPrefix marks our settings, never passed to the child.
	ownPrefix = "LOCH_RUNTIME_"
)

// childEnv returns the environment of the child and the warnings to log:
//   - everything inherited is passed through, except
//     AWS_LAMBDA_MAX_CONCURRENCY, which would switch the AWS bootstraps to
//     their multi-concurrency client, and our own LOCH_RUNTIME_* settings,
//     which point at the Invoke API credentials;
//   - AWS_LAMBDA_RUNTIME_API always points at our Runtime API;
//   - the variables a Lambda runtime expects are added when absent or
//     empty, and passed trimmed when present, so that the child sees the
//     values the runtime validated and uses.
//
// Neither AWS_EXECUTION_ENV (exported by the AWS bootstraps) nor any
// credential is ever invented.
func childEnv(inherited []string, fn Function, runtimeAPIAddr string) (env, warnings []string) {
	defaults := []struct{ key, value string }{
		{"AWS_LAMBDA_FUNCTION_NAME", fn.Name},
		{"AWS_LAMBDA_FUNCTION_VERSION", fn.Version},
		{"AWS_LAMBDA_FUNCTION_MEMORY_SIZE", strconv.Itoa(fn.MemoryMB)},
		{"AWS_LAMBDA_LOG_GROUP_NAME", fn.LogGroupName},
		{"AWS_LAMBDA_LOG_STREAM_NAME", fn.LogStreamName},
		{"AWS_REGION", fn.Region},
		{"AWS_DEFAULT_REGION", fn.Region},
		{"AWS_LAMBDA_INITIALIZATION_TYPE", "on-demand"},
		{"AWS_XRAY_CONTEXT_MISSING", "LOG_ERROR"},
		{"LAMBDA_TASK_ROOT", "/var/task"},
		{"LAMBDA_RUNTIME_DIR", "/var/runtime"},
	}
	defaulted := make(map[string]bool, len(defaults))
	for _, d := range defaults {
		defaulted[d.key] = true
	}
	present := map[string]bool{}
	env = make([]string, 0, len(inherited)+len(defaults)+1)
	for _, kv := range inherited {
		key, value, _ := strings.Cut(kv, "=")
		switch {
		case key == envRuntimeAPI, strings.HasPrefix(key, ownPrefix):
			continue
		case key == envMaxConcurrency:
			warnings = append(warnings, envMaxConcurrency+" removed: one invocation at a time is served")
			continue
		case defaulted[key]:
			value = strings.TrimSpace(value)
			if value == "" {
				continue
			}
			kv = key + "=" + value
		}
		present[key] = true
		env = append(env, kv)
	}
	for _, d := range defaults {
		if !present[d.key] {
			env = append(env, d.key+"="+d.value)
		}
	}
	return append(env, envRuntimeAPI+"="+runtimeAPIAddr), warnings
}
