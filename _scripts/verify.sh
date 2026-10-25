#!/usr/bin/env bash
# verify.sh — the authoritative verification: build, vet, race tests, and the check that
# the generated contract code matches the .proto files (sdk.md S1).
#
# The generation check needs protoc and the two plugins at the versions pinned in
# contracts/tooling.versions. When they are missing this script FAILS; it does not skip.
# For a quicker local loop without the generators, run _scripts/verify.sh --without-generation,
# which says plainly that verification is incomplete and exits non-zero.
set -euo pipefail
cd "$(dirname "$0")/.."

echo "▶ format"; test -z "$(gofmt -l .)" || { gofmt -l .; echo "  ✗ gofmt"; exit 1; }
echo "▶ build";  go build ./...
echo "▶ vet";    go vet ./...
echo "▶ test";   go test -race ./...
echo "▶ typescript"; (cd contracts/typescript && npm ci --no-audit --no-fund >/dev/null && npm run --silent check && npm run --silent build && npm test --silent)

if [[ "${1:-}" == "--without-generation" ]]; then
  echo "  ! generation check skipped on request — verification is NOT complete"
  exit 2
fi
echo "▶ generation"; _scripts/check_generated.sh
