#!/bin/sh
# Prints the CHANGELOG.md section for VERSION (for example 0.5.3 or v0.5.3).
# Fails if the section is missing or empty, so a release cannot be published
# without notes.
set -eu

if [ "$#" -ne 1 ]; then
    echo "uso: $0 VERSIONE" >&2
    exit 2
fi

version=${1#v}
changelog=${CHANGELOG:-CHANGELOG.md}

notes=$(awk -v heading="## $version" '
    $0 == heading { found = 1; next }
    found && /^## / { exit }
    found { print }
' "$changelog" | sed -e '/./,$!d')

if [ -z "$(printf '%s' "$notes" | tr -d '[:space:]')" ]; then
    echo "nessuna nota per $version in $changelog" >&2
    exit 1
fi

printf '%s\n' "$notes"
