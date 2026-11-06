// Copyright 2026 The Loch.sh Authors
// SPDX-License-Identifier: Apache-2.0

package app

import (
	"bytes"
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/loch-sh/lambda-runtime/internal/config"
)

// The test binary doubles as a minimal Lambda runtime that echoes events.
func TestMain(m *testing.M) {
	if os.Getenv("FAKE_RUNTIME") == "1" {
		echoRuntime()
		return
	}
	os.Exit(m.Run())
}

func echoRuntime() {
	api := "http://" + os.Getenv("AWS_LAMBDA_RUNTIME_API") + "/2018-06-01/runtime/invocation/"
	for {
		resp, err := http.Get(api + "next")
		if err != nil {
			os.Exit(1)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		id := resp.Header.Get("Lambda-Runtime-Aws-Request-Id")
		r, err := http.Post(api+id+"/response", "application/json", bytes.NewReader(body))
		if err != nil {
			os.Exit(1)
		}
		r.Body.Close()
	}
}

// syncBuffer is safe for the concurrent writes of the relay and logger.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func freePort(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return strconv.Itoa(ln.Addr().(*net.TCPAddr).Port)
}

func testConfig(t *testing.T) *config.Config {
	t.Setenv("FAKE_RUNTIME", "1")
	t.Setenv("LOCH_RUNTIME_AUTH", "off")
	t.Setenv("LOCH_RUNTIME_INVOKE_ADDR", "127.0.0.1:"+freePort(t))
	t.Setenv("LOCH_RUNTIME_API_ADDR", "127.0.0.1:"+freePort(t))
	t.Setenv("LOCH_RUNTIME_HEALTH_ADDR", "127.0.0.1:"+freePort(t))
	src := config.DefaultSources()
	src.CgroupMemoryMax = ""
	cfg, err := config.Parse([]string{"--", os.Args[0]}, src)
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func TestRunServesAndStops(t *testing.T) {
	cfg := testConfig(t)
	var stdout, stderr syncBuffer
	ctx, cancel := context.WithCancel(context.Background())
	code := make(chan int, 1)
	go func() {
		code <- Run(cfg, Env{Stdout: &stdout, Stderr: &stderr, CgroupRoot: t.TempDir(), Signals: ctx})
	}()

	base := "http://" + cfg.Runtime.InvokeAddr
	deadline := time.Now().Add(10 * time.Second)
	for Health(cfg, io.Discard) != 0 {
		if time.Now().After(deadline) {
			t.Fatalf("never healthy; stderr:\n%s", stderr.String())
		}
		time.Sleep(20 * time.Millisecond)
	}
	// Health is also served on the Invoke API listener.
	if resp, err := http.Get(base + "/_loch/health"); err != nil || resp.StatusCode != 200 {
		t.Fatalf("health on the invoke listener: %v", err)
	} else {
		resp.Body.Close()
	}
	resp, err := http.Post(base+"/2015-03-31/functions/function/invocations", "application/json", strings.NewReader(`{"x":1}`))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || string(body) != `{"x":1}` {
		t.Fatalf("invoke = %d %s", resp.StatusCode, body)
	}

	cancel()
	select {
	case c := <-code:
		if c != 0 {
			t.Errorf("exit code = %d", c)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return")
	}
	for _, want := range []string{"authentication is off", `"msg":"starting"`, "bootstrap started", "stopped"} {
		if !strings.Contains(stderr.String(), want) {
			t.Errorf("stderr lacks %q:\n%s", want, stderr.String())
		}
	}
	if !strings.Contains(stdout.String(), "REPORT RequestId:") {
		t.Errorf("stdout = %s", stdout.String())
	}
	if _, ok := os.LookupEnv("LOCH_RUNTIME_TOKEN"); ok {
		t.Error("secret variables must be removed from the environment")
	}
}

func TestRunRefusesBadCredentials(t *testing.T) {
	cfg := testConfig(t)
	cfg.Runtime.Auth = config.Auth{Enabled: true, TokenFile: "/nonexistent/token"}
	var stderr syncBuffer
	if code := Run(cfg, Env{Stdout: io.Discard, Stderr: &stderr}); code != 1 {
		t.Errorf("code = %d", code)
	}
	if !strings.Contains(stderr.String(), "startup refused") {
		t.Errorf("stderr = %s", stderr.String())
	}
}

func TestRunWithAuthenticationAndMetrics(t *testing.T) {
	const token = "0123456789abcdef0123456789abcdef"
	cfg := testConfig(t)
	cfg.Runtime.Auth = config.Auth{Enabled: true, Token: token}
	ctx, cancel := context.WithCancel(context.Background())
	code := make(chan int, 1)
	go func() {
		code <- Run(cfg, Env{Stdout: io.Discard, Stderr: io.Discard, CgroupRoot: t.TempDir(), Signals: ctx, Version: "v9"})
	}()
	deadline := time.Now().Add(10 * time.Second)
	for Health(cfg, io.Discard) != 0 {
		if time.Now().After(deadline) {
			t.Fatal("never healthy")
		}
		time.Sleep(20 * time.Millisecond)
	}
	base := "http://" + cfg.Runtime.InvokeAddr
	do := func(method, path, auth string) int {
		req, _ := http.NewRequest(method, base+path, strings.NewReader(`{}`))
		if auth != "" {
			req.Header.Set("Authorization", auth)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	// The test calls from the loopback, exempt from authentication (the
	// remote case is covered by invokeapi and the integration tests).
	invoke := "/2015-03-31/functions/function/invocations"
	if got := do("POST", invoke, ""); got != 200 {
		t.Errorf("loopback without credential: %d", got)
	}
	if got := do("POST", invoke, "Bearer "+token); got != 200 {
		t.Errorf("bearer: %d", got)
	}
	req, _ := http.NewRequest("GET", base+"/_loch/metrics", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	for _, want := range []string{`loch_lambda_runtime_info{version="v9"} 1`, `loch_lambda_invocations_total{outcome="success"} 2`,
		"loch_lambda_auth_failures_total 0", `loch_lambda_state{state="ready"} 1`} {
		if !strings.Contains(string(body), want) {
			t.Errorf("metrics lack %q:\n%s", want, body)
		}
	}
	cancel()
	if c := <-code; c != 0 {
		t.Errorf("exit code = %d", c)
	}
}

func TestRunFailsOnBusyPort(t *testing.T) {
	cfg := testConfig(t)
	for _, addr := range []*string{&cfg.Runtime.InvokeAddr, &cfg.Runtime.RuntimeAPIAddr, &cfg.Runtime.HealthAddr} {
		ln, err := net.Listen("tcp4", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		saved := *addr
		*addr = ln.Addr().String()
		if code := Run(cfg, Env{Stdout: io.Discard, Stderr: io.Discard}); code != 1 {
			t.Errorf("busy %s: code = %d", *addr, code)
		}
		ln.Close()
		*addr = saved
	}
}

func TestHealth(t *testing.T) {
	status := http.StatusOK
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(status)
		_, _ = io.WriteString(w, `{"status":"x"}`)
	}))
	defer srv.Close()
	cfg := &config.Config{}
	cfg.Runtime.HealthAddr = strings.TrimPrefix(srv.URL, "http://")
	var out bytes.Buffer
	if code := Health(cfg, &out); code != 0 || !strings.Contains(out.String(), `"status":"x"`) {
		t.Errorf("ready: %d %s", code, out.String())
	}
	status = http.StatusServiceUnavailable
	if code := Health(cfg, io.Discard); code != 1 {
		t.Errorf("not ready: %d", code)
	}
	cfg.Runtime.HealthAddr = "127.0.0.1:" + freePort(t)
	if code := Health(cfg, io.Discard); code != 1 {
		t.Errorf("unreachable: %d", code)
	}
}
