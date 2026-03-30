#!/usr/bin/env bash
# get-password.sh — display the current CONFIG_PASS for the ubersdr_hfdl web UI
#
# Usage:
#   ./get-password.sh

set -euo pipefail

INSTALL_DIR="${HOME}/ubersdr/hfdl"
CONFIG_PASS_FILE="${INSTALL_DIR}/.config_pass"

if [[ ! -f "${CONFIG_PASS_FILE}" ]]; then
    echo "error: password file not found at ${CONFIG_PASS_FILE}" >&2
    echo "       Has install.sh been run yet?" >&2
    exit 1
fi

CONFIG_PASS="$(cat "${CONFIG_PASS_FILE}")"

echo ""
echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
echo "  CONFIG PASSWORD"
echo ""
echo "  ${CONFIG_PASS}"
echo ""
echo "  This password protects the frequency Apply endpoints in the web UI."
echo "  Stored at: ${CONFIG_PASS_FILE}"
echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
echo ""
