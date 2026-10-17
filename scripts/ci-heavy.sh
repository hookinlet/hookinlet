#!/usr/bin/env bash
# Says whether this run of CI should run its heavy checks (the race detector, the
# documentation examples, the image build and the release dry run), by writing
# heavy=true or heavy=false to $GITHUB_OUTPUT.
#
#   a pull request              false: it gets the fast checks
#   a nightly or manual run     true: this is when the heavy checks run
#   a push to main              false only if every changed file is one that no heavy
#                               check reads, and true in any doubt
#
# A file is one no heavy check reads if it is a project document that the release
# archive carries but no test looks inside (CHANGELOG.md and the like) and the change
# only edited it: deleting or renaming it would break the archive, which the release
# dry run checks. Environment: EVENT (github.event_name), BEFORE (github.event.before),
# SHA (github.sha).
set -euo pipefail

out=${GITHUB_OUTPUT:-/dev/stdout}
say() { echo "heavy=$1" >> "$out"; echo "heavy checks: $1 ($2)"; }

case "${EVENT:?EVENT is required}" in
  schedule | workflow_dispatch) say true "$EVENT"; exit 0 ;;
  push) ;;
  *) say false "$EVENT"; exit 0 ;;
esac

before=${BEFORE:-}
sha=${SHA:-HEAD}
if [ -z "$before" ] || [ "$before" = 0000000000000000000000000000000000000000 ]; then
  say true "no previous commit to compare with"; exit 0
fi
if ! git cat-file -e "$before^{commit}" 2>/dev/null; then
  git fetch -q --depth=1 origin "$before" 2>/dev/null || true
fi
if ! changes=$(git diff --name-status "$before" "$sha" 2>/dev/null); then
  say true "could not compare with ${before:0:12}"; exit 0
fi

quiet='^(CHANGELOG|CONTRIBUTING|CODE_OF_CONDUCT|SECURITY|LICENSING)\.md$|^\.github/(ISSUE_TEMPLATE/|PULL_REQUEST_TEMPLATE|CODEOWNERS$|dependabot\.yml$)'
needed=""
while IFS=$'\t' read -r status path rest; do
  [ -n "$status" ] || continue
  if [ "$status" = M ] && printf '%s' "$path" | grep -Eq "$quiet"; then continue; fi
  needed="$path"
  break
done <<< "$changes"

if [ -n "$needed" ]; then
  say true "$needed changed"
else
  say false "only files no heavy check reads were edited"
fi
