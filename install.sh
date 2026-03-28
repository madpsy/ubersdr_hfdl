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

REPO_RAW="https://raw.githubusercontent.com/madpsy/ubersdr_hfdl/master"
FREQ_JSONL_URL="https://ubersdr.org/hfdl/hfdl_frequencies.jsonl"
INSTALL_DIR="${HOME}/ubersdr/hfdl"
COMPOSE_FILE="docker-compose.yml"
FREQ_FILE="hfdl_frequencies.jsonl"
CONTAINER_FREQ_PATH="/data/hfdl_frequencies.jsonl"
FORCE_UPDATE="${FORCE_UPDATE:-0}"
CONFIG_PASS_FILE=".config_pass"

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
# Generate or load the config password
# ---------------------------------------------------------------------------

if [[ -f "${CONFIG_PASS_FILE}" ]]; then
    CONFIG_PASS="$(cat "${CONFIG_PASS_FILE}")"
    PASS_IS_NEW=0
else
    # Generate a strong 32-character alphanumeric password using /dev/urandom
    CONFIG_PASS="$(LC_ALL=C tr -dc 'A-Za-z0-9' </dev/urandom | head -c 16)"
    echo "${CONFIG_PASS}" > "${CONFIG_PASS_FILE}"
    chmod 600 "${CONFIG_PASS_FILE}"
    PASS_IS_NEW=1
fi

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
# Fetch helper scripts
# ---------------------------------------------------------------------------

for script in update.sh start.sh stop.sh restart.sh; do
    echo "Fetching ${script}..."
    curl -fsSL "${REPO_RAW}/${script}" -o "${script}"
    chmod +x "${script}"
    echo "Saved ${script}"
done

# ---------------------------------------------------------------------------
# Fetch HFDL frequency list
# ---------------------------------------------------------------------------

if [[ -f "${FREQ_FILE}" ]]; then
    echo "${FREQ_FILE} already exists — skipping download"
else
    echo "Fetching HFDL frequency list..."
    curl -fsSL "${FREQ_JSONL_URL}" -o "${FREQ_FILE}"
    echo "Saved ${FREQ_FILE}"
fi
# Ensure the frequency file is writable by the container's hfdl user (which
# runs as a system UID that differs from the host user who downloaded the file).
chmod 666 "${FREQ_FILE}"

IQ_RECORDINGS_DIR="iq_recordings"
IQ_CONTAINER_PATH="/iq_recordings"

# ---------------------------------------------------------------------------
# Patch compose file: add bind mounts and FREQ_URL if not already present
# ---------------------------------------------------------------------------

if ! grep -q "hfdl_frequencies.jsonl" "${COMPOSE_FILE}"; then
    # Inject volumes block before the first 'environment:' line, including both
    # the frequency file mount and the IQ recordings directory mount.
    sed -i "s|    environment:|    volumes:\n      - ./${FREQ_FILE}:${CONTAINER_FREQ_PATH}\n      - ./${IQ_RECORDINGS_DIR}:${IQ_CONTAINER_PATH}\n    environment:|" "${COMPOSE_FILE}"
    # Set FREQ_URL env var (replace the commented-out placeholder if present, else append)
    if grep -q "# FREQ_URL:" "${COMPOSE_FILE}"; then
        sed -i "s|# FREQ_URL:.*|FREQ_URL: \"file://${CONTAINER_FREQ_PATH}\"|" "${COMPOSE_FILE}"
    else
        sed -i "s|      EXTRA_ARGS:|      FREQ_URL: \"file://${CONTAINER_FREQ_PATH}\"\n      EXTRA_ARGS:|" "${COMPOSE_FILE}"
    fi
    echo "Patched ${COMPOSE_FILE} with frequency file mount, IQ recordings mount, and FREQ_URL"
fi

# ---------------------------------------------------------------------------
# Inject CONFIG_PASS into compose file
# ---------------------------------------------------------------------------

if grep -q "# CONFIG_PASS:" "${COMPOSE_FILE}"; then
    # Replace the commented-out placeholder with the actual password
    sed -i "s|# CONFIG_PASS:.*|CONFIG_PASS: \"${CONFIG_PASS}\"|" "${COMPOSE_FILE}"
elif grep -q "CONFIG_PASS:" "${COMPOSE_FILE}"; then
    # Already set (e.g. re-run with --force-update) — update the value in place
    sed -i "s|CONFIG_PASS:.*|CONFIG_PASS: \"${CONFIG_PASS}\"|" "${COMPOSE_FILE}"
else
    # Fallback: append before EXTRA_ARGS
    sed -i "s|      EXTRA_ARGS:|      CONFIG_PASS: \"${CONFIG_PASS}\"\n      EXTRA_ARGS:|" "${COMPOSE_FILE}"
fi
echo "CONFIG_PASS set in ${COMPOSE_FILE}"

# ---------------------------------------------------------------------------
# Create IQ recordings directory on the host
# ---------------------------------------------------------------------------

# The container runs as a non-root 'hfdl' user whose UID differs from the host
# user.  chmod 777 ensures the container can write WAV files into the bind-mount
# regardless of UID mapping.
mkdir -p "${INSTALL_DIR}/${IQ_RECORDINGS_DIR}"
chmod 777 "${INSTALL_DIR}/${IQ_RECORDINGS_DIR}"
echo "IQ recordings directory ready: ${INSTALL_DIR}/${IQ_RECORDINGS_DIR}"

# ---------------------------------------------------------------------------
# Pull image and start service
# ---------------------------------------------------------------------------

echo "Pulling latest Docker image..."
docker compose pull

echo "Starting / restarting hfdl_launcher..."
docker compose up -d --remove-orphans --force-recreate

echo ""
echo "Done. hfdl_launcher is running."
echo "  View logs  : docker compose logs -f  (or ./update.sh)"
echo "  Stop       : ./stop.sh"
echo "  Start      : ./start.sh"
echo "  Restart    : ./restart.sh"
echo "  Update     : ./update.sh"
echo ""
if [[ "${PASS_IS_NEW}" == "1" ]]; then
    echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
    echo "  CONFIG PASSWORD (auto-generated)"
    echo ""
    echo "  ${CONFIG_PASS}"
    echo ""
    echo "  This password protects the frequency Apply endpoints in the web UI."
    echo "  It has been saved to: ${INSTALL_DIR}/${CONFIG_PASS_FILE}"
    echo ""
    echo "  To change it, edit CONFIG_PASS in ${INSTALL_DIR}/${COMPOSE_FILE}"
    echo "  and run ./restart.sh  (also update ${CONFIG_PASS_FILE} to match)."
    echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
else
    echo "  Config password loaded from ${INSTALL_DIR}/${CONFIG_PASS_FILE}"
fi
