#!/usr/bin/env bash
# install.sh — fetch the docker-compose.yml from the ubersdr_hfdl repo and start the service
#
# Usage:
#   curl -fsSL https://raw.githubusercontent.com/madpsy/ubersdr_hfdl/refs/heads/main/install.sh | bash
#   — or —
#   ./install.sh [--force-update]
#
# Options:
#   --force-update   Overwrite an existing docker-compose.yml (default: skip if present)
#
# When piping through bash, pass the flag via env var instead:
#   curl -fsSL ... | FORCE_UPDATE=1 bash

set -euo pipefail

REPO_RAW="https://raw.githubusercontent.com/madpsy/ubersdr_hfdl/refs/heads/main"
INSTALL_DIR="${HOME}/ubersdr/hfdl"
COMPOSE_FILE="docker-compose.yml"
FORCE_UPDATE="${FORCE_UPDATE:-0}"

# Parse flags when run directly (not piped)
for arg in "$@"; do
    case "$arg" in
        --force-update) FORCE_UPDATE=1 ;;
        *) echo "Unknown argument: $arg" >&2; exit 1 ;;
    esac
done

die() { echo "error: $*" >&2; exit 1; }

# ---------------------------------------------------------------------------
# Dependency checks
# ---------------------------------------------------------------------------

command -v docker >/dev/null || die "docker not found in PATH — please install Docker first"
docker compose version >/dev/null 2>&1 || die "docker compose plugin not found — please install Docker Compose v2"

# ---------------------------------------------------------------------------
# Prepare install directory
# ---------------------------------------------------------------------------

mkdir -p "${INSTALL_DIR}"
cd "${INSTALL_DIR}"

# ---------------------------------------------------------------------------
# Fetch compose file
# ---------------------------------------------------------------------------

if [[ -f "${COMPOSE_FILE}" && "${FORCE_UPDATE}" != "1" ]]; then
    echo "${COMPOSE_FILE} already exists — skipping download (use --force-update to overwrite)"
else
    echo "Fetching ${COMPOSE_FILE} from GitHub..."
    curl -fsSL "${REPO_RAW}/${COMPOSE_FILE}" -o "${COMPOSE_FILE}"
    echo "Saved ${COMPOSE_FILE}"
fi

# ---------------------------------------------------------------------------
# Pull image and start service
# ---------------------------------------------------------------------------

echo "Pulling latest Docker image..."
docker compose pull

echo "Starting / restarting hfdl_launcher..."
docker compose up -d --remove-orphans --force-recreate

echo ""
echo "Done. hfdl_launcher is running."
echo "  View logs : docker compose logs -f"
echo "  Stop      : docker compose down"
