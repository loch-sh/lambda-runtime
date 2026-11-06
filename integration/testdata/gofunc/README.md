# Go test function (provided.al2023)

A minimal Go function built with `aws-lambda-go`, as Go functions are
deployed on Lambda: the code directory holds one executable named
`bootstrap`.

```sh
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -tags lambda.norpc -o /path/to/code/bootstrap .
chmod 0755 /path/to/code/bootstrap   # readable and executable by uid 10001
```

`GOARCH` must match the image (`arm64` or `amd64`). Run it on
`public.ecr.aws/lambda/provided:al2023` (multi-arch) with the code at
`/var/task`; the handler argument is required by `/lambda-entrypoint.sh` but
ignored by the function (`bootstrap` by convention).

- `{"name":"x"}` answers `{"hello":"x","requestId":"…","runtime":"go"}`;
- `{"op":"throw"}` is a function error (`Unhandled`, `errorType`
  `errorString`).
