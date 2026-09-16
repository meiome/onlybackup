#!/bin/sh
set -eu

if [ "$#" -ne 2 ]; then
    echo "uso: $0 VERSIONE DIRECTORY_OUTPUT_ESISTENTE" >&2
    exit 2
fi

version=$1
output_dir=$2
if ! printf '%s\n' "$version" | grep -Eq '^[0-9]+\.[0-9]+\.[0-9]+$'; then
    echo "versione non valida: usare il formato 1.2.3" >&2
    exit 2
fi
if [ ! -d "$output_dir" ] || [ ! -f "$output_dir/SHA256SUMS" ]; then
    echo "creare prima il pacchetto Linux e SHA256SUMS nella directory di output" >&2
    exit 2
fi

archive="onlybackup_${version}_windows_amd64"
stage="$output_dir/$archive"
zip_path="$output_dir/$archive.zip"
if [ -e "$stage" ] || [ -e "$zip_path" ]; then
    echo "staging o archivio Windows gia esistente" >&2
    exit 2
fi

mkdir -p "$stage/bin" "$stage/docs" "$stage/scripts"
CGO_ENABLED=0 GOOS=windows GOARCH=amd64 ./scripts/go.sh build \
    -buildvcs=false -trimpath -o "$stage/bin/onlybackup.exe" ./cmd/onlybackup
CGO_ENABLED=0 GOOS=windows GOARCH=amd64 ./scripts/go.sh build \
    -buildvcs=false -trimpath -o "$stage/bin/onlybackup-recover.exe" ./cmd/onlybackup-recover

cp README.md LICENSE "$stage/"
cp docs/CLIENT-WINDOWS.md docs/PROTOCOL.md docs/RESTORE-TEST.md docs/SECURITY.md "$stage/docs/"
cp scripts/backup-file.ps1 "$stage/scripts/"

source_date_epoch=${SOURCE_DATE_EPOCH:-$(git show -s --format=%ct HEAD)}
./scripts/go.sh run ./scripts/zip-release.go "$stage" "$zip_path" "$source_date_epoch"
rm -r "$stage"
(
    cd "$output_dir"
    sha256sum "$archive.zip" >> SHA256SUMS
    sort -o SHA256SUMS SHA256SUMS
)
printf 'creato %s\n' "$zip_path"
