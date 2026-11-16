#!/usr/bin/env sh
# Fetch the MiSans faces that are embedded into the binaries.
#
# The font files are not committed: the MiSans license forbids redistributing
# the font on its own, and the repository only carries the notice and the
# original license text. Run this once after cloning, then build as usual.
#
#   pwsh -File scripts/fetch-fonts.ps1      (Windows)
#   sh   scripts/fetch-fonts.sh             (Linux, macOS)

set -eu

ROOT=$(CDPATH='' cd "$(dirname "$0")/.." && pwd)
DESTINATION=${1:-"$ROOT/assets/fonts"}

# Official release archive of MiSans 4.009, the version NOTICE.txt documents.
SOURCE='https://hyperos.mi.com/font-download/MiSans.zip'
WEIGHTS='Regular Medium Bold'

missing() {
    for weight in $WEIGHTS; do
        for format in ttf woff2; do
            if [ ! -f "$DESTINATION/MiSans-$weight.$format" ]; then
                return 0
            fi
        done
    done
    return 1
}

if [ -f "$DESTINATION/fonts.go" ] && ! missing; then
    printf 'MiSans faces already present in %s\n' "$DESTINATION"
    exit 0
fi

WORK=$(mktemp -d "${TMPDIR:-/tmp}/misans.XXXXXX")
trap 'rm -rf "$WORK"' EXIT

printf 'Downloading %s\n' "$SOURCE"
if command -v curl >/dev/null 2>&1; then
    curl -fL --retry 2 -o "$WORK/MiSans.zip" "$SOURCE"
else
    wget -O "$WORK/MiSans.zip" "$SOURCE"
fi

command -v unzip >/dev/null 2>&1 || {
    printf 'unzip is required to extract %s\n' "$SOURCE" >&2
    exit 1
}
unzip -q "$WORK/MiSans.zip" -d "$WORK"

mkdir -p "$DESTINATION"
for weight in $WEIGHTS; do
    for format in ttf woff2; do
        match=$(find "$WORK" -type f -name "MiSans-$weight.$format" | head -n 1)
        if [ -z "$match" ]; then
            printf 'MiSans-%s.%s is missing from %s\n' "$weight" "$format" "$SOURCE" >&2
            exit 1
        fi
        cp "$match" "$DESTINATION/"
        printf '  %s\n' "$(basename "$match")"
    done
done
printf 'MiSans faces copied to %s\n' "$DESTINATION"
