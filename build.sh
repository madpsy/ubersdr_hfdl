#!/usr/bin/env bash
# build.sh — build ubersdr_iq and hfdl_launcher
#
# Usage:
#   ./build.sh              # build both binaries in this directory
#   ./build.sh install      # build and install to /usr/local/bin (requires sudo)
#   ./build.sh clean        # remove built binaries

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "$SCRIPT_DIR"

UBERSDR_BIN="ubersdr_iq"
LAUNCHER_BIN="hfdl_launcher"
INSTALL_DIR="${INSTALL_DIR:-/usr/local/bin}"

build() {
    echo "Building $UBERSDR_BIN..."
    go build -o "$UBERSDR_BIN" .

    echo "Building $LAUNCHER_BIN..."
    go build -o "$LAUNCHER_BIN" ./cmd/hfdl_launcher/

    echo "Done: $SCRIPT_DIR/$UBERSDR_BIN  $SCRIPT_DIR/$LAUNCHER_BIN"
}

install() {
    build
    echo "Installing to $INSTALL_DIR..."
    sudo cp "$UBERSDR_BIN" "$LAUNCHER_BIN" "$INSTALL_DIR/"
    echo "Installed $INSTALL_DIR/$UBERSDR_BIN and $INSTALL_DIR/$LAUNCHER_BIN"
}

clean() {
    echo "Removing built binaries..."
    rm -f "$UBERSDR_BIN" "$LAUNCHER_BIN"
    echo "Done"
}

case "${1:-build}" in
    build)   build   ;;
    install) install ;;
    clean)   clean   ;;
    *)
        echo "Usage: $0 [build|install|clean]" >&2
        exit 1
        ;;
esac
