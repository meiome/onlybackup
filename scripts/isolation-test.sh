#!/usr/bin/env bash
# Run only the project's test binaries inside a disposable, networkless container.
# Build first: make build; ./scripts/go.sh test -c -o bin/onlybackup-system.test ./tests/system
set -euo pipefail
project_dir=$(cd "$(dirname "$0")/.." && pwd)
test_image=${ONLYBACKUP_TEST_IMAGE:-debian:bookworm-slim}
[[ -x "$project_dir/bin/onlybackup-system.test" ]] || { echo 'Compilare prima il test: make isolation-test' >&2; exit 1; }
exec docker run --rm --network none --read-only \
    --tmpfs /tmp:rw,nosuid,nodev,mode=1777 \
    --mount "type=bind,src=$project_dir/bin,dst=/onlybackup,readonly" \
    -e ONLYBACKUP_BIN=/onlybackup -e ONLYBACKUP_ISOLATION=1 \
    "$test_image" /onlybackup/onlybackup-system.test -test.v
