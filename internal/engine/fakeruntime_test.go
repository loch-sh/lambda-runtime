// Copyright 2026 The Loch.sh Authors
// SPDX-License-Identifier: Apache-2.0

package engine

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// fakeRuntime is a minimal Lambda runtime client, run when the test binary
// is started with FAKE_RUNTIME=1. FAKE_INIT selects the init behavior:
// "" (ok), "error" (posts /init/error, exits 1), "exit" (exits 2),
// "slow:<ms>" (sleeps, then ok). Each event selects what the handler does.
func fakeRuntime() {
	api := "http://" + os.Getenv("AWS_LAMBDA_RUNTIME_API") + "/2018-06-01/runtime/"
	switch init := os.Getenv("FAKE_INIT"); {
	case init == "error":
		post(api+"init/error", `{"errorType":"Runtime.ImportModuleError","errorMessage":"no module"}`,
			"Runtime.ImportModuleError")
		os.Exit(1)
	case init == "exit":
		os.Exit(2)
	case init == "slow-error":
		time.Sleep(300 * time.Millisecond)
		post(api+"init/error", `{"errorType":"Runtime.ImportModuleError","errorMessage":"no module"}`,
			"Runtime.ImportModuleError")
		os.Exit(1)
	case strings.HasPrefix(init, "slow:"):
		ms, _ := strconv.Atoi(strings.TrimPrefix(init, "slow:"))
		time.Sleep(time.Duration(ms) * time.Millisecond)
	}
	for {
		resp, err := http.Get(api + "invocation/next")
		if err != nil {
			os.Exit(10)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			os.Exit(11)
		}
		handle(api+"invocation/"+resp.Header.Get("Lambda-Runtime-Aws-Request-Id")+"/", body)
	}
}

type fakeEvent struct {
	Op string `json:"op"`
	Ms int    `json:"ms"`
	N  int    `json:"n"`
}

func handle(base string, body []byte) {
	var ev fakeEvent
	_ = json.Unmarshal(body, &ev)
	switch ev.Op {
	case "fail":
		post(base+"error", `{"errorType":"MyError","errorMessage":"boom"}`, "")
	case "sleep":
		time.Sleep(time.Duration(ev.Ms) * time.Millisecond)
		post(base+"response", `"slept"`, "")
	case "exit":
		fmt.Println("bye")
		os.Exit(3)
	case "oom":
		// Simulates the OOM killer: bump the cgroup counter, then SIGKILL.
		path := os.Getenv("FAKE_MEMORY_EVENTS")
		_ = os.WriteFile(path, []byte("oom 1\noom_kill 1\n"), 0o600)
		_ = syscall.Kill(os.Getpid(), syscall.SIGKILL)
	case "oom-descendant":
		// The OOM killer took a descendant; the bootstrap (a shell) then
		// exits with 137 instead of dying from SIGKILL itself.
		_ = os.WriteFile(os.Getenv("FAKE_MEMORY_EVENTS"), []byte("oom 1\noom_kill 1\n"), 0o600)
		os.Exit(137)
	case "big":
		post(base+"response", strings.Repeat("x", ev.N), "")
	case "print":
		fmt.Println("hello from the handler")
		fmt.Fprintln(os.Stderr, "warning from the handler")
		post(base+"response", `"printed"`, "")
	case "respond-then-exit":
		post(base+"response", `"done"`, "")
		os.Exit(4)
	default:
		post(base+"response", string(body), "")
	}
}

func post(url, body, errorType string) {
	req, _ := http.NewRequest(http.MethodPost, url, bytes.NewReader([]byte(body)))
	if errorType != "" {
		req.Header.Set("Lambda-Runtime-Function-Error-Type", errorType)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		os.Exit(12)
	}
	resp.Body.Close()
}
