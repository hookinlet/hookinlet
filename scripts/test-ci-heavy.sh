#!/usr/bin/env bash
# Checks scripts/ci-heavy.sh against small throwaway repositories.
set -euo pipefail
script=$(cd "$(dirname "$0")" && pwd)/ci-heavy.sh
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
cd "$tmp"
git init -q -b main .
git config user.email t@example.invalid
git config user.name t
mkdir -p pkg .github/ISSUE_TEMPLATE
echo a > CHANGELOG.md; echo a > README.md; echo a > pkg/a.go; echo a > .github/ISSUE_TEMPLATE/bug.md
git add -A; git commit -q -m base
base=$(git rev-parse HEAD)

fail=0
# expect NAME WANT EVENT BEFORE: run the script at HEAD and compare.
expect() {
  local name=$1 want=$2 event=$3 before=${4:-$base} got
  got=$(EVENT=$event BEFORE=$before SHA=$(git rev-parse HEAD) GITHUB_OUTPUT=/dev/stdout "$script" | grep '^heavy=' || true)
  if [ "$got" != "heavy=$want" ]; then echo "FAIL $name: got '$got', want heavy=$want"; fail=1; else echo "ok   $name"; fi
}
commit() { git add -A; git commit -q -m "$1"; }

expect "a pull request" false pull_request
expect "the nightly run" true schedule
expect "a manual run" true workflow_dispatch
expect "a push with no previous commit" true push 0000000000000000000000000000000000000000
expect "a push whose previous commit is unknown" true push 1111111111111111111111111111111111111111

echo b >> CHANGELOG.md; echo b >> .github/ISSUE_TEMPLATE/bug.md; commit docs
expect "a push that only edits the changelog and a template" false push
echo b >> README.md; commit readme
expect "a push that edits the README, which the archive carries and a doc test may read" true push "$(git rev-parse HEAD~1)"
echo b >> pkg/a.go; commit code
expect "a push that edits Go code" true push "$(git rev-parse HEAD~1)"
git rm -q CHANGELOG.md; commit remove
expect "a push that deletes a document the archive carries" true push "$(git rev-parse HEAD~1)"
echo b >> CHANGELOG.md; echo b >> pkg/a.go; commit both
expect "a push that edits the changelog and code together" true push "$(git rev-parse HEAD~1)"
exit $fail
