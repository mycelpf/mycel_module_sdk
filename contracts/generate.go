// Package contracts holds the host↔module wire contract and its code generation.
//
// `go generate ./contracts/...` runs _scripts/generate.sh, which compiles every .proto
// under contracts/ from the repository root, so the generated code lands beside its
// .proto in contracts/modulerpc/v1 (the canonical import path). The generators are
// pinned in contracts/tooling.versions and the script refuses other versions.
// _scripts/check_generated.sh (run by _scripts/verify.sh) regenerates into a temporary
// directory and fails on stale, missing or unexpected generated files.
//
// The previous directive ran protoc from contracts/ with output to "..", which wrote a
// second copy of the contract to modulerpc/v1 that the drift check never looked at.
package contracts

//go:generate bash ../_scripts/generate.sh
