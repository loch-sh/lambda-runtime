// Copyright 2026 The Loch.sh Authors
// SPDX-License-Identifier: Apache-2.0

// Package app wires the components into the subcommands of the binary.
package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/loch-sh/lambda-runtime/internal/cgroup"
	"github.com/loch-sh/lambda-runtime/internal/config"
	"github.com/loch-sh/lambda-runtime/internal/engine"
	"github.com/loch-sh/lambda-runtime/internal/invokeapi"
	"github.com/loch-sh/lambda-runtime/internal/logs"
	"github.com/loch-sh/lambda-runtime/internal/metrics"
	"github.com/loch-sh/lambda-runtime/internal/netutil"
	"github.com/loch-sh/lambda-runtime/internal/process"
	"github.com/loch-sh/lambda-runtime/internal/queue"
	"github.com/loch-sh/lambda-runtime/internal/runtimeapi"
)

const (
	// runtimeAPIConns caps the connections to the Runtime API: a runtime
	// client uses one or two.
	runtimeAPIConns = 16
	// healthConns caps the connections to the health listener.
	healthConns = 4
	// minBodyRate sets the deadline of a request upload (bytes/s): 6 MiB
	// must arrive within 5 s + 6 s on the internal Docker network.
	minBodyRate = 1 << 20
	// httpShutdown bounds the flush of the last responses at exit.
	httpShutdown = 5 * time.Second
)

// Env is what Run needs from the process, replaceable in tests.
type Env struct {
	// Version is reported by the metrics.
	Version        string
	Stdout, Stderr io.Writer
	CgroupRoot     string
	// Signals delivers the shutdown signals; nil means SIGTERM and SIGINT.
	Signals context.Context
	// HardenErr is the result of process.Harden, run first by main.
	HardenErr error
}

// Run runs the function until SIGTERM and returns the exit code.
func Run(cfg *config.Config, env Env) int {
	relay := logs.NewRelay(env.Stdout, env.Stderr)
	log := newLogger(relay, cfg.Runtime.LogLevel)
	store, authenticator, err := secure(cfg, env.HardenErr, log)
	if err != nil {
		log.Error("startup refused", "error", err)
		return 1
	}
	logStartup(cfg, log)
	warnLowFileLimit(cfg.Runtime.MaxConns, openFilesLimit, log)

	ctx, stop := shutdownContext(env)
	defer stop()
	if store != nil {
		stopReload := reloadOnSIGHUP(store, log)
		defer stopReload()
	}
	parts, err := start(cfg, env, relay, authenticator, log)
	if err != nil {
		log.Error("not started", "error", err)
		return 1
	}
	defer parts.close()
	parts.serveUntil(ctx, log)
	log.Info("stopped")
	return 0
}

// shutdownContext ends on SIGTERM or SIGINT, or with env.Signals in tests.
func shutdownContext(env Env) (context.Context, context.CancelFunc) {
	if env.Signals != nil {
		return env.Signals, func() {}
	}
	return signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
}

// parts are the running components.
type parts struct {
	reaper                           *process.Reaper
	queue                            *queue.Queue
	engine                           *engine.Engine
	runtimeSrv, invokeSrv, healthSrv *http.Server
}

// start builds the components and starts the servers; the bootstrap
// starts with serveUntil.
func start(cfg *config.Config, env Env, relay *logs.Relay, authenticator invokeapi.Authenticator, log *slog.Logger) (*parts, error) {
	p := &parts{reaper: process.NewReaper(log)}
	events := make(chan runtimeapi.Event, 16)
	machine := runtimeapi.NewMachine(events)
	var err error
	if p.runtimeSrv, err = serveRuntimeAPI(cfg, machine, log); err != nil {
		p.close()
		return nil, fmt.Errorf("runtime api: %w", err)
	}
	p.queue = queue.New(cfg.Runtime.QueueSize, cfg.Runtime.QueueBytes)
	p.queue.SetUnverifiedLimits(unverifiedLimits(cfg.Runtime))
	opts := engineOptions(cfg, env, log)
	if !opts.Memory.Available() {
		log.Warn("cgroup memory files not readable: no Max Memory Used, no OOM detection")
	}
	collector := metrics.New(env.Version, func() (int, int) {
		s := p.queue.Stats()
		return s.Depth, s.Peak
	}, opts.Memory.PeakBytes)
	opts.Observer = collector
	p.engine = engine.New(opts, engine.Dependencies{
		Machine: machine, Events: events, Reaper: p.reaper, Relay: relay, Source: p.queue,
	}, os.Getpid())
	if p.invokeSrv, p.healthSrv, err = serveInvokeAPI(cfg, p.queue, p.engine, authenticator, collector, log); err != nil {
		p.close()
		return nil, fmt.Errorf("invoke api: %w", err)
	}
	return p, nil
}

// serveUntil runs the bootstrap until ctx ends, then shuts down: queued
// requests get 503, the engine stops the bootstrap, the last responses
// are flushed.
func (p *parts) serveUntil(ctx context.Context, log *slog.Logger) {
	done := make(chan struct{})
	go func() {
		defer close(done)
		p.engine.Run(ctx)
	}()
	<-ctx.Done()
	log.Info("shutdown requested")
	for _, job := range p.queue.Close() {
		job.Fail(engine.ErrShuttingDown)
	}
	<-done
	shutdownCtx, cancel := context.WithTimeout(context.Background(), httpShutdown)
	defer cancel()
	_ = p.invokeSrv.Shutdown(shutdownCtx)
}

func (p *parts) close() {
	for _, srv := range []*http.Server{p.healthSrv, p.invokeSrv, p.runtimeSrv} {
		if srv != nil {
			_ = srv.Close()
		}
	}
	p.reaper.Close()
}

func newLogger(relay *logs.Relay, level string) *slog.Logger {
	var l slog.Level
	_ = l.UnmarshalText([]byte(level))
	return slog.New(slog.NewJSONHandler(relay.Writer(true), &slog.HandlerOptions{Level: l}))
}

func logStartup(cfg *config.Config, log *slog.Logger) {
	for _, w := range cfg.Warnings {
		log.Warn(w)
	}
	rt, fn := cfg.Runtime, cfg.Function
	log.Info("starting",
		"function", fn.Name, "version", fn.Version, "memory_mb", fn.MemoryMB, "memory_source", string(fn.MemorySource),
		"timeout", fn.Timeout.String(), "invoke_addr", rt.InvokeAddr, "runtime_api_addr", rt.RuntimeAPIAddr,
		"queue_size", rt.QueueSize, "queue_bytes", rt.QueueBytes, "queue_timeout", rt.QueueTimeout.String(),
		"max_payload", rt.MaxPayload, "max_conns", rt.MaxConns, "health_addr", rt.HealthAddr, "command", cfg.ChildArgs)
}

func engineOptions(cfg *config.Config, env Env, log *slog.Logger) engine.Options {
	fn := cfg.Function
	root := env.CgroupRoot
	if root == "" {
		root = cgroup.Resolve(cgroup.DefaultRoot, cgroup.SelfCgroupFile)
	}
	return engine.Options{
		Command:       cfg.ChildArgs,
		Env:           cfg.ChildEnv,
		Dir:           cfg.Runtime.Workdir,
		FunctionARN:   fn.ARN(),
		Timeout:       fn.Timeout,
		InitTimeout:   cfg.Runtime.InitTimeout,
		ShutdownGrace: cfg.Runtime.ShutdownGrace,
		MaxResponse:   cfg.Runtime.MaxResponse(),
		Platform:      logs.Platform{JSON: fn.LogJSON, Version: fn.Version, MemoryMB: fn.MemoryMB},
		Memory:        cgroup.NewMemory(root),
		Log:           log,
	}
}

func serveRuntimeAPI(cfg *config.Config, m *runtimeapi.Machine, log *slog.Logger) (*http.Server, error) {
	ln, err := net.Listen("tcp4", cfg.Runtime.RuntimeAPIAddr)
	if err != nil {
		return nil, err
	}
	srv := runtimeapi.NewServer(runtimeapi.NewHandler(m, cfg.Runtime.MaxResponse(), log), log)
	go serve(srv, netutil.LimitListener(ln, runtimeAPIConns), log)
	return srv, nil
}

// serveInvokeAPI serves the Invoke API (with health and metrics) on
// InvokeAddr, and health alone on the loopback HealthAddr used by the
// health subcommand. authenticator is nil when authentication is off.
func serveInvokeAPI(cfg *config.Config, q *queue.Queue, eng *engine.Engine, authenticator invokeapi.Authenticator,
	collector *metrics.Collector, log *slog.Logger) (invoke, health *http.Server, err error) {
	ln, err := net.Listen("tcp", cfg.Runtime.InvokeAddr)
	if err != nil {
		return nil, nil, err
	}
	healthLn, err := net.Listen("tcp4", cfg.Runtime.HealthAddr)
	if err != nil {
		ln.Close()
		return nil, nil, err
	}
	h := invokeapi.NewHandler(invokeapi.Options{
		Function:     invokeapi.FunctionName{Name: cfg.Function.Name, Region: cfg.Function.Region},
		Version:      cfg.Function.Version,
		MaxPayload:   cfg.Runtime.MaxPayload,
		QueueTimeout: cfg.Runtime.QueueTimeout,
		MinBodyRate:  minBodyRate,
		// One caller may fill the queue (1 running + QueueSize waiting),
		// not more.
		MaxInFlightPerIP:    cfg.Runtime.QueueSize + 1,
		RetryAfter:          eng.RetryAfter,
		Auth:                authenticator,
		LoopbackWithoutAuth: true,
		AllowBrowser:        cfg.Runtime.AllowBrowser,
		Observer:            collector,
		Metrics:             collector,
		Queue:               q,
		State:               eng.State,
		Log:                 log,
	})
	invoke = invokeapi.NewServer(h, log)
	go serve(invoke, netutil.LimitListener(netutil.PerIPLimitListener(ln, connsPerIP(cfg.Runtime.MaxConns), invokeapi.ThrottledResponse, collector.Throttled), cfg.Runtime.MaxConns), log)
	healthMux := http.NewServeMux()
	healthMux.HandleFunc("/_loch/health", h.Health)
	health = invokeapi.NewServer(healthMux, log)
	go serve(health, netutil.LimitListener(healthLn, healthConns), log)
	return invoke, health, nil
}

func serve(srv *http.Server, ln net.Listener, log *slog.Logger) {
	if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Error("server stopped", "error", fmt.Sprint(err))
	}
}
