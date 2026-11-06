// Copyright 2026 The Loch.sh Authors
// SPDX-License-Identifier: Apache-2.0

//go:build integration

package integration

import (
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// moreImages are the other supported images: the other runtime versions
// on mirror.gcr.io, and the multi-arch public.ecr.aws ones.
var moreImages = []image{
	{"node24", "mirror.gcr.io/amazon/aws-lambda-nodejs:24-" + awsArch(), "node", "index.handler"},
	{"node26", "mirror.gcr.io/amazon/aws-lambda-nodejs:26-" + awsArch(), "node", "index.handler"},
	{"python312", "mirror.gcr.io/amazon/aws-lambda-python:3.12-" + awsArch(), "python", "app.handler"},
	{"python314", "mirror.gcr.io/amazon/aws-lambda-python:3.14-" + awsArch(), "python", "app.handler"},
	{"ecr-node22", "public.ecr.aws/lambda/nodejs:22", "node", "index.handler"},
	{"ecr-python313", "public.ecr.aws/lambda/python:3.13", "python", "app.handler"},
}

func TestEveryImage(t *testing.T) {
	for _, img := range moreImages {
		t.Run(img.name, func(t *testing.T) {
			t.Parallel()
			c := start(t, img, runOptions{})
			c.waitHealth("ready")
			var echoed struct{ Event map[string]string }
			r := c.invoke(`{"hello":"world"}`)
			if r.status != 200 || r.functionError != "" || json.Unmarshal(r.body, &echoed) != nil || echoed.Event["hello"] != "world" {
				t.Fatalf("success: %s", r)
			}
			if r := c.invoke(`{"op":"throw"}`); r.functionError != "Unhandled" {
				t.Fatalf("throw: %s", r)
			}
		})
	}
}

func TestInitErrors(t *testing.T) {
	cases := []struct {
		img     image
		handler string
		want    string
	}{
		{images[0], "missing.handler", "Runtime.ImportModuleError"},
		{images[0], "index.nope", "Runtime.HandlerNotFound"},
		{images[1], "missing.handler", "Runtime.ImportModuleError"},
		{images[1], "app.nope", "Runtime.HandlerNotFound"},
	}
	for _, c := range cases {
		t.Run(c.img.name+"/"+c.handler, func(t *testing.T) {
			t.Parallel()
			ct := start(t, c.img, runOptions{cmd: []string{c.handler}})
			ct.waitHealth("error")
			r := ct.invoke(`{}`)
			if r.status != 200 || r.functionError != "Unhandled" || r.errorType() != c.want {
				t.Fatalf("init error: %s", r)
			}
		})
	}
}

// TestSlowInit: init longer than the init timeout goes on, and the first
// invocation waits for it (the init timeout is lowered to keep the test
// short; the default is 10 s, as on Lambda).
func TestSlowInit(t *testing.T) {
	forEachImage(t, func(t *testing.T, img image) {
		c := start(t, img, runOptions{env: []string{"INIT_SLEEP_MS=2000", "LOCH_RUNTIME_INIT_TIMEOUT=1"}})
		time.Sleep(1200 * time.Millisecond) // past the init timeout
		if r := c.invoke(`{"op":"echo"}`); r.status != 200 || r.functionError != "" {
			t.Fatalf("after a slow init: %s", r)
		}
		if logs := c.logs(); !strings.Contains(logs, "charged to the first invocation") {
			t.Errorf("no suppressed-init log:\n%s", logs)
		}
	})
}

func TestPayloadLimits(t *testing.T) {
	forEachImage(t, func(t *testing.T, img image) {
		c := start(t, img, runOptions{})
		c.waitHealth("ready")
		event := make([]byte, 6<<20+1)
		r := c.invokeWith("POST", invokePath, event, nil)
		if r.status != 413 || r.header.Get("X-Amzn-Errortype") != "RequestTooLargeException" {
			t.Fatalf("6 MiB + 1: %d %s", r.status, r.body)
		}
		r = c.invoke(`{"op":"big-response"}`)
		if r.status != 200 || r.functionError != "Unhandled" || r.errorType() != "Function.ResponseSizeTooLarge" {
			t.Fatalf("big response: %s", r)
		}
		// The runtime survives (re-inited if its client died on the 413).
		if r := c.invoke(`{"op":"echo"}`); r.status != 200 || r.functionError != "" {
			t.Fatalf("after the big response: %s", r)
		}
	})
}

const invokePath = "/2015-03-31/functions/function/invocations"

func TestMethodsAndNames(t *testing.T) {
	c := start(t, images[0], runOptions{})
	c.waitHealth("ready")
	for _, m := range []string{"GET", "PUT", "DELETE", "PATCH"} {
		if r := c.invokeWith(m, invokePath, nil, nil); r.status != 405 {
			t.Errorf("%s: %d", m, r.status)
		}
	}
	if r := c.invokeWith("POST", "/2015-03-31/functions/other/invocations", []byte(`{}`), nil); r.status != 404 {
		t.Errorf("unknown function: %d", r.status)
	}
	arn := "/2015-03-31/functions/arn:aws:lambda:us-east-1:000000000000:function:function/invocations"
	if r := c.invokeWith("POST", arn, []byte(`{}`), nil); r.status != 200 {
		t.Errorf("ARN: %d %s", r.status, r.body)
	}
	if r := c.invokeWith("POST", invokePath, []byte(`{}`), http.Header{"X-Amz-Invocation-Type": {"DryRun"}}); r.status != 204 {
		t.Errorf("DryRun: %d", r.status)
	}
}

func TestStreamifyResponse(t *testing.T) {
	c := start(t, images[0], runOptions{cmd: []string{"stream.handler"}})
	c.waitHealth("ready")
	r := c.invoke(`{}`)
	if r.status != 200 || r.functionError != "" || string(r.body) != "one two three" {
		t.Fatalf("stream: %s", r)
	}
	r = c.invoke(`{"fail":true}`)
	if r.status != 200 || r.functionError != "Unhandled" {
		t.Fatalf("failing stream: %s", r)
	}
	if r := c.invoke(`{}`); r.status != 200 || string(r.body) != "one two three" {
		t.Fatalf("after a failing stream: %s", r)
	}
}

// goBootstrap builds the Go test function as a static bootstrap.
func goBootstrap(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	cmd := exec.Command("go", "build", "-tags", "lambda.norpc", "-o", filepath.Join(dir, "bootstrap"), ".")
	cmd.Dir = filepath.Join("testdata", "gofunc")
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux", "GOARCH="+runtime.GOARCH)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}

// TestProvidedAL2023 runs a Go bootstrap on provided.al2023, with the
// image entrypoint after "--" and with a relative ./bootstrap entrypoint.
func TestProvidedAL2023(t *testing.T) {
	code := goBootstrap(t)
	img := image{"provided", "public.ecr.aws/lambda/provided:al2023", "", "bootstrap"}
	for name, cmd := range map[string][]string{
		"entrypoint script": {"--", "/lambda-entrypoint.sh", "bootstrap"},
		"relative program":  {"--", "./bootstrap"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			c := start(t, img, runOptions{code: code, cmd: cmd})
			c.waitHealth("ready")
			r := c.invoke(`{"name":"go"}`)
			var out map[string]string
			if r.status != 200 || json.Unmarshal(r.body, &out) != nil || out["hello"] != "go" || out["requestId"] == "" {
				t.Fatalf("go function: %s", r)
			}
			r = c.invoke(`{"op":"throw"}`)
			var fnErr struct{ ErrorType, ErrorMessage string }
			if r.status != 200 || r.functionError != "Unhandled" || json.Unmarshal(r.body, &fnErr) != nil ||
				fnErr.ErrorType != "errorString" || fnErr.ErrorMessage != "thrown on purpose" {
				t.Fatalf("go function error: %s", r)
			}
		})
	}
}

// TestProvidedBootstrapErrors: what happens to a provided.al2023 function
// whose bootstrap cannot run. The image's /var/runtime/bootstrap execs
// /var/task/bootstrap (or /opt/bootstrap) and reports
// Runtime.InvalidEntrypoint otherwise; the runtime reports it to the
// invoker as a function error and retries the init with a backoff (the
// init error answers every invocation, retry or not).
func TestProvidedBootstrapErrors(t *testing.T) {
	img := image{"provided", "public.ecr.aws/lambda/provided:al2023", "", "bootstrap"}
	for name, c := range map[string]struct {
		code             string
		errorType, cause string
	}{
		"missing":        {codeDir(t, "", 0), "Runtime.InvalidEntrypoint", "no bootstrap found"},
		"not executable": {codeDir(t, "#!/bin/sh\nexit 0\n", 0o644), "Runtime.InvalidEntrypoint", "failed to exec /var/task/bootstrap"},
		"exits at init":  {codeDir(t, "#!/bin/sh\nexit 3\n", 0o755), "Runtime.ExitError", "exit status 3"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			ct := start(t, img, runOptions{code: c.code})
			ct.waitHealth("error")
			r := ct.invoke(`{}`)
			var out struct{ ErrorType, ErrorMessage string }
			if r.status != 200 || r.functionError != "Unhandled" || json.Unmarshal(r.body, &out) != nil ||
				out.ErrorType != c.errorType || !strings.Contains(out.ErrorMessage, c.cause) {
				t.Fatalf("invoke: %s", r)
			}
		})
	}
}

// codeDir returns a code directory holding a bootstrap with content and
// mode (none when content is ""). The directory is 0755: t.TempDir is
// 0700, which uid 10001 cannot enter.
func codeDir(t *testing.T, content string, mode os.FileMode) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if content != "" {
		path := filepath.Join(dir, "bootstrap")
		if err := os.WriteFile(path, []byte(content), mode); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(path, mode); err != nil { // not filtered by the umask
			t.Fatal(err)
		}
	}
	return dir
}

// TestSharpResize is the use case of the spike: an image resized with
// sharp (node_modules built for linux-arm64 in the spike).
func TestSharpResize(t *testing.T) {
	code := "/private/tmp/claude-501/-Users-nicolas-Developer-host-panel-group/d5fbeb13-4c0d-406e-b1d8-de5896c9f47e/scratchpad/fn-spike/code-sharp"
	if _, err := os.Stat(filepath.Join(code, "node_modules", "sharp")); err != nil || runtime.GOARCH != "arm64" {
		t.Skip("spike sharp code not available for this architecture")
	}
	c := start(t, images[0], runOptions{code: code, env: []string{"AWS_LAMBDA_FUNCTION_TIMEOUT=30"}})
	c.waitHealth("ready")
	r := c.invoke(`{"file":"big.jpg","width":800}`)
	var out struct{ OutSize string }
	if r.status != 200 || r.functionError != "" || json.Unmarshal(r.body, &out) != nil || !strings.HasPrefix(out.OutSize, "800x") {
		t.Fatalf("resize: %s", r)
	}
}

// TestJSONLogFormat: with AWS_LAMBDA_LOG_FORMAT=JSON, the platform lines
// are JSON records (platform.start, platform.runtimeDone, platform.report).
func TestJSONLogFormat(t *testing.T) {
	forEachImage(t, func(t *testing.T, img image) {
		c := start(t, img, runOptions{env: []string{"AWS_LAMBDA_LOG_FORMAT=JSON"}})
		c.waitHealth("ready")
		r := c.invoke(`{"op":"echo"}`)
		if r.status != 200 {
			t.Fatalf("invoke: %s", r)
		}
		requestID := r.header.Get("X-Amzn-Requestid")
		types := map[string]bool{}
		for line := range strings.SplitSeq(r.logTail, "\n") {
			var rec struct {
				Time   string
				Type   string
				Record map[string]any
			}
			if json.Unmarshal([]byte(line), &rec) != nil || !strings.HasPrefix(rec.Type, "platform.") {
				continue
			}
			if rec.Record["requestId"] != requestID || rec.Time == "" {
				t.Errorf("record %s", line)
			}
			types[rec.Type] = true
			if rec.Type == "platform.report" {
				metrics, _ := rec.Record["metrics"].(map[string]any)
				if metrics["durationMs"] == nil || metrics["memorySizeMB"] == nil || rec.Record["status"] != "success" {
					t.Errorf("report %s", line)
				}
			}
		}
		for _, want := range []string{"platform.start", "platform.runtimeDone", "platform.report"} {
			if !types[want] {
				t.Errorf("no %s record in:\n%s", want, r.logTail)
			}
		}
		if strings.Contains(r.logTail, "START RequestId") {
			t.Errorf("text platform lines in JSON mode:\n%s", r.logTail)
		}
	})
}
