#!/usr/bin/env bash
# docker.sh — build the hfdl_launcher Docker image
#
# All binaries (ubersdr_iq, hfdl_launcher, dumphfdl, libacars) are built
# from source inside the Docker image.  No host binaries are required.
#
# Usage:
#   ./docker.sh [build|push|run]
#
#   build  — build the image (default)
#   push   — build then push to registry (set IMAGE env var)
#   run    — run the image (set env vars below)
#
# Environment variables (build):
#   IMAGE              Docker image name/tag        (default: hfdl_launcher:latest)
#   PLATFORM           Docker --platform flag       (default: linux/amd64)
#   DUMPHFDL_VERSION   dumphfdl git branch/tag      (default: master)
#   LIBACARS_VERSION   libacars release version     (default: 2.2.1)

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

IMAGE="${IMAGE:-madpsy/ubersdr_hfdl:latest}"
PLATFORM="${PLATFORM:-linux/amd64}"
DUMPHFDL_VERSION="${DUMPHFDL_VERSION:-master}"
LIBACARS_VERSION="${LIBACARS_VERSION:-2.2.1}"

# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------

die() { echo "error: $*" >&2; exit 1; }

check_deps() {
    command -v docker >/dev/null || die "docker not found in PATH"
}

build() {
    check_deps

    # Create a temporary build context from the source tree only
    TMPCTX="$(mktemp -d)"
    trap 'rm -rf "$TMPCTX"' EXIT

    echo "Staging build context in $TMPCTX..."

    # Copy source tree (excluding built binaries at the root only)
    rsync -a --exclude='/ubersdr_iq' --exclude='/hfdl_launcher' \
              --exclude='.git' \
              "$SCRIPT_DIR/" "$TMPCTX/"

    echo "Building image $IMAGE (platform=$PLATFORM)..."
    docker build \
        --platform "$PLATFORM" \
        --tag "$IMAGE" \
        --build-arg "DUMPHFDL_VERSION=${DUMPHFDL_VERSION}" \
        --build-arg "LIBACARS_VERSION=${LIBACARS_VERSION}" \
        "$TMPCTX"

    echo "Built: $IMAGE"
}

push() {
    build
    echo "Pushing $IMAGE..."
    docker push "$IMAGE"
}

run_image() {
    # Build hfdl_launcher argument list from env vars
    # UBERSDR_URL defaults to http://172.20.0.1:8080 in the binary if not set
    # IQ mode (bandwidth) is chosen automatically — no flag needed.
    args=()
    [[ -n "${UBERSDR_URL:-}" ]] && args+=(-url "$UBERSDR_URL")

    [[ -n "${PASS:-}"         ]] && args+=(-pass        "$PASS")
    [[ -n "${STATION:-}"      ]] && args+=(-station     "$STATION")
    [[ -n "${SYSTEM_TABLE:-}" ]] && args+=(-system-table "$SYSTEM_TABLE")
    [[ -n "${FREQ_URL:-}"     ]] && args+=(-freq-url    "$FREQ_URL")
    [[ "${SILENT:-}"  == "1"  ]] && args+=(-silent)
    [[ "${DRY_RUN:-}" == "1"  ]] && args+=(-dry-run)

    # EXTRA_ARGS is passed after -- to dumphfdl (space-separated)
    if [[ -n "${EXTRA_ARGS:-}" ]]; then
        args+=(--)
        # word-split intentional here
        # shellcheck disable=SC2086
        args+=($EXTRA_ARGS)
    fi

    # Any positional args to docker.sh run are appended verbatim
    args+=("${@}")

    docker run --rm -it \
        --platform "$PLATFORM" \
        "$IMAGE" \
        "${args[@]}"
}

# ---------------------------------------------------------------------------
# Environment variable reference (for docker run -e ...)
# ---------------------------------------------------------------------------
#
#   UBERSDR_URL    UberSDR base URL (default: http://172.20.0.1:8080)
#   PASS           Bypass password
#   STATION        Comma-separated station IDs           e.g. 1,2,3
#   SYSTEM_TABLE   Path to system table inside container
#   FREQ_URL       Custom HFDL frequency list URL
#   SILENT         Set to 1 to discard decoded output
#   DRY_RUN        Set to 1 for dry-run mode
#   EXTRA_ARGS     Extra dumphfdl args (passed after --)
#                  e.g. "--output udp,address=host,port=5555 --output-format json"
#
# Note: IQ mode (bandwidth) is chosen automatically per window — iq48, iq96,
# or iq192 is selected based on how tightly channels are clustered.

# ---------------------------------------------------------------------------
# Main
# ---------------------------------------------------------------------------

case "${1:-build}" in
    build) build ;;
    push)  push  ;;
    run)   shift; run_image "$@" ;;
    *)
        echo "Usage: $0 [build|push|run [hfdl_launcher-args...]]" >&2
        exit 1
        ;;
esac
