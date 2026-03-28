#!/usr/bin/env bash
# update.sh — re-run the installer to pull the latest image and config
#
# Usage:
#   ./update.sh

curl -fsSL https://raw.githubusercontent.com/madpsy/ubersdr_hfdl/master/install.sh | bash
