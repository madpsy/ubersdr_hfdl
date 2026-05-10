#!/usr/bin/env bash
# docker.sh — build the hfdl_launcher Docker image
#
# All binaries (ubersdr_iq, hfdl_launcher, dumphfdl, libacars) are built
# from source inside the Docker image.  No host binaries are required.
#
# Usage:
#   ./docker.sh [build|push|run|arm64]
#
#   build  — build the image for linux/amd64 locally (default)
#   arm64  — build the image for linux/arm64 locally
#   push   — build both linux/amd64 AND linux/arm64 via buildx and push a
#             multi-arch manifest to the registry, then commit & push git
#   run    — run the image (set env vars below)
#
# Environment variables (build):
#   IMAGE              Docker image name/tag        (default: madpsy/ubersdr_hfdl:latest)
#   PLATFORM           Docker --platform flag       (default: linux/amd64)
#   DUMPHFDL_VERSION   dumphfdl git branch/tag      (default: master)
#   LIBACARS_VERSION   libacars release version     (default: 2.2.1)

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

IMAGE="${IMAGE:-madpsy/ubersdr_hfdl:latest}"
PLATFORM="${PLATFORM:-linux/amd64}"
DUMPHFDL_VERSION="${DUMPHFDL_VERSION:-master}"
LIBACARS_VERSION="${LIBACARS_VERSION:-2.2.1}"

# Name of the buildx builder used for multi-arch builds
BUILDER_NAME="ubersdr_hfdl_builder"

# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------

die() { echo "error: $*" >&2; exit 1; }

check_deps() {
    command -v docker >/dev/null || die "docker not found in PATH"
}

# Ensure a buildx builder that supports multi-arch exists and is active.
ensure_builder() {
    if ! docker buildx inspect "$BUILDER_NAME" &>/dev/null; then
        echo "Creating buildx builder '$BUILDER_NAME'..."
        docker buildx create --name "$BUILDER_NAME" --driver docker-container --bootstrap
    fi
    docker buildx use "$BUILDER_NAME"
}

# Stage the build context into a temp directory (strips host binaries / .git).
stage_context() {
    TMPCTX="$(mktemp -d)"
    trap 'rm -rf "$TMPCTX"' EXIT
    echo "Staging build context in $TMPCTX..."
    rsync -a --exclude='/ubersdr_iq' --exclude='/hfdl_launcher' \
              --exclude='.git' \
              "$SCRIPT_DIR/" "$TMPCTX/"
}

# ---------------------------------------------------------------------------
# build — single-platform local load (amd64 by default, arm64 via arm64 cmd)
# ---------------------------------------------------------------------------

build() {
    check_deps
    stage_context

    echo "Building image $IMAGE (platform=$PLATFORM)..."
    ensure_builder
    docker buildx build \
        --platform "$PLATFORM" \
        --tag "$IMAGE" \
        --build-arg "DUMPHFDL_VERSION=${DUMPHFDL_VERSION}" \
        --build-arg "LIBACARS_VERSION=${LIBACARS_VERSION}" \
        --load \
        "$TMPCTX"

    echo "Built: $IMAGE"
}

# ---------------------------------------------------------------------------
# push — multi-arch build (amd64 + arm64) pushed directly to registry
# ---------------------------------------------------------------------------

push() {
    check_deps
    stage_context
    ensure_builder

    echo "Building and pushing multi-arch image $IMAGE (linux/amd64,linux/arm64)..."
    docker buildx build \
        --platform linux/amd64,linux/arm64 \
        --tag "$IMAGE" \
        --build-arg "DUMPHFDL_VERSION=${DUMPHFDL_VERSION}" \
        --build-arg "LIBACARS_VERSION=${LIBACARS_VERSION}" \
        --push \
        "$TMPCTX"

    echo "Pushed multi-arch manifest: $IMAGE"

    echo "Committing and pushing git repository..."
    git add -A
    git diff --cached --quiet || git commit -m "Release $IMAGE"
    git push
}

run_image() {
    # Build hfdl_launcher argument list from env vars
    # UBERSDR_URL defaults to http://ubersdr:8080 in the binary if not set
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
#   UBERSDR_URL    UberSDR base URL (default: http://ubersdr:8080)
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
    arm64) PLATFORM=linux/arm64 build ;;
    push)  push  ;;
    run)   shift; run_image "$@" ;;
    *)
        echo "Usage: $0 [build|arm64|push|run [hfdl_launcher-args...]]" >&2
        exit 1
        ;;
esac
