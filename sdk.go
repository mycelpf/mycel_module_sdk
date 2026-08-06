// Package modulesdk is the shared module SDK — the single definition of the types and
// the seam every mycel gRPC module is built against. It is the root package of the
// standalone module github.com/mycelpf/mycel_module_sdk, imported (not copied) by
// each module, alongside its sibling packages ./server (transport glue) and
// ./contracts/modulerpc/v1 (the host↔module wire contract). This module replaced the
// per-module vendored copies that used to drift apart.
//
// This package is PURE: it imports neither the gRPC/contract bindings nor any datastore
// driver, so a module's own logic can import it and stay transport-agnostic and
// natively testable (the logic-purity invariant). The transport glue lives in ./server.
//
//	a module's logic  →  imports mycel_module_sdk         (Request/Response/Host/Row/RouteFunc)
//	its main.go       →  imports mycel_module_sdk/server  (server.Serve(modulesdk.Descriptor{…}))
//
// A mycel module is a FULL process: it owns its own data, clock, and syscalls. The
// host (mycel_app_core) is a pure gRPC client that dials this module for Describe
// and /m/<token>/* — there is no host→module capability grant. The Host seam below
// is the module's OWN per-request datastore handle, not something the host injects.
package modulesdk

import (
	"context"
	"net/http"
)

// Request is one inbound request, in plain types (no contract bindings). The server
// adapter builds it from the gRPC HandleRequest; the path already has the
// /m/<token> prefix stripped by the host.
type Request struct {
	Method  string
	Path    string            // path under /m/<token> (no leading host prefix)
	Query   string            // raw query string, no leading '?'
	Headers map[string]string // lower-cased header names
	Body    []byte
}

// Response is the module's reply. If Headers has no "content-type", the server
// adapter defaults it to application/json.
type Response struct {
	Status  uint16
	Headers map[string]string
	Body    string
}

// Row is one stringly-typed result row (v0.1): each cell is a column rendered ::text.
type Row struct {
	Cells []string
}

// Host is the per-request datastore seam: a Postgres transaction scoped to this
// module's schema (search_path = <token>). The server adapter implements it over a
// *sql.Tx; tests implement it with a fake. A module reaches Postgres ONLY through
// this interface during a request.
//
// TRANSACTION MODEL: every Query/Exec made while serving ONE request runs inside a
// SINGLE transaction. You do NOT begin/commit — the server commits when your
// handler returns status < 500 and rolls back on a 5xx or any failed statement.
// Always pass values via params (never concatenate them into the SQL string).
type Host interface {
	Query(sql string, params []string) ([]Row, error)
	Exec(sql string, params []string) (uint64, error)
	Log(level, message string)
	// NowMillis is the wall clock in unix milliseconds (e.g. token exp/verify).
	NowMillis() uint64
	// CallExt invokes a base module's EXTENSION service — the hook a base delegates
	// client-specific decisions to (topology a: base → extension over gRPC). envKey
	// names the env var holding the extension's gRPC backend URL (e.g.
	// "MYCEL_IAM_EXT_URL"); the SDK dials it as a ModuleHandler and calls
	// Handle{method, path, body}. configured=false when that env var is unset (no
	// extension wired) — the caller then keeps its base behavior.
	CallExt(envKey, method, path string, body []byte) (status uint16, out []byte, configured bool, err error)
}

// NavEntry is one sidebar link, its path relative to /m/<token>.
type NavEntry struct {
	Label string
	Path  string
}

// Migration is one ordered DDL step the module ships. The server applies these
// (inside the module's schema) at boot when DATABASE_URL is set.
type Migration struct {
	ID  string
	SQL string
}

// RouteFunc is the module's pure request handler — the one function ./logic must
// provide. It is total: it returns a Response for every request (404 for unknown
// paths) and never panics on bad input.
type RouteFunc func(Request, Host) Response

// Descriptor is EVERYTHING a module declares to the SDK — its identity, nav, the
// migrations it ships, and its handler. main.go fills one of these from ./logic and
// hands it to server.Serve. This is the whole module-author surface.
//
// A module supplies EXACTLY ONE handler:
//   - Route   — a pure func(Request, Host) Response. The SDK opens a per-request
//     Postgres transaction (search_path = the module schema) and passes it as Host.
//     Use this for new modules; the logic stays transport-agnostic + unit-testable.
//   - Handler — an existing http.Handler. The SDK bridges the gRPC request to it and
//     maps the response back; the handler owns its own datastore (no SDK Host/tx).
//     Use this to mount a module already written as an http.Handler.
type Descriptor struct {
	Token           string       // PG schema + /m/<token>/* prefix + <token>-app element
	ContractVersion string       // semver of the module contract this builds against
	MenuGroup       string       // sidebar header the nav appears under
	Nav             []NavEntry   // sidebar links
	UIEntry         string       // optional UI bundle path (else taken from the manifest)
	Migrations      []Migration  // ordered DDL, applied at boot when DATABASE_URL is set
	Route           RouteFunc    // pure handler (SDK provides a tx-scoped Host)
	Handler         http.Handler // OR an existing http.Handler (the SDK bridges gRPC↔HTTP)

	// OpenAPI is the module's own OpenAPI 3.x document (JSON), returned verbatim on
	// Describe. Paths are declared relative to the module's root (e.g. "/roles", NOT
	// "/m/<token>/roles") — the host namespaces them under /m/<token> when it
	// federates the app's single spec. Typically `//go:embed openapi.json`. Empty ⇒
	// the host renders a generic passthrough operation for this module instead.
	OpenAPI []byte

	// Verify, if set, is a DEPLOY-TIME readiness gate the SDK runs at startup (with a
	// few retries, since a wired collaborator may still be booting). Use it to enforce a
	// contract this module REQUIRES of a collaborator it depends on — the canonical case
	// being a BASE module verifying that a client's EXTENSION service honors the
	// extension contract: probe it via Host.CallExt and validate the responses. Return
	// nil when there is nothing to enforce (e.g. no extension wired — an unconfigured
	// collaborator is a legitimate choice, not a breach). A non-nil error is FAIL-CLOSED:
	// the SDK logs it loudly and holds the module NOT_SERVING (so k8s readiness keeps the
	// whole app out of rotation) until a redeploy fixes it — a non-conformant collaborator
	// takes the app down rather than silently degrading. The returned error's message
	// should BE the human-readable conformance report. This is the strict gate; keep
	// per-request RUNTIME CallExt calls best-effort.
	Verify func(Host) error

	// Shutdown, if set, is the module's OWN graceful-stop hook — for in-process state
	// the SDK cannot know about, most importantly a background queue. The SDK runs it
	// AFTER GracefulStop, so no handler can still be enqueueing into something you are
	// already draining, and BEFORE the datastore is closed and the socket removed, so
	// the hook may still use the DB.
	//
	// WHY THIS EXISTS: GracefulStop drains in-flight RPCs and nothing else. A handler
	// that only did a channel send finishes in microseconds, so without this hook the
	// process exits with the queue still in memory — events that Emit already accepted,
	// that no drop counter recorded, and that no error surfaced anywhere. Invisible in
	// dev (you rarely restart under load), permanent in production (every rolling
	// deploy, scale-down and HPA scale-in).
	//
	// The ctx carries a bounded deadline (MYCEL_SHUTDOWN_TIMEOUT_MS, default 8000 —
	// under k8s' default 30s grace period even after shutdownGrace). A returned error
	// is logged, not fatal: the process is exiting either way, so the error's job is to
	// leave a trace of what could not be delivered.
	Shutdown func(context.Context) error
}
