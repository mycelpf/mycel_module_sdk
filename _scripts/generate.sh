#!/usr/bin/env bash
# generate.sh — the one generation path for the wire contract (sdk.md S1).
#
#   _scripts/generate.sh [output_root]
#
# Compiles every .proto under contracts/ from the repository root, so generated files land
# beside their .proto (contracts/modulerpc/v1) and record that path as their source.
# output_root defaults to the repository root; _scripts/check_generated.sh passes a
# temporary directory so verification never writes into the checkout.
#
# Fails when protoc or a plugin is missing or reports a version other than the one pinned
# in contracts/tooling.versions.
set -euo pipefail
repository_root="$(cd "$(dirname "$0")/.." && pwd)"
output_root="${1:-$repository_root}"
manifest="$repository_root/contracts/tooling.versions"

while read -r tool expected; do
  [[ -z "$tool" || "$tool" == \#* ]] && continue
  if ! command -v "$tool" >/dev/null 2>&1; then
    echo "generate: $tool is not installed (want: $expected; see contracts/tooling.versions)" >&2
    exit 1
  fi
  actual="$("$tool" --version 2>&1 | head -n 1)"
  if [[ "$actual" != "$expected" ]]; then
    echo "generate: $tool reports \"$actual\", want \"$expected\" (contracts/tooling.versions)" >&2
    exit 1
  fi
done < "$manifest"

cd "$repository_root"
proto_files=()
while IFS= read -r file; do proto_files+=("$file"); done < <(find contracts -name '*.proto' | LC_ALL=C sort)
if [[ ${#proto_files[@]} -eq 0 ]]; then
  echo "generate: no .proto files under contracts/" >&2
  exit 1
fi
mkdir -p "$output_root"
protoc -I . \
  --go_out="$output_root" --go_opt=paths=source_relative \
  --go-grpc_out="$output_root" --go-grpc_opt=paths=source_relative \
  "${proto_files[@]}"
