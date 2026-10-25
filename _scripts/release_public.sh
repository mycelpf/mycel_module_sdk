#!/usr/bin/env bash
# release_public.sh — publish one release of the module contract to the public repository.
#
#   _scripts/release_public.sh vX.Y.Z             # check, then publish
#   _scripts/release_public.sh vX.Y.Z --dry-run   # check and build the release commit; push nothing
#
# CODE lives in cogniworks/mycel_module_sdk (this repository, git `origin`), with its full
# history. RELEASES live in mycelpf/mycel_module_sdk, the module path downstream code imports:
# one commit per release whose files are exactly the released tree, tagged vX.Y.Z. Development
# history is never published, so the public repository holds only releases.
#
# The release is the contract (contracts/, the generated Go, TypeScript and Rust bindings) with
# its example implementation (server/, wire/) and LICENSE/NOTICE (Apache-2.0).
#
# Refuses unless: the tree is clean and pushed to origin/main; the tag matches the version in
# contracts/typescript/package.json and rust/Cargo.toml; the tag is not yet public;
# _scripts/verify.sh passes (build, vet, race tests, TypeScript, generated code matches the
# .proto files) and the Rust crate tests pass with its lockfile. The push is a fast-forward.
#
# Env: MYCEL_SDK_PUBLIC_REMOTE overrides the public repository (tests use a local bare repo).
#      MYCEL_SDK_SKIP_VERIFY=1 skips verify.sh and the Rust tests (tests of this script only).
set -euo pipefail
cd "$(cd "$(dirname "$0")/.." && pwd)"
PUBLIC="${MYCEL_SDK_PUBLIC_REMOTE:-https://github.com/mycelpf/mycel_module_sdk.git}"
TAG="${1:-}"
DRY=0
[ "${2:-}" = --dry-run ] && DRY=1
[[ "$TAG" =~ ^v([0-9]+\.[0-9]+\.[0-9]+)$ ]] || { echo "usage: _scripts/release_public.sh vX.Y.Z [--dry-run]"; exit 2; }
VERSION="${BASH_REMATCH[1]}"

# 1. the code repository, clean and pushed.
[ -z "$(git status --porcelain)" ] || { echo "✗ working tree dirty — commit and push first"; exit 1; }
git fetch -q origin main
[ "$(git rev-parse HEAD)" = "$(git rev-parse origin/main)" ] || { echo "✗ HEAD is not origin/main — release only what is pushed to main"; exit 1; }

# 2. one version everywhere.
ts="$(sed -n 's/^[[:space:]]*"version": "\([^"]*\)".*/\1/p' contracts/typescript/package.json | head -1)"
rs="$(sed -n 's/^version = "\([^"]*\)"/\1/p' rust/Cargo.toml | head -1)"
[ "$ts" = "$VERSION" ] && [ "$rs" = "$VERSION" ] || { echo "✗ $TAG but package.json says $ts and Cargo.toml says $rs"; exit 1; }

# 3. the public side: the tag is new, and its main is a known base.
git fetch -q "$PUBLIC" main
base="$(git rev-parse FETCH_HEAD)"
if git ls-remote --exit-code --tags "$PUBLIC" "refs/tags/$TAG" >/dev/null 2>&1; then
  echo "✗ $TAG is already public"; exit 1
fi

# 4. checks, on exactly this tree.
if [ "${MYCEL_SDK_SKIP_VERIFY:-0}" != 1 ]; then
  _scripts/verify.sh
  (cd rust && cargo test --locked --quiet)
fi

# 5. the release commit: this tree, on top of the public history, nothing else.
tree="$(git rev-parse 'HEAD^{tree}')"
source="$(git rev-parse --short=12 HEAD)"
if [ "$(git rev-parse "$base^{tree}")" = "$tree" ]; then
  echo "✗ the public repository already has exactly these files; nothing to release"; exit 1
fi
release="$(printf 'mycel_module_sdk %s\n\nThe module contract at %s: contracts, generated Go, TypeScript and Rust\nbindings, and the example implementation. Released from %s.\n' "$TAG" "$VERSION" "$source" |
  git commit-tree "$tree" -p "$base")"
[ "$(git rev-parse "$release^{tree}")" = "$tree" ] || { echo "✗ release commit does not match the checked tree"; exit 1; }
echo "release commit $(git rev-parse --short "$release") on public $(git rev-parse --short "$base"), files = $source"
if [ "$DRY" = 1 ]; then echo "dry run: nothing pushed"; exit 0; fi

# 6. publish: fast-forward main, then the annotated tag.
git tag -a -m "mycel_module_sdk $TAG" "$TAG" "$release"
git push "$PUBLIC" "$release:refs/heads/main"
git push "$PUBLIC" "refs/tags/$TAG"
git tag -d "$TAG" >/dev/null  # the tag belongs to the public history only
echo "✓ published $TAG to $PUBLIC"
echo "  check: go list -m github.com/mycelpf/mycel_module_sdk@$TAG"
