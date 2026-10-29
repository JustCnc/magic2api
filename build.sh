#!/usr/bin/env bash
# Build the plugin as a C shared library for the current or a cross target.
#
#   ./build.sh                  # windows/amd64 (the local default)
#   ./build.sh linux/amd64      # needs x86_64-linux-gnu-gcc
#   ./build.sh all              # every target the toolchain supports
#
# Output: dist/<goos>/<goarch>/magic2api.{dll,so,dylib}
set -euo pipefail

cd "$(dirname "$0")"

NAME=magic2api
OUT_DIR=${OUT_DIR:-$(pwd)/dist}
TARGETS=${*:-windows/amd64}

build_target() {
  local target="$1"
  local goos="${target%%/*}"
  local goarch="${target##*/}"
  local ext cc
  case "$goos" in
    windows) ext="dll"; cc=${CC:-x86_64-w64-mingw32-gcc} ;;
    linux)   ext="so";  cc=${CC:-gcc} ;;
    darwin)  ext="dylib"; cc=${CC:-clang} ;;
    *) echo "unsupported target: $target" >&2; return 1 ;;
  esac
  if ! command -v "$cc" >/dev/null 2>&1; then
    echo "missing C compiler for $target ($cc)" >&2
    return 1
  fi
  echo "==> $target"
  mkdir -p "$OUT_DIR/$goos/$goarch"
  CGO_ENABLED=1 GOOS="$goos" GOARCH="$goarch" CC="$cc" \
    go build -buildmode=c-shared -trimpath -ldflags="-s -w" \
    -o "$OUT_DIR/$goos/$goarch/$NAME.$ext" .
  rm -f "$OUT_DIR/$goos/$goarch/$NAME.h"
}

if [[ "$TARGETS" == "all" ]]; then
  TARGETS="windows/amd64 linux/amd64"
fi

for target in $TARGETS; do
  build_target "$target"
done

echo
echo "artifacts:"
find "$OUT_DIR" -type f \( -name "*.so" -o -name "*.dll" -o -name "*.dylib" \) -exec ls -lh {} \;
