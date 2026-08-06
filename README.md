# mycel_module_sdk

`github.com/cogniworks/mycel_module_sdk` — the **single definition of the seam every
mycel gRPC module is built against**: the plain types a module's logic speaks, the
transport glue that serves it, and the host↔module wire contract. Modules *import* it
(never copy it), which is what stops the per-module definitions from drifting apart.

## Where this sits

Three repos form the module toolchain — grasp all three and you have the whole picture:

| repo | role | one line |
|---|---|---|
| **`mycel_module_sdk`** (this) | the **seam** | the types + transport + wire contract every module is built against |
| [`mycel_app_core_modules`](../mycel_app_core_modules) | the **modules** | concrete implementations, one directory each |
| [`mycel_release`](../mycel_release) | the **pipeline** | builds each module's image and pushes it to ghcr |

## The runtime model in one picture

A mycel module is a **full process** — it owns its own data, clock, and syscalls. The
`mycel_app_core` **host is a pure gRPC client**: it dials the module for `Describe` and
`/m/<token>/*`. There is no host→module capability grant; the module's datastore handle
is its *own*, not something the host injects.

```
mycel_app_core (host, gRPC client)  ──dials──▶  a module (full process)
                                                 └─ serves Describe + /m/<token>/*
```

## The two import surfaces

The core invariant is **logic purity**: the root package imports neither the gRPC
bindings nor any datastore driver, so a module's own logic stays transport-agnostic and
natively testable. Transport lives in a separate package.

```
a module's logic  →  import "…/mycel_module_sdk"          // Request, Response, Row, Host, RouteFunc, Descriptor
its main.go       →  import "…/mycel_module_sdk/server"   // server.Serve(modulesdk.Descriptor{…})
```

## Packages

| package | what it is |
|---|---|
| `.` (`modulesdk`, [sdk.go](sdk.go)) | **pure** types + the `Host` seam. `Request`/`Response`/`Row`/`RouteFunc`/`Descriptor`, and `Host` (the per-request datastore interface). |
| `./server` | transport glue — `server.Serve(Descriptor{…})` adapts gRPC `HandleRequest` to the pure types, implements `Host` over a `*sql.Tx`, adds peer-cred + telemetry. |
| `./analytics` | **stdlib-only** business-event + KPI client: `Emit` (non-blocking), the queue + flusher, and `KPI` reads. Takes its transport as a func value, so it imports neither gRPC nor `./server`. |
| `./contracts/modulerpc/v1` | the host↔module gRPC wire contract. |

### Business events (`./analytics`)

A module emits at a business state transition; the client queues in-process and a
background flusher delivers batches to the `mycel_kpi` module over `server.CallExt` (a
direct module→module dial — the host is not involved). `Emit` takes no context and
returns no error, because both would put analytics back in the OLTP path.

```go
an := analytics.New(analytics.Config{Service: "acme", Transport: server.CallExt})
…
an.Emit(analytics.Event{
    Type: "OrderSubmitted", SubjectType: "order", SubjectID: id,
    Attrs: map[string]string{"customer_tier": tier},
    Nums:  map[string]float64{"weight": w},
}.WithHeaders(req.Headers))   // actor_id + trace_id come from the request
```

**Wire `Shutdown`, or every deploy loses events.** `GracefulStop` drains in-flight RPCs
and knows nothing about the queue, so without the hook the process exits with events that
`Emit` already accepted, that no counter recorded, and that no error reported — invisible
in dev, permanent in production:

```go
server.Serve(modulesdk.Descriptor{…, Shutdown: an.Shutdown})
```

Unset `MYCEL_KPI_URL` disables the client entirely (no goroutine, no retries), so an app
running without a kpi module costs nothing.

### The transaction model (the one rule to remember)

`Host` is a Postgres transaction scoped to the module's schema (`search_path = <token>`).
Every `Query`/`Exec` made while serving **one request** runs inside a **single**
transaction — you do *not* begin/commit. The server commits when the handler returns
status `< 500` and rolls back on a `5xx` or any failed statement. Always pass values as
`params`, never concatenate into SQL.

## Consumption

Consumed via a `replace` to a sibling checkout — the **primary, indefinite** mechanism
(same convention as [`mycel_release`](../mycel_release)):

```
require github.com/cogniworks/mycel_module_sdk v0.0.0
replace github.com/cogniworks/mycel_module_sdk => ../../../../mycel_module_sdk
```

(The `../` depth depends on the consumer's location — e.g. a module's `module/service/`.)
Because a module's Docker build can't resolve that local `replace`, the release pipeline
runs `go mod vendor` around the build; see `mycel_app_core_modules/RELEASE.md`.

## Who builds against it

The six gRPC modules in [`mycel_app_core_modules`](../mycel_app_core_modules) — `mycel_iam`,
`mycel_cluster_console`, `mycel_file_system`, `mycel_kpi`, `mycel_project_management`,
`mycel_workflow_manager`. (The `mycel_client_workflow` builder base is a Temporal worker,
not a gRPC module, and does not use this SDK.)
