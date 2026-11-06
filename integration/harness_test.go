// Copyright 2026 The Loch.sh Authors
// SPDX-License-Identifier: Apache-2.0

//go:build integration

// Package integration runs the runtime against the real AWS Lambda base
// images, under the hardening of production. It needs Docker:
//
//	go test -tags integration -v ./integration/
package integration

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// image is a Lambda base image and the handler of its test function.
type image struct {
	name    string
	ref     string
	code    string
	handler string
}

var images = []image{
	{"node22", "mirror.gcr.io/amazon/aws-lambda-nodejs:22-" + awsArch(), "node", "index.handler"},
	{"python313", "mirror.gcr.io/amazon/aws-lambda-python:3.13-" + awsArch(), "python", "app.handler"},
}

// awsArch is the architecture suffix of the AWS image tags.
func awsArch() string {
	if runtime.GOARCH == "arm64" {
		return "arm64"
	}
	return "x86_64"
}

var (
	buildOnce sync.Once
	binary    string
	buildErr  error
)

// runtimeBinary builds the static Linux binary once.
func runtimeBinary(t *testing.T) string {
	t.Helper()
	buildOnce.Do(func() {
		dir, err := os.MkdirTemp("", "lr-integration")
		if err != nil {
			buildErr = err
			return
		}
		binary = filepath.Join(dir, "loch-lambda-runtime")
		cmd := exec.Command("go", "build", "-trimpath", "-o", binary, "../cmd/loch-lambda-runtime")
		cmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux", "GOARCH="+runtime.GOARCH)
		out, err := cmd.CombinedOutput()
		if err != nil {
			buildErr = fmt.Errorf("build: %v\n%s", err, out)
		}
	})
	if buildErr != nil {
		t.Fatal(buildErr)
	}
	return binary
}

// container is a running function.
type container struct {
	t    *testing.T
	name string
	url  string
}

type runOptions struct {
	memory string
	env    []string
	// cmd replaces the image handler shortcut.
	cmd []string
	// code replaces the image's test function directory (absolute path).
	code string
	// auth keeps authentication on (the credentials come with env).
	auth bool
	// network and alias attach the container to a test network.
	network, alias string
	// mounts are extra -v volumes.
	mounts []string
}

// start runs img under the production hardening, the Invoke API published
// on a random loopback port for the test.
func start(t *testing.T, img image, o runOptions) *container {
	t.Helper()
	if o.memory == "" {
		o.memory = "256m"
	}
	code := o.code
	if code == "" {
		var err error
		if code, err = filepath.Abs(filepath.Join("testdata", img.code)); err != nil {
			t.Fatal(err)
		}
	}
	name := fmt.Sprintf("lr-it-%s-%d", img.name, time.Now().UnixNano())
	args := []string{"run", "-d", "--name", name,
		"--read-only", "--user", "10001:10001", "--cap-drop", "ALL", "--security-opt", "no-new-privileges:true",
		"--tmpfs", "/tmp:size=64m", "--memory", o.memory, "--memory-swap", o.memory, "--cpus", "0.5", "--pids-limit", "128",
		"-p", "127.0.0.1::8080",
		"-v", runtimeBinary(t) + ":/loch/lambda-runtime:ro",
		"-v", code + ":/var/task:ro",
		"-e", "AWS_LAMBDA_FUNCTION_TIMEOUT=3",
		"--entrypoint", "/loch/lambda-runtime",
	}
	if !o.auth {
		args = append(args, "-e", "LOCH_RUNTIME_AUTH=off")
	}
	for _, m := range o.mounts {
		args = append(args, "-v", m)
	}
	if o.network != "" {
		args = append(args, "--network", o.network, "--network-alias", o.alias)
	}
	for _, e := range o.env {
		args = append(args, "-e", e)
	}
	args = append(args, img.ref)
	if o.cmd != nil {
		args = append(args, o.cmd...)
	} else {
		args = append(args, img.handler)
	}
	docker(t, args...)
	c := &container{t: t, name: name}
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("logs of %s:\n%s", name, c.logs())
		}
		_, _ = exec.Command("docker", "rm", "-f", name).CombinedOutput()
	})
	port := strings.TrimSpace(docker(t, "port", name, "8080/tcp"))
	_, p, _ := net.SplitHostPort(port[strings.LastIndex(port, "\n")+1:])
	c.url = "http://127.0.0.1:" + p
	return c
}

func docker(t *testing.T, args ...string) string {
	t.Helper()
	out, err := exec.Command("docker", args...).CombinedOutput()
	if err != nil {
		t.Fatalf("docker %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return string(out)
}

func (c *container) logs() string {
	out, _ := exec.Command("docker", "logs", c.name).CombinedOutput()
	return string(out)
}

// waitHealth waits for /_loch/health to answer want.
func (c *container) waitHealth(want string) {
	c.t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	last := ""
	for time.Now().Before(deadline) {
		resp, err := http.Get(c.url + "/_loch/health")
		if err == nil {
			b, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			last = string(b)
			if strings.Contains(last, `"status":"`+want+`"`) {
				return
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	c.t.Fatalf("health never reached %s (last %q)", want, last)
}

// response is an Invoke API answer.
type response struct {
	status        int
	body          []byte
	functionError string
	logTail       string
	header        http.Header
}

func (c *container) invoke(event string) response {
	c.t.Helper()
	r, err := c.tryInvoke(event)
	if err != nil {
		c.t.Fatalf("invoke %s: %v", event, err)
	}
	return r
}

// tryInvoke invokes and returns transport errors instead of failing.
func (c *container) tryInvoke(event string) (response, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost,
		c.url+"/2015-03-31/functions/function/invocations", bytes.NewReader([]byte(event)))
	req.Header.Set("X-Amz-Log-Type", "Tail")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return response{}, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	tail, _ := base64.StdEncoding.DecodeString(resp.Header.Get("X-Amz-Log-Result"))
	return response{resp.StatusCode, body, resp.Header.Get("X-Amz-Function-Error"), string(tail), resp.Header}, nil
}

// errorType returns the errorType of a function error body.
func (r response) errorType() string {
	var doc struct{ ErrorType string }
	_ = json.Unmarshal(r.body, &doc)
	return doc.ErrorType
}

func (r response) String() string {
	return fmt.Sprintf("%d fe=%q %s", r.status, r.functionError, r.body)
}

func (c *container) dockerExec(args ...string) (string, error) {
	out, err := exec.Command("docker", append([]string{"exec", c.name}, args...)...).CombinedOutput()
	return string(out), err
}

// network creates a Docker network for the test.
func network(t *testing.T) string {
	t.Helper()
	name := fmt.Sprintf("lr-it-net-%d", time.Now().UnixNano())
	docker(t, "network", "create", name)
	t.Cleanup(func() { _, _ = exec.Command("docker", "network", "rm", name).CombinedOutput() })
	return name
}

// invokeWith sends an invocation with extra headers and an arbitrary
// method and body.
func (c *container) invokeWith(method, path string, body []byte, header http.Header) response {
	c.t.Helper()
	r, err := c.tryInvokeWith(method, path, body, header)
	if err != nil {
		c.t.Fatalf("%s %s: %v", method, path, err)
	}
	return r
}

// tryInvokeWith is invokeWith returning transport errors, for goroutines.
func (c *container) tryInvokeWith(method, path string, body []byte, header http.Header) (response, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, method, c.url+path, bytes.NewReader(body))
	for k, v := range header {
		req.Header[k] = v
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return response{}, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return response{resp.StatusCode, b, resp.Header.Get("X-Amz-Function-Error"), "", resp.Header}, nil
}
