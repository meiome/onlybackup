#!/usr/bin/env bash
# End-to-end restore proof using two disposable, socket-only MariaDB instances.
set -euo pipefail
umask 077
cd "$(dirname "$0")/.."

for command in /usr/sbin/mariadbd /usr/bin/mariadb-install-db /usr/bin/mariadb /usr/bin/mysqldump; do
    [[ -x "$command" ]] || { echo "Comando richiesto non disponibile: $command" >&2; exit 2; }
done
for command in openssl curl python3; do
    command -v "$command" >/dev/null || { echo "Comando richiesto non disponibile: $command" >&2; exit 2; }
done
for binary in onlybackup onlybackup-admin onlybackup-vault onlybackup-writer onlybackup-receiver onlybackup-recover; do
    [[ -x "bin/$binary" ]] || { echo "Binario mancante: bin/$binary (eseguire make build)" >&2; exit 2; }
done

run_dir=$(mktemp -d /tmp/onlybackup-mysql-restore.XXXXXXXX)
source_pid=
target_pid=
vault_pid=
writer_pid=
receiver_pid=

stop_pid() {
    local pid=$1
    if [[ -n "$pid" ]] && kill -0 "$pid" 2>/dev/null; then
        kill "$pid" 2>/dev/null || true
        wait "$pid" 2>/dev/null || true
    fi
}

cleanup() {
    stop_pid "$receiver_pid"
    stop_pid "$writer_pid"
    stop_pid "$vault_pid"
    stop_pid "$target_pid"
    stop_pid "$source_pid"
    if [[ "${KEEP_MYSQL_RESTORE_FILES:-0}" == 1 ]]; then
        echo "File di prova conservati in: $run_dir" >&2
    else
        case "$run_dir" in
            /tmp/onlybackup-mysql-restore.*) rm -rf -- "$run_dir" ;;
            *) echo "Directory temporanea inattesa, non rimossa: $run_dir" >&2 ;;
        esac
    fi
}
trap cleanup EXIT

fail() {
    echo "ERRORE: $*" >&2
    for log in "$run_dir"/*.log; do
        [[ -f "$log" ]] || continue
        echo "--- $log" >&2
        tail -n 100 "$log" >&2 || true
    done
    exit 1
}

wait_for_mariadb() {
    local socket=$1 pid=$2 label=$3
    for _ in {1..200}; do
        if /usr/bin/mariadb --no-defaults --protocol=SOCKET --socket="$socket" --user=root \
            --batch --skip-column-names -e 'SELECT 1' >/dev/null 2>&1; then
            return
        fi
        kill -0 "$pid" 2>/dev/null || fail "$label terminato durante l'avvio"
        sleep 0.05
    done
    fail "$label non pronto"
}

init_mariadb() {
    local datadir=$1 log=$2
    mkdir -p -- "$datadir"
    /usr/bin/mariadb-install-db --no-defaults --datadir="$datadir" \
        --auth-root-authentication-method=normal --skip-test-db >"$log" 2>&1
}

start_mariadb() {
    local datadir=$1 socket=$2 pidfile=$3 log=$4
    /usr/sbin/mariadbd --no-defaults --datadir="$datadir" --socket="$socket" \
        --pid-file="$pidfile" --log-error="$log" --skip-networking \
        --max-connections=20 &
    mariadb_started_pid=$!
}

source_dir="$run_dir/mariadb-source"
source_socket="$run_dir/source.sock"
init_mariadb "$source_dir" "$run_dir/source-init.log"
start_mariadb "$source_dir" "$source_socket" "$run_dir/source.pid" "$run_dir/source.log"
source_pid=$mariadb_started_pid
wait_for_mariadb "$source_socket" "$source_pid" "MariaDB sorgente"

/usr/bin/mariadb --no-defaults --protocol=SOCKET --socket="$source_socket" --user=root <<'SQL'
CREATE DATABASE onlybackup_fixture CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci;
USE onlybackup_fixture;
CREATE TABLE customers (
    id INT PRIMARY KEY,
    name VARCHAR(100) NOT NULL UNIQUE
) ENGINE=InnoDB;
CREATE TABLE orders (
    id INT PRIMARY KEY,
    customer_id INT NOT NULL,
    amount DECIMAL(10,2) NOT NULL,
    note VARCHAR(255) NOT NULL,
    CONSTRAINT orders_customer_fk FOREIGN KEY (customer_id) REFERENCES customers(id)
) ENGINE=InnoDB;
INSERT INTO customers VALUES (1, 'Ada'), (2, 'Renato è qui');
INSERT INTO orders VALUES
    (10, 1, 12.50, 'prima riga'),
    (11, 1, 7.25, 'UTF-8: caffè'),
    (12, 2, 101.01, 'relazione conservata');
SQL

/usr/bin/mariadb --no-defaults --protocol=SOCKET --socket="$source_socket" --user=root \
    --batch --skip-column-names onlybackup_fixture \
    -e "SELECT c.id,HEX(c.name),COUNT(o.id),CAST(SUM(o.amount) AS CHAR) FROM customers c JOIN orders o ON o.customer_id=c.id GROUP BY c.id,c.name ORDER BY c.id" \
    >"$run_dir/source.expected"

mkdir -p -- "$run_dir/dump-bin" "$run_dir/local-copies" "$run_dir/encrypted-temp"
cat >"$run_dir/source-client.cnf" <<EOF
[client]
protocol=socket
socket=$source_socket
user=root
EOF
cat >"$run_dir/dump-bin/mysqldump" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
: "${ONLYBACKUP_MYSQL_SOURCE_CONFIG:?configurazione MariaDB sorgente mancante}"
exec /usr/bin/mysqldump --defaults-file="$ONLYBACKUP_MYSQL_SOURCE_CONFIG" "$@"
EOF
chmod 0700 "$run_dir/dump-bin/mysqldump"

port=$(python3 - <<'PY'
import socket
with socket.socket() as sock:
    sock.bind(('127.0.0.1', 0))
    print(sock.getsockname()[1])
PY
)
openssl req -x509 -newkey rsa:2048 -nodes -days 1 -subj '/CN=localhost' \
    -addext 'subjectAltName=DNS:localhost,IP:127.0.0.1' \
    -keyout "$run_dir/tls.key" -out "$run_dir/tls.crt" >"$run_dir/cert.log" 2>&1

bin/onlybackup-admin --state "$run_dir/state" init >"$run_dir/init.json"
bin/onlybackup-admin --state "$run_dir/state" profiles set --name restore-test \
    --total 64MiB --max-backup 16MiB --daily 10 --concurrent 2 >"$run_dir/profile.json"
bin/onlybackup-admin --state "$run_dir/state" keys create --name restore-test \
    --profile restore-test --out "$run_dir/client.key" >"$run_dir/key.json"
bin/onlybackup-recover keygen --out "$run_dir/age-identity.txt" >"$run_dir/age-key.json"
recipient=$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["recipient"])' "$run_dir/age-key.json")

cat >"$run_dir/client-clear.json" <<EOF
{"url":"https://127.0.0.1:$port","key_file":"$run_dir/client.key","ca_file":"$run_dir/tls.crt","plaintext":true}
EOF
cat >"$run_dir/client-encrypted.json" <<EOF
{"url":"https://127.0.0.1:$port","key_file":"$run_dir/client.key","ca_file":"$run_dir/tls.crt","encrypt_to":"$recipient","encrypted_temp_limit_bytes":16777216,"encrypted_temp_dir":"$run_dir/encrypted-temp"}
EOF

bin/onlybackup-vault --state "$run_dir/state" --socket "$run_dir/vault.sock" \
    --reserve-free 0 >"$run_dir/vault.log" 2>&1 &
vault_pid=$!
for _ in {1..200}; do
    [[ -S "$run_dir/vault.sock" ]] && break
    kill -0 "$vault_pid" 2>/dev/null || fail 'vault terminato durante avvio'
    sleep 0.05
done
[[ -S "$run_dir/vault.sock" ]] || fail 'vault non pronto'

bin/onlybackup-writer --socket "$run_dir/writer.sock" --vault-socket "$run_dir/vault.sock" \
    >"$run_dir/writer.log" 2>&1 &
writer_pid=$!
for _ in {1..200}; do
    [[ -S "$run_dir/writer.sock" ]] && break
    kill -0 "$writer_pid" 2>/dev/null || fail 'writer terminato durante avvio'
    sleep 0.05
done
[[ -S "$run_dir/writer.sock" ]] || fail 'writer non pronto'

bin/onlybackup-receiver --listen "127.0.0.1:$port" --socket "$run_dir/writer.sock" \
    --tls-cert "$run_dir/tls.crt" --tls-key "$run_dir/tls.key" \
    >"$run_dir/receiver.log" 2>&1 &
receiver_pid=$!
receiver_ready=0
for _ in {1..200}; do
    status=$(curl --silent --cacert "$run_dir/tls.crt" -o /dev/null -w '%{http_code}' \
        "https://127.0.0.1:$port/v1/backups" || true)
    if [[ "$status" == 405 ]]; then receiver_ready=1; break; fi
    kill -0 "$receiver_pid" 2>/dev/null || fail 'receiver terminato durante avvio'
    sleep 0.05
done
[[ "$receiver_ready" == 1 ]] || fail 'receiver non pronto'

PATH="$run_dir/dump-bin:$PWD/bin:$PATH" ONLYBACKUP_MYSQL_SOURCE_CONFIG="$run_dir/source-client.cnf" \
    scripts/backup-mysql.sh onlybackup_fixture "$run_dir/local-copies" "$run_dir/client-clear.json" \
    >"$run_dir/receipt-clear.json" 2>"$run_dir/send-clear.log"
clear_local=$(sed -n 's/^Copia locale: //p' "$run_dir/send-clear.log" | head -n 1)
[[ -n "$clear_local" && -f "$clear_local" ]] || fail 'copia SQL locale in chiaro non trovata'

PATH="$run_dir/dump-bin:$PWD/bin:$PATH" ONLYBACKUP_MYSQL_SOURCE_CONFIG="$run_dir/source-client.cnf" \
    scripts/backup-mysql.sh onlybackup_fixture "$run_dir/local-copies" "$run_dir/client-encrypted.json" \
    >"$run_dir/receipt-encrypted.json" 2>"$run_dir/send-encrypted.log"
encrypted_local=$(sed -n 's/^Copia locale: //p' "$run_dir/send-encrypted.log" | head -n 1)
[[ -n "$encrypted_local" && -f "$encrypted_local" ]] || fail 'copia SQL locale per invio cifrato non trovata'

clear_id=$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["id"])' "$run_dir/receipt-clear.json")
encrypted_id=$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["id"])' "$run_dir/receipt-encrypted.json")

# Recovery is deliberately local and happens only after the receiver, writer
# gateway and authoritative vault have stopped.
stop_pid "$receiver_pid"
receiver_pid=
stop_pid "$writer_pid"
writer_pid=
stop_pid "$vault_pid"
vault_pid=
stop_pid "$source_pid"
source_pid=

bin/onlybackup-recover --state "$run_dir/state" --id "$clear_id" \
    --out "$run_dir/recovered-clear.sql" >"$run_dir/recover-clear.json"
bin/onlybackup-recover --state "$run_dir/state" --id "$encrypted_id" \
    --identity-file "$run_dir/age-identity.txt" --out "$run_dir/recovered-encrypted.sql" \
    >"$run_dir/recover-encrypted.json"
cmp "$clear_local" "$run_dir/recovered-clear.sql"
cmp "$encrypted_local" "$run_dir/recovered-encrypted.sql"

target_dir="$run_dir/mariadb-target"
target_socket="$run_dir/target.sock"
init_mariadb "$target_dir" "$run_dir/target-init.log"
start_mariadb "$target_dir" "$target_socket" "$run_dir/target.pid" "$run_dir/target.log"
target_pid=$mariadb_started_pid
wait_for_mariadb "$target_socket" "$target_pid" "MariaDB destinazione"

/usr/bin/mariadb --no-defaults --protocol=SOCKET --socket="$target_socket" --user=root \
    -e 'CREATE DATABASE restore_clear CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci; CREATE DATABASE restore_encrypted CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci'
/usr/bin/mariadb --no-defaults --protocol=SOCKET --socket="$target_socket" --user=root \
    restore_clear <"$run_dir/recovered-clear.sql"
/usr/bin/mariadb --no-defaults --protocol=SOCKET --socket="$target_socket" --user=root \
    restore_encrypted <"$run_dir/recovered-encrypted.sql"

for restored in restore_clear restore_encrypted; do
    /usr/bin/mariadb --no-defaults --protocol=SOCKET --socket="$target_socket" --user=root \
        --batch --skip-column-names "$restored" \
        -e "SELECT c.id,HEX(c.name),COUNT(o.id),CAST(SUM(o.amount) AS CHAR) FROM customers c JOIN orders o ON o.customer_id=c.id GROUP BY c.id,c.name ORDER BY c.id" \
        >"$run_dir/$restored.actual"
    cmp "$run_dir/source.expected" "$run_dir/$restored.actual"
    fk_count=$(/usr/bin/mariadb --no-defaults --protocol=SOCKET --socket="$target_socket" --user=root \
        --batch --skip-column-names information_schema \
        -e "SELECT COUNT(*) FROM REFERENTIAL_CONSTRAINTS WHERE CONSTRAINT_SCHEMA='$restored' AND CONSTRAINT_NAME='orders_customer_fk'")
    [[ "$fk_count" == 1 ]] || fail "vincolo esterno assente in $restored"
done

python3 - "$run_dir/recover-clear.json" "$run_dir/recover-encrypted.json" <<'PY'
import json, sys
clear = json.load(open(sys.argv[1]))
encrypted = json.load(open(sys.argv[2]))
assert clear["verified"] is True and clear["content_format"] == "" and clear["decrypted"] is False
assert encrypted["verified"] is True and encrypted["content_format"] == "age-v1" and encrypted["decrypted"] is True
PY

version=$(/usr/sbin/mariadbd --version | sed -n '1p')
printf 'OK: dump reali, pipeline HTTPS-writer-vault in chiaro/cifrato, recupero locale e import MariaDB verificati.\n%s\n' "$version"
