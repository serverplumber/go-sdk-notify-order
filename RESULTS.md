# Results

All three tests fail at both versions tested: the client returns from
`CallTool` before its handlers have run for progress notifications that the
server sent before the response.

## Environment

- Go: `go1.26.7 linux/amd64` (from `shell.nix`, `GOTOOLCHAIN=local`)
- Machine: 13th Gen Intel Core i9-13900H, 20 logical CPUs, Linux 6.18.53 (NixOS)
- Command: `go test -race -count=1 -v -run 'ProgressBeforeResult|ProxyForwardsProgress' ./...`
  (what `just repro` runs), 1000 calls per test, three runs per version.

## Versions

| Label    | Module version                         | Commit                                     |
|----------|----------------------------------------|--------------------------------------------|
| `v1.8.0` | `v1.8.0`                               | `3f3b699b2b67e1ed033a63d6651671dab53c2d32` |
| `main`   | `v1.8.1-0.20261002152441-25a53e3f27df` | `25a53e3f27df62cef7066d81a8704b3df695800a` |

`main` is what `go get github.com/modelcontextprotocol/go-sdk@main` resolved to
on 2026-10-03.

The code named in the report is the same at both versions. In
`internal/jsonrpc2/conn.go`, `readIncoming` retires a `*Response` inline
(`ac.retire(msg)`), and passes a `*Request` to `acceptRequest`, which appends
it to `handlerQueue` for `handleAsync` to drain on another goroutine.
Between the two versions, `conn.go` differs only in the addition of
`Connection.CancelCause`. `readIncoming`, `acceptRequest` and `handleAsync` are
unchanged.

## Counts

"Violations" means calls whose `CallTool` returned with fewer than 3 of that
call's progress notifications handled. Each cell lists runs 1 / 2 / 3, each out
of 1000 calls.

| Test                                  | `v1.8.0` violations | `main` violations |
|---------------------------------------|---------------------|-------------------|
| `TestProgressBeforeResult_InMemory`   | 27 / 19 / 20        | 13 / 17 / 19      |
| `TestProgressBeforeResult_HTTP`       | 399 / 375 / 416     | 448 / 419 / 359   |
| `TestProxyForwardsProgressAfterResult`| 284 / 313 / 285     | 292 / 298 / 330   |

Notifications handled after their call returned (the same runs):

| Test                                  | `v1.8.0`        | `main`          |
|---------------------------------------|-----------------|-----------------|
| `TestProgressBeforeResult_InMemory`   | 29 / 20 / 21    | 15 / 18 / 19    |
| `TestProgressBeforeResult_HTTP`       | 530 / 495 / 528 | 580 / 545 / 449 |

For the proxy test, out of 3000 upstream notifications per run:

| Proxy test                                                         | `v1.8.0`        | `main`          |
|--------------------------------------------------------------------|-----------------|-----------------|
| Dropped: reached the proxy's client after its tool handler returned | 294 / 317 / 286 | 300 / 307 / 327 |
| Late: reached the downstream client after its `CallTool` returned   | 15 / 16 / 12    | 6 / 14 / 17     |

## Reading the proxy result

The proxy is written the obvious way. Its tool handler registers a forwarder
for its progress token, then calls upstream, then deregisters the forwarder
when it returns. Upstream progress that the proxy's client handles only after
`CallTool` returned to the proxy therefore has nowhere to go:

- It is **dropped** if the forwarder is gone. This causes most of the
  downstream violations.
- It is **forwarded after the proxy has answered**, if the forwarder was loaded
  just before the handler returned. This is the spec violation (2025-11-25,
  `basic/utilities/progress`).

The downstream "late" count cannot separate those two causes from the
downstream client's own reordering. That reordering is the same bug as in the
in-memory test, so the late count measures their sum.
