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
	Method string
	Path   string // path under /m/<token> (no leading host prefix)
	Query  string // raw query string, no leading '?'
	// Headers is the legacy single-value view: lower-cased names, repeated values joined
	// with ", " (Cookie with "; "). Kept for source compatibility; HeaderFields is the
	// lossless form (sdk.md S4).
	Headers map[string]string
	// HeaderFields holds every request header with all of its values in order, names
	// lower-cased (sdk.md S4).
	HeaderFields []HeaderField
	Body         []byte

	// User is the verified user app_core attached to this request (sdk.md S11.4). It is
	// the zero User when app_core verified no user credential: public routes and
	// network-service routes. The SDK never builds it from a header; a caller-supplied
	// x-mycel-user header is ignored.
	User User

	// ctx is the request's context; see Context.
	ctx context.Context
}

// Context returns the request's context (implementation decision D4): the server sets the
// gRPC call context, so cancellation and the deadline from app_core reach module code, and
// Host calls made for this request carry it. A Request built without the server returns
// context.Background.
func (r Request) Context() context.Context {
	if r.ctx == nil {
		return context.Background()
	}
	return r.ctx
}

// WithContext returns a copy of r that carries ctx.
func (r Request) WithContext(ctx context.Context) Request {
	r.ctx = ctx
	return r
}

// Response is the module's reply.
//
// Headers are taken from HeaderFields when it is non-empty; Headers is then ignored.
// Otherwise the legacy Headers map is read through HeaderFieldsFromLegacyMap: several
// Set-Cookie values joined by a newline become separate cookies (sdk.md S4). A header
// with an invalid name or CR, LF or NUL in a value turns the reply into a 500 error. If
// there is no content-type, the server adapter sets application/json, except on 204 and
// 304. Body is sent byte for byte; it is never parsed or transcoded.
type Response struct {
	Status       uint16
	Headers      map[string]string
	HeaderFields []HeaderField
	Body         string
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
	// Group is the menu group this entry appears under (sdk.md S11.2). Empty means the
	// module's MenuGroup. A non-empty Group must not be only spaces. Entries keep their
	// declared order; groups appear in the order of their first entry.
	Group string
}

// Migration is one ordered DDL step the module ships. The server applies these
// (inside the module's schema) at boot when DATABASE_URL is set.
type Migration struct {
	ID  string
	SQL string
}

// RouteFunc is the module's pure request handler — the one function ./logic must
// provide. It is total: it returns a Response for every request (404 for unknown
// paths) and never panics on bad input. Request.Context is cancelled when app_core gives
// up on the call; long work should watch it.
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
	// Describe and spliced into the app's single federated spec.
	//
	// Declare paths ALREADY PREFIXED with /m/<token> — "/m/roles/roles", not "/roles".
	// The federated spec is served at the host's origin, so an unprefixed path advertises
	// a URL that does not exist: the operation resolves to /roles when the host actually
	// serves it at /m/<token>/roles, and "Try it" in the API docs 404s. Namespace
	// components.schemas and operationIds by token too — every module's fragment merges
	// into ONE paths map and ONE schemas map, so a bare "task" or "getHealth" from two
	// modules silently overwrites the other.
	//
	// (This comment previously said the opposite — that paths were relative and the host
	// prefixed them. It never did; it splices fragments verbatim. Four of five modules
	// followed the prefixed convention and one followed this doc, which is exactly the
	// bug that convention drift produces.)
	//
	// Typically `//go:embed openapi.json`. Empty ⇒ the host renders a generic passthrough
	// operation for this module instead.
	OpenAPI []byte

	// Verify, if set, is a DEPLOY-TIME readiness gate the SDK runs at startup. Use it to
	// enforce a contract this module REQUIRES of a collaborator — the canonical case being
	// a BASE module verifying that a client's EXTENSION service honors the extension
	// contract: probe it via Host.CallExt and validate the responses. Return nil when
	// there is nothing to enforce (an unconfigured collaborator is a legitimate choice).
	// A non-nil error is FAIL-CLOSED: the module stays NOT_SERVING and Handle answers 503
	// verification_pending, and the SDK retries with capped exponential backoff and
	// jitter until it passes, so a collaborator that comes up or is fixed brings this
	// module back without a redeploy. The error message is the conformance report; it is
	// logged with credentials redacted and never returned to callers (sdk.md S8). Keep
	// per-request RUNTIME CallExt calls best-effort.
	//
	// Legacy form: Verify cannot be cancelled; shutdown waits for a running call only
	// through CallExt, which carries the shutdown context. Prefer VerifyContext.
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

	// DescriptorSchemaMajor selects the discovery schema (sdk.md S2). 0 is the legacy
	// descriptor: the host may infer a UI from UIEntry through its legacy adapter only.
	// 1 is the v0.2 typed declaration: UI nil then means the module has no UI, no
	// navigation fallback and no entry asset. Other values fail boot.
	DescriptorSchemaMajor uint32

	// UI is the typed UI declaration (UX A10, A11). Allowed only with
	// DescriptorSchemaMajor 1. With UI set, UIEntry must be empty or equal UI.EntryAsset.
	UI *UIDescriptor

	// Capabilities are the module's declared capability names, reported on Describe.
	Capabilities []string

	// NEEDED_MYCEL_LICENSE lists the license catalog product IDs this module needs, for
	// example []string{"clc"} (license §5.2, core I21). Every listed product must be
	// granted; empty means no license is needed. It is set in code so it is compiled into
	// the binary; nothing at run time can change it. Unknown or repeated IDs, or IDs out
	// of catalog order, fail ValidateNeededMycelLicense and boot. app_core does the
	// license check; the SDK only reports the list on Describe.
	NEEDED_MYCEL_LICENSE []string //nolint:revive // the name is the design's contract name

	// RouteClasses marks exact routes for app_core's admission matrix (sdk.md S11.1).
	// app_core accepts them only from the mycel_iam module.
	RouteClasses []RouteClassDeclaration

	// VerifyContext is the cancellable form of Verify (sdk.md S3). Set at most one of
	// Verify and VerifyContext; setting both fails boot. Each attempt gets a context with
	// a deadline (default 5 seconds) derived from the server lifetime, cancelled when
	// shutdown begins; the hook must honour it, and Host.CallExt calls made through the
	// given Host carry it. The Host has no database.
	VerifyContext func(context.Context, Host) error

	// Start, if set, runs once after startup verification succeeds and before the
	// module reports SERVING (sdk.md S11.3). It must return promptly: long work runs in
	// goroutines started through background.Go, which the SDK waits for at shutdown.
	//
	// ctx is the server lifetime context. At shutdown the SDK drains RPCs, then cancels
	// ctx, then waits for background work, then runs Shutdown, then closes the datastore,
	// all inside the 15-second grace. store uses the same schema and pool as the route
	// adapter, but with no per-request transaction: each statement, or each explicit
	// Transaction, commits on its own. An error from Start is a permanent boot error: the
	// process exits non-zero. Start runs in every replica; singleton work takes a lock.
	// Start is allowed only with Route: with Handler, the module owns its datastore and
	// boot fails.
	Start func(ctx context.Context, store Datastore, background Background) error
}

// Statements runs SQL against the module's own schema. Pass values as params; never
// concatenate them into the SQL string.
type Statements interface {
	Query(ctx context.Context, sql string, params []string) ([]Row, error)
	Exec(ctx context.Context, sql string, params []string) (uint64, error)
}

// Datastore is the start hook's datastore handle (sdk.md S11.3). Query and Exec each
// commit on their own. Transaction runs fn inside one transaction, which commits when fn
// returns nil and rolls back otherwise.
type Datastore interface {
	Statements
	Transaction(ctx context.Context, fn func(Statements) error) error
}

// Background starts work that outlives the start hook. The SDK waits for every function
// started here before it runs Shutdown and closes the datastore. Each function receives
// the server lifetime context and must return once it is cancelled.
type Background interface {
	Go(fn func(ctx context.Context))
}
