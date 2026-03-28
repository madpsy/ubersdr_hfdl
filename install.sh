#!/usr/bin/env bash
# install.sh — fetch the docker-compose.yml from the ubersdr_hfdl repo and start the service
#
# Usage:
#   curl -fsSL https://raw.githubusercontent.com/madpsy/ubersdr_hfdl/master/install.sh | bash
#   — or —
#   ./install.sh

set -euo pipefail

REPO_RAW="https://raw.githubusercontent.com/madpsy/ubersdr_hfdl/master"
INSTALL_DIR="${HOME}/ubersdr/hfdl"
COMPOSE_FILE="docker-compose.yml"

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

echo "Fetching ${COMPOSE_FILE} from GitHub..."
curl -fsSL "${REPO_RAW}/${COMPOSE_FILE}" -o "${COMPOSE_FILE}"

echo "Saved ${COMPOSE_FILE}"

# ---------------------------------------------------------------------------
# Pull image and start service
# ---------------------------------------------------------------------------

echo "Pulling Docker image..."
docker compose pull

echo "Starting hfdl_launcher..."
docker compose up -d

echo ""
echo "Done. hfdl_launcher is running."
echo "  View logs : docker compose logs -f"
echo "  Stop      : docker compose down"
