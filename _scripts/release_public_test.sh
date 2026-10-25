#!/usr/bin/env bash
# release_public_test.sh — tests _scripts/release_public.sh against throwaway repositories: a
# bare "origin" (the private code repository) and a bare "public" (the release repository).
# Run: _scripts/release_public_test.sh
set -euo pipefail
SCRIPT="$(cd "$(dirname "$0")" && pwd)/release_public.sh"
tmp="$(mktemp -d)"; trap 'rm -rf "$tmp"' EXIT
export GIT_AUTHOR_NAME=test GIT_AUTHOR_EMAIL=test@example.com GIT_COMMITTER_NAME=test GIT_COMMITTER_EMAIL=test@example.com
export MYCEL_SDK_SKIP_VERIFY=1 MYCEL_SDK_PUBLIC_REMOTE="$tmp/public.git"
fail=0
check() { if eval "$2"; then echo "ok    $1"; else echo "FAIL  $1"; fail=1; fi; }

git init -q --bare "$tmp/origin.git"; git init -q --bare "$tmp/public.git"
git clone -q "$tmp/origin.git" "$tmp/work" 2>/dev/null; cd "$tmp/work"
git checkout -q -b main
mkdir -p _scripts contracts/typescript rust
cp "$SCRIPT" _scripts/release_public.sh
printf '{\n\t"version": "0.2.0"\n}\n' > contracts/typescript/package.json
printf '[package]\nversion = "0.2.0"\n' > rust/Cargo.toml
echo one > contract.proto; git add -A; git commit -q -m "first"
git push -q origin main
git push -q "$tmp/public.git" main:main                       # the public repository's existing history
echo two > contract.proto; git commit -qam "private step"; echo three > contract.proto; git commit -qam "private step 2"
git push -q origin main
public_before="$(git ls-remote "$tmp/public.git" refs/heads/main | cut -f1)"

echo dirty > extra.txt
check "refuses a dirty tree" '! _scripts/release_public.sh v0.2.0 >/dev/null 2>&1'
rm extra.txt
check "refuses a tag that does not match the package versions" '! _scripts/release_public.sh v0.3.0 >/dev/null 2>&1'
echo four > contract.proto; git commit -qam "unpushed"
check "refuses an unpushed HEAD" '! _scripts/release_public.sh v0.2.0 >/dev/null 2>&1'
git reset -q --hard origin/main
check "dry run succeeds" '_scripts/release_public.sh v0.2.0 --dry-run >/dev/null 2>&1'
check "dry run pushes nothing" '[ "$(git ls-remote "$tmp/public.git" refs/heads/main | cut -f1)" = "$public_before" ] && ! git ls-remote --exit-code --tags "$tmp/public.git" v0.2.0 >/dev/null'
check "publishes" '_scripts/release_public.sh v0.2.0 >/dev/null 2>&1'
pub="$(git ls-remote "$tmp/public.git" refs/heads/main | cut -f1)"
git fetch -q "$tmp/public.git" main
check "public main gains exactly one commit, on top of its old head" '[ "$(git rev-list --count "$public_before..$pub")" = 1 ] && [ "$(git rev-parse "$pub^")" = "$public_before" ]'
check "the release commit has exactly the released files" '[ "$(git rev-parse "$pub^{tree}")" = "$(git rev-parse "HEAD^{tree}")" ]'
check "no private commit is published" '! git log --format=%s "$pub" | grep -q "private step"'
check "the tag is annotated and points at the release commit" '[ "$(git ls-remote "$tmp/public.git" "refs/tags/v0.2.0^{}" | cut -f1)" = "$pub" ] && [ "$(git cat-file -t "$(git ls-remote "$tmp/public.git" refs/tags/v0.2.0 | cut -f1)")" = tag ]'
check "the tag is not left in the private clone" '! git rev-parse -q --verify refs/tags/v0.2.0 >/dev/null'
check "refuses a tag that is already public" '! _scripts/release_public.sh v0.2.0 >/dev/null 2>&1'
[ "$fail" = 0 ] && echo PASS || { echo FAIL; exit 1; }
