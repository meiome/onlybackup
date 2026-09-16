#!/usr/bin/env bash
# Usage: backup-file.sh FILE CONFIG_CLIENT DIRECTORY_RICEVUTE [DESCRIZIONE]
set -euo pipefail
umask 077

if [[ $# -lt 3 || $# -gt 4 ]]; then
    echo "Uso: $0 FILE CONFIG_CLIENT DIRECTORY_RICEVUTE [DESCRIZIONE]" >&2
    exit 2
fi

source_file=$(realpath -- "$1")
client_config=$(realpath -- "$2")
receipt_dir=$3
description=${4:-"Backup file: $(basename -- "$source_file")"}
client=${ONLYBACKUP_CLIENT:-onlybackup}

if [[ ! -f "$source_file" ]]; then
    echo "File sorgente non regolare: $source_file" >&2
    exit 2
fi

mkdir -p -- "$receipt_dir"
timestamp=$(date -u +%Y%m%dT%H%M%S%NZ)
receipt="$receipt_dir/receipt-$timestamp-$$.json"

"$client" send \
    --quiet \
    --config "$client_config" \
    --description "$description" \
    --receipt "$receipt" \
    "$source_file" >/dev/null

printf 'Ricevuta: %s\n' "$receipt"
