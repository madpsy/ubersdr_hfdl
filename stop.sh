#!/usr/bin/env bash
# stop.sh — stop the hfdl_launcher service
#
# Usage:
#   ./stop.sh

set -euo pipefail

INSTALL_DIR="${HOME}/ubersdr/hfdl"

cd "${INSTALL_DIR}"
echo "Stopping hfdl_launcher..."
docker compose down
echo "Done."
