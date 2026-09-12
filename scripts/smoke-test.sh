#!/usr/bin/env bash
# Exercises actual binaries, HTTPS, a Unix socket, SQLite and local recovery.
set -euo pipefail
umask 077
cd "$(dirname "$0")/.."
run_dir=$(mktemp -d /tmp/onlybackup-smoke.XXXXXXXX)
writer_pid=
receiver_pid=
cleanup() {
    if [[ -n "$receiver_pid" ]]; then kill "$receiver_pid" 2>/dev/null || true; wait "$receiver_pid" 2>/dev/null || true; fi
    if [[ -n "$writer_pid" ]]; then kill "$writer_pid" 2>/dev/null || true; wait "$writer_pid" 2>/dev/null || true; fi
    if [[ "${KEEP_SMOKE_FILES:-0}" == 1 ]]; then
        echo "File di prova: $run_dir"
    else
        rm -rf -- "$run_dir"
    fi
}
trap cleanup EXIT
fail() { echo "$*" >&2; cat "$run_dir/writer.log" "$run_dir/receiver.log" 2>/dev/null >&2 || true; exit 1; }
# The OS chooses an available port. It is released immediately before the receiver starts.
port=$(python3 - <<'PY'
import socket
with socket.socket() as s:
    s.bind(('127.0.0.1',0))
    print(s.getsockname()[1])
PY
)
openssl req -x509 -newkey rsa:2048 -nodes -days 1 -subj '/CN=localhost' \
    -addext 'subjectAltName=DNS:localhost,IP:127.0.0.1' \
    -keyout "$run_dir/tls.key" -out "$run_dir/tls.crt" >"$run_dir/cert.log" 2>&1
bin/onlybackup-admin --state "$run_dir/state" init >"$run_dir/init.json"
bin/onlybackup-admin --state "$run_dir/state" profiles set --name pippo --total 10MiB --max-backup 2MiB --daily 10 --concurrent 2 >"$run_dir/profile.json"
bin/onlybackup-admin --state "$run_dir/state" keys create --name prova --profile pippo --out "$run_dir/client.key" >"$run_dir/key.json"
key_id=$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["id"])' "$run_dir/key.json")
bin/onlybackup-writer --state "$run_dir/state" --socket "$run_dir/writer.sock" --reserve-free 0 >"$run_dir/writer.log" 2>&1 &
writer_pid=$!
for _ in {1..100}; do [[ -S "$run_dir/writer.sock" ]] && break; sleep 0.05; done
[[ -S "$run_dir/writer.sock" ]] || fail 'Writer non avviato'
bin/onlybackup-receiver --listen "127.0.0.1:$port" --socket "$run_dir/writer.sock" --tls-cert "$run_dir/tls.crt" --tls-key "$run_dir/tls.key" >"$run_dir/receiver.log" 2>&1 &
receiver_pid=$!
ready=0
for _ in {1..100}; do
    status=$(curl --silent --cacert "$run_dir/tls.crt" -o /dev/null -w '%{http_code}' "https://127.0.0.1:$port/v1/backups" || true)
    if [[ "$status" == 405 ]]; then ready=1; break; fi
    sleep 0.05
done
[[ "$ready" == 1 ]] || fail 'Receiver non avviato'
python3 - "$run_dir/db.sql" <<'PY'
import sys
with open(sys.argv[1], 'wb') as f:
    f.write(b'CREATE TABLE example (id INTEGER);\n' * 10000)
PY
bin/onlybackup send --plaintext --url "https://127.0.0.1:$port" --key-file "$run_dir/client.key" --ca-file "$run_dir/tls.crt" --description 'Backup smoke test' "$run_dir/db.sql" >"$run_dir/receipt.json"
id=$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["id"])' "$run_dir/receipt.json")
[[ -f "$run_dir/state/backups/$id.backup" ]] || fail 'Backup non trovato'
cmp "$run_dir/db.sql" "$run_dir/state/backups/$id.backup"
bin/onlybackup-inspect --state "$run_dir/state" status --id "$id" >"$run_dir/status.json"
bin/onlybackup-recover --state "$run_dir/state" --id "$id" --out "$run_dir/recovered.sql" >"$run_dir/recovery.json"
cmp "$run_dir/db.sql" "$run_dir/recovered.sql"
if bin/onlybackup-recover --state "$run_dir/state" --id "$id" --out "$run_dir/recovered.sql" > /dev/null 2>&1; then fail 'Recovery ha sovrascritto un file'; fi
for method in GET DELETE PUT PATCH HEAD; do
    status=$(curl --silent --cacert "$run_dir/tls.crt" -X "$method" -o /dev/null -w '%{http_code}' --max-time 3 "https://127.0.0.1:$port/v1/backups" || true)
    [[ "$status" == 405 ]] || fail "Metodo $method non bloccato: $status"
done
bin/onlybackup-admin --state "$run_dir/state" keys revoke --id "$key_id" >"$run_dir/revoked.json"
if bin/onlybackup send --quiet --plaintext --url "https://127.0.0.1:$port" --key-file "$run_dir/client.key" --ca-file "$run_dir/tls.crt" --description revocata "$run_dir/db.sql" >"$run_dir/rejected.out" 2>"$run_dir/rejected.err"; then fail 'Chiave revocata accettata'; fi
[[ -f "$run_dir/state/backups/$id.backup" ]] || fail 'La revoca ha cancellato il backup'
python3 - "$run_dir" <<'PY'
import json,pathlib,sys
root=pathlib.Path(sys.argv[1])
r=json.loads((root/'receipt.json').read_text())
s=json.loads((root/'status.json').read_text())
assert r['status']=='complete'
assert s['original_name']=='db.sql'
assert 'HTTP 401' in (root/'rejected.err').read_text()
assert len(list((root/'state/backups').iterdir()))==1
assert not list((root/'state/incoming').iterdir())
PY
printf 'OK: HTTPS, upload, SHA-256, catalogo, recupero, nessuna sovrascrittura, API ristrette e revoca.\n'
