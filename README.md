# loch lambda-runtime

Run AWS Lambda functions in plain Docker containers, with the official AWS
base images and unchanged handlers. No vendor lock-in: the runtime speaks
AWS's public protocols and knows nothing about the platform around it.

`loch-lambda-runtime` replaces the Runtime Interface Emulator (RIE) bundled in
the AWS base images. It is the container's PID 1:

- it serves the **Lambda Runtime API** (`2018-06-01`) to the image's bootstrap
  on `127.0.0.1:9001`;
- it serves the **Lambda Invoke API** (`POST /2015-03-31/functions/{name}/invocations`)
  on port 8080, so `aws lambda invoke --endpoint-url`, SAM and the AWS SDKs work;
- it runs exactly one invocation at a time, queues the others (429 when the
  queue is full), enforces the timeout, reports errors the AWS way
  (`X-Amz-Function-Error`), never truncates a payload, and never crashes on
  concurrent requests.

> Status: under development (v1). See [docs/spec.md](docs/spec.md).

## Usage

The binary is static (no libc, no curl): mount it read-only into any Lambda
image and make it the entrypoint.

```sh
docker run --rm -p 127.0.0.1:8080:8080 \
  -v "$PWD/loch-lambda-runtime-linux-arm64:/loch/lambda-runtime:ro" \
  -v "$PWD/code:/var/task:ro" \
  -e LOCH_RUNTIME_AUTH=off \
  -e AWS_LAMBDA_FUNCTION_TIMEOUT=30 \
  --entrypoint /loch/lambda-runtime \
  public.ecr.aws/lambda/nodejs:22 index.handler

curl -X POST localhost:8080/2015-03-31/functions/function/invocations -d '{"hello":"world"}'
```

Command line:

```
loch-lambda-runtime [run] [flags] -- <command> [args...]   # the image ENTRYPOINT and CMD
loch-lambda-runtime [run] [flags] <handler>                # = /lambda-entrypoint.sh <handler>
loch-lambda-runtime health                                 # exit 0 when ready (compose healthcheck)
loch-lambda-runtime invoke [--timeout <s>] [--log-tail]    # event on stdin, JSON envelope on stdout
loch-lambda-runtime version
```

Use `run` explicitly if your handler is literally named `health`, `invoke` or
`version`.

## Configuration

Every `LOCH_RUNTIME_*` variable also exists as a flag (`--queue-size`, …);
flags win. Invalid values stop the runtime at startup with a message naming the
setting.

| Variable | Default | Meaning |
|---|---|---|
| `LOCH_RUNTIME_INVOKE_ADDR` | `0.0.0.0:8080` | Invoke API listen address |
| `LOCH_RUNTIME_API_ADDR` | `127.0.0.1:9001` | Runtime API listen address (loopback only) |
| `LOCH_RUNTIME_HEALTH_ADDR` | `127.0.0.1:8081` | health-only listener used by `health` (loopback only) |
| `LOCH_RUNTIME_BOOTSTRAP` | `/lambda-entrypoint.sh` | program of the `<handler>` shortcut |
| `LOCH_RUNTIME_WORKDIR` | current directory (the image `WORKDIR`) | working directory of the bootstrap |
| `LOCH_RUNTIME_INIT_TIMEOUT` | `10s` | init budget before it is charged to the first invocation |
| `LOCH_RUNTIME_SHUTDOWN_GRACE` | `8s` | time given to the running invocation on SIGTERM |
| `LOCH_RUNTIME_MAX_PAYLOAD` | `6MiB` | request limit (responses: + 100 bytes, as Lambda) |
| `LOCH_RUNTIME_QUEUE_SIZE` | `16` | waiting invocations |
| `LOCH_RUNTIME_QUEUE_BYTES` | `min(32MiB, memory/8)` | total size of waiting invocations |
| `LOCH_RUNTIME_QUEUE_TIMEOUT` | `30s` | maximum wait in the queue |
| `LOCH_RUNTIME_MAX_CONNS` | `64` | open connections to the Invoke API |
| `LOCH_RUNTIME_AUTH` | `on` | `off` disables authentication (development only) |
| `LOCH_RUNTIME_TOKEN` | | bearer token (read at startup, then removed from the environment) |
| `LOCH_RUNTIME_TOKEN_FILE` | | file holding the bearer token |
| `LOCH_RUNTIME_ACCESS_KEY` | | `<access key id>:<secret>` for SigV4 (removed from the environment once read) |
| `LOCH_RUNTIME_ACCESS_KEY_FILE` | | file holding `<access key id>:<secret>` for SigV4 |
| `LOCH_RUNTIME_LOG_LEVEL` | `info` | `debug`, `info`, `warn`, `error` |

Durations accept Go syntax (`1m30s`) or seconds (`2.5`); sizes accept bytes or
a `KiB`/`MiB`/`GiB` suffix.

Function settings use the AWS variables: `AWS_LAMBDA_FUNCTION_NAME` (default
`function`), `AWS_LAMBDA_FUNCTION_VERSION` (`$LATEST`), `AWS_LAMBDA_FUNCTION_TIMEOUT`
(seconds, default 300; an RIE convention, not an AWS variable),
`AWS_LAMBDA_FUNCTION_MEMORY_SIZE` (default: the cgroup memory limit, else 3008),
`AWS_REGION` (default `us-east-1`).

The bootstrap receives the inherited environment, plus the variables a Lambda
runtime expects when they are absent (`AWS_LAMBDA_LOG_GROUP_NAME`,
`AWS_LAMBDA_LOG_STREAM_NAME`, `AWS_DEFAULT_REGION`, `LAMBDA_TASK_ROOT`, …).
`AWS_LAMBDA_RUNTIME_API` is always forced to the Runtime API, and
`AWS_LAMBDA_MAX_CONCURRENCY` and every `LOCH_RUNTIME_*` variable are always
removed. No credential is ever invented.

## Not supported (v1)

Extensions and Telemetry APIs (layer extensions in `/opt/extensions` are not
started), response streaming to the client (streamed responses are buffered),
SnapStart, X-Ray export, asynchronous `Event` invocations.

## Development

```sh
make test         # unit tests (race detector)
make test-linux   # unit tests in a Linux container
make cover        # coverage
make release      # static linux/amd64 and linux/arm64 binaries + SHA256SUMS
```

## License

Apache License 2.0, see [LICENSE](LICENSE) and [NOTICE](NOTICE).
