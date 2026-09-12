#!/usr/bin/env bash
# Usage: backup-mysql.sh DATABASE LOCAL_DIR CLIENT_CONFIG
# Configure MySQL credentials via ~/.my.cnf or mysql_config_editor, never in arguments.
set -euo pipefail
umask 077
if [[ $# != 3 ]]; then echo "Uso: $0 DATABASE DIRECTORY_LOCALE CONFIG_CLIENT" >&2; exit 2; fi
database=$1
local_dir=$2
client_config=$3
mkdir -p -- "$local_dir"
partial=$(mktemp "$local_dir/mysql-$(date -u +%Y%m%dT%H%M%SZ)-XXXXXXXX.sql.part")
trap 'if [[ -n "$partial" ]]; then rm -f -- "$partial"; fi' EXIT
mysqldump --single-transaction -- "$database" > "$partial"
backup=${partial%.part}
# Publication without replacement, even if another script is running.
ln -- "$partial" "$backup"
rm -- "$partial"
partial=
printf 'Copia locale: %s\n' "$backup" >&2
onlybackup send --config "$client_config" --description "Backup MySQL: $database" "$backup"
