#!/usr/bin/env bash
# start.sh — start the hfdl_launcher service
#
# Usage:
#   ./start.sh

set -euo pipefail

INSTALL_DIR="${HOME}/ubersdr/hfdl"

cd "${INSTALL_DIR}"
echo "Starting hfdl_launcher..."
docker compose up -d --remove-orphans
echo "Done."
echo "  View logs : docker compose logs -f"
