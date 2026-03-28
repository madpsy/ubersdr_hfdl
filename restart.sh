#!/usr/bin/env bash
# restart.sh — restart the hfdl_launcher service
#
# Usage:
#   ./restart.sh

set -euo pipefail

INSTALL_DIR="${HOME}/ubersdr/hfdl"

cd "${INSTALL_DIR}"
echo "Stopping hfdl_launcher..."
docker compose down
echo "Starting hfdl_launcher..."
docker compose up -d --remove-orphans
echo "Done."
echo "  View logs : docker compose logs -f"
