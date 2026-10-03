# Results

All four tests fail at both versions tested. The client returns from
`CallTool` before its handlers have run for progress notifications that the
server sent before the response.

## Environment

- Go: `go1.26.7 linux/amd64` (from `shell.nix`, `GOTOOLCHAIN=local`)
- Machine: 13th Gen Intel Core i9-13900H, 20 logical CPUs, Linux 6.18.53 (NixOS)
- Command: `go test -count=1 -v ./...`, 1000 calls per test, three runs per
  version. No race detector: the tests also fail under `-race`, and report no
  data race, but it is not needed.

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

Each cell lists runs 1 / 2 / 3, each of 1000 calls, 3 progress notifications
per call.

| Test | What is counted | `v1.8.0` | `main` |
|---|---|---|---|
| `TestProgressBeforeResult_InMemory` | calls whose `CallTool` returned before all 3 progress handlers ran | 77 / 69 / 80 | 43 / 52 / 58 |
| `TestProgressBeforeResult_HTTP` | the same, over Streamable HTTP | 342 / 373 / 366 | 374 / 399 / 379 |
| `TestProxyLosesProgress` | upstream progress the proxy dropped, of 3000 | 488 / 487 / 472 | 465 / 460 / 472 |
| `TestProxySendsProgressAfterResult` | progress the proxy's caller read after the call's response, of 3000 | 579 / 589 / 591 | 575 / 608 / 594 |

## Where the reordering happens

`TestProgressBeforeResult_InMemory` reads the client's transport as well as
its handlers (`wire_test.go`). In every run, the transport delivered all
three progress notifications before the response, so the reordering is after
`Read`, in `jsonrpc2.Connection`. The test stops with "does not apply" if
that ever stops being true. The HTTP client's connection cannot be wrapped
the same way (the SDK type-asserts it for an unexported interface), so the
HTTP test counts at the handlers only. HTTP's wire order was not measured,
so its higher rate is unexplained: it is not attributed to `jsonrpc2.Connection`
or to the streamable transport.

## What a proxy can do about it

The proxy forwards its upstream's progress to its own caller. It has to stop
forwarding a call's progress at some point, and the only moment the SDK gives
it is the upstream `CallTool` returning, when progress sent before the
upstream's response may not have been handled yet. The two proxy tests make
one choice each:

- **Stop when the call returns** (`TestProxyLosesProgress`): about one in six
  progress notifications is lost.
- **Keep forwarding** (`TestProxySendsProgressAfterResult`): nothing is lost,
  but about one in five reaches the caller after the proxy has answered. That
  is read at the caller's transport, so it is what the proxy sent and not the
  caller's own reordering, and it breaks 2025-11-25 `basic/utilities/progress`:
  progress MUST only reference tokens of active requests, and MUST stop after
  completion.

Neither choice is correct, and the SDK offers no third: nothing tells a caller
when the notifications read ahead of a response have been handled.
