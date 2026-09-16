#!/bin/sh
set -eu

if [ "$#" -ne 2 ]; then
    echo "uso: $0 VERSIONE DIRECTORY_OUTPUT" >&2
    exit 2
fi

version=$1
output_dir=$2

if ! printf '%s\n' "$version" | grep -Eq '^[0-9]+\.[0-9]+\.[0-9]+$'; then
    echo "versione non valida: usare il formato 1.2.3" >&2
    exit 2
fi

if [ -e "$output_dir" ]; then
    echo "la directory di output esiste gia: $output_dir" >&2
    exit 2
fi

if [ "$(./scripts/go.sh env GOOS)" != "linux" ] || [ "$(./scripts/go.sh env GOARCH)" != "amd64" ]; then
    echo "questa release deve essere compilata nativamente su Linux AMD64" >&2
    exit 2
fi

archive="onlybackup_${version}_linux_amd64"
stage="$output_dir/$archive"
mkdir -p "$stage/bin" "$stage/docs" "$stage/deploy" "$stage/scripts"

for name in onlybackup onlybackup-admin onlybackup-inspect onlybackup-receiver onlybackup-recover onlybackup-writer; do
    CGO_ENABLED=1 GOOS=linux GOARCH=amd64 ./scripts/go.sh build \
        -buildvcs=false -trimpath -o "$stage/bin/$name" "./cmd/$name"
done

cp README.md LICENSE "$stage/"
cp docs/INSTALL-DEBIAN.md docs/INSTALL.md docs/PROTOCOL.md docs/RESTORE-TEST.md docs/SECURITY.md "$stage/docs/"
cp deploy/onlybackup-receiver.service deploy/onlybackup-writer.service "$stage/deploy/"
cp scripts/backup-mysql.sh "$stage/scripts/"

source_date_epoch=${SOURCE_DATE_EPOCH:-$(git show -s --format=%ct HEAD)}
tar --sort=name --mtime="@$source_date_epoch" --owner=0 --group=0 --numeric-owner \
    -C "$output_dir" -czf "$output_dir/$archive.tar.gz" "$archive"
rm -r "$stage"

(
    cd "$output_dir"
    sha256sum "$archive.tar.gz" > SHA256SUMS
)

printf 'creati %s e %s\n' "$output_dir/$archive.tar.gz" "$output_dir/SHA256SUMS"
