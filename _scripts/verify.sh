#!/usr/bin/env bash
# verify.sh — what CI (and you, before pushing) runs: build, vet, race tests, and a
# check that the generated contract stubs still match the .proto.
#
# The codegen check exists because regeneration was a manual command in the README and
# nothing diffed the output. Stubs that silently disagree with the contract they claim to
# implement are the worst kind of drift: everything compiles.
set -euo pipefail
cd "$(dirname "$0")/.."

echo "▶ build";  go build ./...
echo "▶ vet";    go vet ./...
echo "▶ test";   go test -race ./...

echo "▶ codegen"
if ! command -v protoc >/dev/null 2>&1; then
  echo "  ! protoc not installed — skipping the stub-drift check (CI must not skip this)"
  exit 0
fi
if ! command -v protoc-gen-go >/dev/null 2>&1 || ! command -v protoc-gen-go-grpc >/dev/null 2>&1; then
  echo "  ! protoc-gen-go / protoc-gen-go-grpc not on PATH — skipping (see contracts/generate.go)"
  exit 0
fi
go generate ./contracts/... >/dev/null
if ! git diff --quiet -- contracts/; then
  echo "  ✗ generated stubs differ from modulerpc.proto — run: go generate ./contracts/..."
  git --no-pager diff --stat -- contracts/
  exit 1
fi
echo "  ✓ stubs match the proto"
