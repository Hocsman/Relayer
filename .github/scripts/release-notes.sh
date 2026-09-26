#!/usr/bin/env bash
# release-notes.sh VERSION [CHANGELOG]
#
# Prints the CHANGELOG.md section of VERSION (without its heading) followed by
# the compare link the CHANGELOG gives that version, which is what a release
# page shows. Fails when the CHANGELOG has no section for VERSION, so a tag
# whose notes were never written is not published with a bare commit list.
set -euo pipefail

version="${1:?usage: release-notes.sh VERSION [CHANGELOG]}"
changelog="${2:-CHANGELOG.md}"

notes="$(awk -v heading="## [$version] - " '
  index($0, heading) == 1 { inside = 1; next }
  inside && /^## \[/ { exit }
  inside { print }
' "$changelog" | sed -e '/./,$!d' | sed -e ':a' -e '/^\n*$/{$d;N;ba' -e '}')"

if [[ -z "$notes" ]]; then
  printf 'CHANGELOG has no section for %s\n' "$version" >&2
  exit 1
fi

printf '%s\n' "$notes"

compare="$(awk -v label="[$version]: " 'index($0, label) == 1 { print substr($0, length(label) + 1); exit }' "$changelog")"
if [[ -n "$compare" ]]; then
  printf '\n**Full commit list**: %s\n' "$compare"
fi
