// Copyright 2026 The Loch.sh Authors
// SPDX-License-Identifier: Apache-2.0

//go:build integration

package integration

import (
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"
)

// forEachImage runs f on every image, in parallel.
func forEachImage(t *testing.T, f func(t *testing.T, img image)) {
	for _, img := range images {
		t.Run(img.name, func(t *testing.T) {
			t.Parallel()
			f(t, img)
		})
	}
}

func TestSuccessAndFunctionError(t *testing.T) {
	forEachImage(t, func(t *testing.T, img image) {
		c := start(t, img, runOptions{})
		c.waitHealth("ready")
		r := c.invoke(`{"hello":"world"}`)
		var echoed struct{ Event map[string]string }
		_ = json.Unmarshal(r.body, &echoed)
		if r.status != 200 || r.functionError != "" || echoed.Event["hello"] != "world" {
			t.Fatalf("success: %s", r)
		}
		if !strings.Contains(r.logTail, "START RequestId:") || !strings.Contains(r.logTail, "REPORT RequestId:") {
			t.Errorf("tail = %q", r.logTail)
		}
		r = c.invoke(`{"op":"throw"}`)
		if r.status != 200 || r.functionError != "Unhandled" || !strings.Contains(string(r.body), "boom from handler") {
			t.Fatalf("throw: %s", r)
		}
		if r.header.Get("X-Amzn-Requestid") == "" || r.header.Get("X-Amz-Executed-Version") != "$LATEST" {
			t.Errorf("headers = %v", r.header)
		}
	})
}

func TestTimeoutThenReinit(t *testing.T) {
	forEachImage(t, func(t *testing.T, img image) {
		c := start(t, img, runOptions{})
		c.waitHealth("ready")
		r := c.invoke(`{"op":"sleep","ms":10000}`)
		if r.status != 200 || r.functionError != "Unhandled" || r.errorType() != "Sandbox.Timedout" ||
			!strings.Contains(string(r.body), "Task timed out after 3.00 seconds") {
			t.Fatalf("timeout: %s", r)
		}
		if !strings.Contains(r.logTail, "Status: timeout") {
			t.Errorf("tail = %q", r.logTail)
		}
		// Re-inited: served by a new bootstrap.
		if r := c.invoke(`{"op":"echo"}`); r.status != 200 || r.functionError != "" {
			t.Fatalf("after timeout: %s", r)
		}
		c.waitHealth("ready")
	})
}

func TestOOMThenReinit(t *testing.T) {
	forEachImage(t, func(t *testing.T, img image) {
		c := start(t, img, runOptions{memory: "128m"})
		c.waitHealth("ready")
		r := c.invoke(`{"op":"oom"}`)
		if r.status != 200 || r.functionError != "Unhandled" || r.errorType() != "Runtime.OutOfMemory" {
			t.Fatalf("oom: %s", r)
		}
		if r := c.invoke(`{"op":"echo"}`); r.status != 200 || r.functionError != "" {
			t.Fatalf("after oom: %s", r)
		}
		// PID 1 survived the OOM: the container still runs.
		if out, err := c.dockerExec("/loch/lambda-runtime", "health"); err != nil {
			t.Errorf("health after oom: %v %s", err, out)
		}
	})
}

func TestExitDuringInvocationAndWhileIdle(t *testing.T) {
	forEachImage(t, func(t *testing.T, img image) {
		c := start(t, img, runOptions{})
		c.waitHealth("ready")
		r := c.invoke(`{"op":"exit"}`)
		if r.status != 200 || r.errorType() != "Runtime.ExitError" || !strings.Contains(string(r.body), "exit status 3") {
			t.Fatalf("exit: %s", r)
		}
		// The response is posted, then the process exits while idle: the
		// response wins, and the next invocation is served after a re-init.
		r = c.invoke(`{"op":"exit-after-response"}`)
		if r.status != 200 || r.functionError != "" || !strings.Contains(string(r.body), "bye") {
			t.Fatalf("exit after response: %s", r)
		}
		for i := range 3 {
			if r := c.invoke(`{"op":"echo"}`); r.status != 200 || r.functionError != "" {
				t.Fatalf("invocation %d after the idle exit: %s", i, r)
			}
		}
		if logs := c.logs(); !strings.Contains(logs, "bootstrap exited") {
			t.Errorf("no exit logged:\n%s", logs)
		}
	})
}

// TestChildCannotReadOurEnvironment checks Harden(): the function code
// runs under our uid but cannot read /proc/1/environ, where the
// credentials were.
func TestChildCannotReadOurEnvironment(t *testing.T) {
	forEachImage(t, func(t *testing.T, img image) {
		c := start(t, img, runOptions{env: []string{"LOCH_RUNTIME_TOKEN=0123456789abcdef0123456789abcdef"}})
		c.waitHealth("ready")
		r := c.invoke(`{"op":"environ"}`)
		var doc struct {
			Readable bool
			Code     any
		}
		if r.status != 200 || r.functionError != "" || json.Unmarshal(r.body, &doc) != nil {
			t.Fatalf("environ: %s", r)
		}
		// EACCES: "EACCES" for Node.js, errno 13 for Python.
		if doc.Readable || (doc.Code != "EACCES" && doc.Code != float64(13)) {
			t.Fatalf("/proc/1/environ: %s", r)
		}
		r = c.invoke(`{"op":"echo"}`)
		var echo struct {
			RuntimeAPI string `json:"runtimeApi"`
			Token      *string
		}
		if r.status != 200 || json.Unmarshal(r.body, &echo) != nil || echo.RuntimeAPI != "127.0.0.1:9001" || echo.Token != nil {
			t.Errorf("child environment: %s", r)
		}
		// docker exec and the healthcheck still work on a non-dumpable PID 1.
		if out, err := c.dockerExec("/loch/lambda-runtime", "health"); err != nil || !strings.Contains(out, "ready") {
			t.Errorf("health: %v %s", err, out)
		}
	})
}

// TestFiftyParallelInvocations is the scenario that crashes the RIE: every
// request is served or throttled (429), and the runtime survives.
func TestFiftyParallelInvocations(t *testing.T) {
	forEachImage(t, func(t *testing.T, img image) {
		c := start(t, img, runOptions{})
		c.waitHealth("ready")
		var mu sync.Mutex
		counts := map[int]int{}
		var wg sync.WaitGroup
		for range 50 {
			wg.Go(func() {
				r, err := c.tryInvoke(`{"op":"sleep","ms":100}`)
				if err != nil {
					t.Errorf("transport error: %v", err)
					return
				}
				mu.Lock()
				counts[r.status]++
				mu.Unlock()
				if r.status == 200 && r.functionError != "" {
					t.Errorf("function error: %s", r)
				}
				if r.status == 429 && r.header.Get("Retry-After") == "" {
					t.Error("429 without Retry-After")
				}
			})
		}
		wg.Wait()
		// One source IP may fill the queue: 1 running + 16 queued are
		// served, the rest is throttled, never dropped.
		if counts[200]+counts[429] != 50 || counts[200] < 17 {
			t.Errorf("statuses = %v", counts)
		}
		if r := c.invoke(`{"op":"echo"}`); r.status != 200 {
			t.Fatalf("after the burst: %s", r)
		}
		t.Logf("statuses: %v", counts)
	})
}

// TestSigtermDuringInvocation: the running invocation finishes within the
// grace period, a queued one gets 503, then the container exits 0.
func TestSigtermDuringInvocation(t *testing.T) {
	forEachImage(t, func(t *testing.T, img image) {
		c := start(t, img, runOptions{})
		c.waitHealth("ready")
		running := make(chan response, 1)
		queued := make(chan response, 1)
		go func() { running <- c.invoke(`{"op":"sleep","ms":1500}`) }()
		time.Sleep(200 * time.Millisecond)
		go func() { queued <- c.invoke(`{"op":"echo"}`) }()
		time.Sleep(200 * time.Millisecond)
		docker(t, "kill", "--signal", "TERM", c.name)
		if r := <-running; r.status != 200 || r.functionError != "" {
			t.Fatalf("running invocation: %s", r)
		}
		if r := <-queued; r.status != 503 || r.header.Get("X-Amzn-Errortype") != "ServiceException" || r.header.Get("Retry-After") == "" {
			t.Fatalf("queued invocation: %s %v", r, r.header)
		}
		if out := strings.TrimSpace(docker(t, "wait", c.name)); out != "0" {
			t.Errorf("exit code = %s", out)
		}
	})
}
