#!/usr/bin/env bash
# Checks that the one template we change is still the generator's own, plus the lines
# we meant to change. internal/api/templates/strict-interface.tmpl is a copy of the
# template that oapi-codegen (at the version pinned in internal/api/generate.go) uses
# to write JSON responses, with two edits. If the generator's version moves and its
# template changed, our copy would silently keep the old one; this compares the real
# upstream file with ours and requires the difference to be exactly the committed one
# (internal/api/templates/strict-interface.tmpl.diff).
#
# To update after a deliberate change or a new generator version:
#   scripts/check-template.sh --update
set -euo pipefail
cd "$(dirname "$0")/.."

scratch=$(mktemp -d)
trap 'rm -rf "$scratch"' EXIT

version=$(sed -n 's|.*oapi-codegen/v2/cmd/oapi-codegen@\(v[^ ]*\) .*|\1|p' internal/api/generate.go | head -1)
[ -n "$version" ] || { echo "check-template: cannot find the generator version in internal/api/generate.go" >&2; exit 2; }

dir=$(cd "$scratch" && GOFLAGS=-mod=mod go mod download -json "github.com/oapi-codegen/oapi-codegen/v2@$version" | sed -n 's/.*"Dir": "\(.*\)",/\1/p')
upstream="$dir/pkg/codegen/templates/strict/strict-interface.tmpl"
[ -f "$upstream" ] || { echo "check-template: no template at $upstream" >&2; exit 2; }

actual="$scratch/actual.diff"
rc=0
diff "$upstream" internal/api/templates/strict-interface.tmpl > "$actual" || rc=$?
[ "$rc" -le 1 ] || { echo "check-template: diff failed (exit $rc)" >&2; exit 2; }   # 1 = they differ, as expected

if [ "${1:-}" = "--update" ]; then
  cp "$actual" internal/api/templates/strict-interface.tmpl.diff
  echo "check-template: recorded the difference from oapi-codegen $version"
  exit 0
fi
if ! diff -u internal/api/templates/strict-interface.tmpl.diff "$actual"; then
  echo "check-template: our template differs from oapi-codegen $version's in ways other than the recorded ones." >&2
  echo "Rebase internal/api/templates/strict-interface.tmpl on the upstream file, then run scripts/check-template.sh --update." >&2
  exit 1
fi
echo "ok   the strict-server template is oapi-codegen $version's, plus the recorded edits"
