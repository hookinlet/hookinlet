#!/usr/bin/env bash
# Makes a copy of this repository's history for publishing: the working notes (plans, instructions
# for tools) are removed from every commit, merge commits do not carry this repository's pull request
# numbers (a new repository numbers its own), and there are no tags or other branches. The tip of the
# copy must be byte for byte the tip of main, and the script checks that.
#
#   scripts/public-history.sh DESTINATION        e.g. ../hookinlet-public
#
# Needs git-filter-repo (https://github.com/newren/git-filter-repo). Push the copy to the new
# repository yourself; this script never pushes anywhere.
set -euo pipefail
dst=${1:?usage: scripts/public-history.sh DESTINATION}
src=$(cd "$(dirname "$0")/.." && pwd)
notes=(PLAN.md CLAUDE.md GUARDRAILS.md)

[ ! -e "$dst" ] || { echo "public-history: $dst exists; remove it first" >&2; exit 1; }
command -v git-filter-repo >/dev/null || { echo "public-history: git-filter-repo is not installed" >&2; exit 1; }

git clone -q --no-local --single-branch --branch main --no-tags "$src" "$dst"
cd "$dst"
args=(--force --invert-paths)
for f in "${notes[@]}"; do args+=(--path "$f"); done
callback=$(mktemp)
trap 'rm -f "$callback"' EXIT
cat > "$callback" <<'PY'
import re
m = message.decode("utf-8")
m = re.sub(r"^Merge pull request #\d+ from [^/\s]+/(\S+)", r"Merge branch '\1'", m)
return m.encode("utf-8")
PY
git filter-repo "${args[@]}" --message-callback "$(cat "$callback")"

[ "$(git rev-parse 'HEAD^{tree}')" = "$(git -C "$src" rev-parse 'main^{tree}')" ] ||
  { echo "public-history: the tip of the copy differs from main's; do not publish it" >&2; exit 1; }
[ -z "$(git log --all --oneline -- "${notes[@]}")" ] ||
  { echo "public-history: a working-notes file is still in the history; do not publish it" >&2; exit 1; }
git remote remove origin 2>/dev/null || true
echo "ok   $dst: $(git log --oneline | wc -l | tr -d ' ') commits, no working notes, tip identical to main"
