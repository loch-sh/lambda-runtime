// Copyright 2026 The Loch.sh Authors
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// Defaults of the function settings, as the RIE and AWS apply them.
const (
	DefaultFunctionName = "function"
	DefaultVersion      = "$LATEST"
	DefaultRegion       = "us-east-1"
	DefaultTimeout      = 300 * time.Second
	DefaultMemoryMB     = 3008

	// maxFunctionTimeout bounds AWS_LAMBDA_FUNCTION_TIMEOUT (AWS caps it
	// at 900 s; self-hosted functions may need more).
	maxFunctionTimeout = 24 * time.Hour
	maxMemoryMB        = 1 << 20
)

// memoryWarning returns a note to log when the memory size was derived
// rather than configured, "" otherwise.
func (f Function) memoryWarning() string {
	if f.MemorySource != MemoryFromCgroup {
		return ""
	}
	return "AWS_LAMBDA_FUNCTION_MEMORY_SIZE derived from the cgroup memory limit: " + strconv.Itoa(f.MemoryMB) + " MB"
}

// MemorySource tells where Function.MemoryMB comes from.
type MemorySource string

// Memory sources.
const (
	MemoryFromEnv     MemorySource = "env"
	MemoryFromCgroup  MemorySource = "cgroup"
	MemoryFromDefault MemorySource = "default"
)

// Function describes the Lambda function as seen by the runtime.
type Function struct {
	Name          string
	Version       string
	MemoryMB      int
	MemorySource  MemorySource
	Timeout       time.Duration
	Region        string
	LogGroupName  string
	LogStreamName string
	// LogJSON is set by AWS_LAMBDA_LOG_FORMAT=JSON: platform log lines
	// (START, END, REPORT) are then written as JSON records.
	LogJSON bool
}

// ARN returns the fake but well-formed function ARN given to the runtime.
func (f Function) ARN() string {
	return "arn:aws:lambda:" + f.Region + ":000000000000:function:" + f.Name
}

// parseFunction reads the AWS_* variables describing the function.
func parseFunction(src Sources) (Function, []error) {
	var errs []error
	env := func(k string) string {
		v, _ := src.Env.Lookup(k)
		return strings.TrimSpace(v)
	}
	fn := Function{
		Name:    orDefault(env("AWS_LAMBDA_FUNCTION_NAME"), DefaultFunctionName),
		Version: orDefault(env("AWS_LAMBDA_FUNCTION_VERSION"), DefaultVersion),
		Region:  orDefault(env("AWS_REGION"), orDefault(env("AWS_DEFAULT_REGION"), DefaultRegion)),
	}
	if !validFunctionName(fn.Name) {
		errs = append(errs, fmt.Errorf("AWS_LAMBDA_FUNCTION_NAME: %q must be 1-64 letters, digits, '-' or '_'", fn.Name))
	}
	if !validToken(fn.Version) {
		errs = append(errs, fmt.Errorf("AWS_LAMBDA_FUNCTION_VERSION: %q is not a valid version", fn.Version))
	}
	if !validToken(fn.Region) {
		errs = append(errs, fmt.Errorf("AWS_REGION: %q is not a valid region", fn.Region))
	}

	var err error
	fn.Timeout, err = functionTimeout(env("AWS_LAMBDA_FUNCTION_TIMEOUT"))
	errs = appendIf(errs, err)
	fn.MemoryMB, fn.MemorySource, err = functionMemory(env("AWS_LAMBDA_FUNCTION_MEMORY_SIZE"), src.CgroupMemoryMax)
	errs = appendIf(errs, err)

	switch strings.ToUpper(env("AWS_LAMBDA_LOG_FORMAT")) {
	case "", "TEXT":
	case "JSON":
		fn.LogJSON = true
	default:
		errs = append(errs, fmt.Errorf("AWS_LAMBDA_LOG_FORMAT: %q is neither Text nor JSON", env("AWS_LAMBDA_LOG_FORMAT")))
	}
	fn.LogGroupName = orDefault(env("AWS_LAMBDA_LOG_GROUP_NAME"), "/aws/lambda/"+fn.Name)
	fn.LogStreamName = env("AWS_LAMBDA_LOG_STREAM_NAME")
	if fn.LogStreamName == "" {
		fn.LogStreamName = logStreamName(src.Now(), fn.Version)
	}
	return fn, errs
}

// functionTimeout parses AWS_LAMBDA_FUNCTION_TIMEOUT, in seconds.
func functionTimeout(v string) (time.Duration, error) {
	if v == "" {
		return DefaultTimeout, nil
	}
	secs, err := strconv.ParseFloat(v, 64)
	if err != nil || !(secs > 0 && secs <= maxFunctionTimeout.Seconds()) {
		return DefaultTimeout, fmt.Errorf("AWS_LAMBDA_FUNCTION_TIMEOUT: %q must be a positive number of seconds up to %d",
			v, int(maxFunctionTimeout.Seconds()))
	}
	return time.Duration(secs * float64(time.Second)), nil
}

// functionMemory resolves the memory size: the environment, else the cgroup
// limit, else the AWS default.
func functionMemory(v, cgroupMemoryMax string) (int, MemorySource, error) {
	if v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > maxMemoryMB {
			return DefaultMemoryMB, MemoryFromDefault,
				fmt.Errorf("AWS_LAMBDA_FUNCTION_MEMORY_SIZE: %q must be a positive number of MB", v)
		}
		return n, MemoryFromEnv, nil
	}
	if mb, ok := cgroupMemoryMB(cgroupMemoryMax); ok {
		return mb, MemoryFromCgroup, nil
	}
	return DefaultMemoryMB, MemoryFromDefault, nil
}

// cgroupMemoryMB reads a cgroup v2 memory.max file. "max" (no limit), a
// missing file or garbage yield ok=false.
func cgroupMemoryMB(path string) (int, bool) {
	if path == "" {
		return 0, false
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}
	n, err := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
	if err != nil || n < 1<<20 {
		return 0, false
	}
	return int(n >> 20), true
}

// logStreamName builds a name shaped like AWS's: 2026/11/06/[$LATEST]<hex>.
func logStreamName(now time.Time, version string) string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return now.UTC().Format("2006/01/02") + "/[" + version + "]" + hex.EncodeToString(b[:])
}
