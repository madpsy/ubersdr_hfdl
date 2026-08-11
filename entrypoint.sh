#!/bin/sh
# entrypoint.sh — translate environment variables into hfdl_launcher flags
#
# Environment variables:
#   UBERSDR_URL         UberSDR base URL (default: http://ubersdr:8080)
#   PASS                Bypass password
#   STATION             Comma-separated station IDs           e.g. 1,2,3
#   SYSTEM_TABLE        Path to system table file
#   FREQ_URL            Custom HFDL frequency list URL
#   DRY_RUN             Set to 1 for dry-run mode
#   WEB_PORT            Port for the web statistics server    (default: 6090, 0 = disabled)
#   WEB_STATIC          Path to static web files directory
#                       (default: /usr/local/share/hfdl_launcher/static)
#   EXTRA_ARGS          Extra dumphfdl args passed after --
#                       e.g. "--output decoded:json:tcp:address=host,port=5555"
#   IQ_RECORD_DIR       Directory inside the container to write IQ WAV recordings.
#                       Mount a host directory here to persist files on the host.
#                       Recording is disabled when this variable is unset.
#                       e.g. /iq_recordings
#   IQ_RECORD_SECONDS   Duration of each IQ recording in seconds (default: 30)
#
#   SELCAL_FREQS        HF voice channels to monitor for SELCAL / SELCAL32.
#                       Comma-separated  kHz[:label[:audio]]
#                       e.g. "5598:NAT-A,8906:NAT-A,8864:NAT-B"
#                       A third field of "audio" relays the channel to
#                       listeners without running the SELCAL decoder on it.
#                       Unset = feature disabled (no receiver sessions used).
#   SELCAL_AUDIO        Set to 0 to decode only and not relay audio to browsers
#   SELCAL_MAX_LISTENERS  Max simultaneous listeners per channel (default: 10)
#   SELCAL_RECORD_DIR   Directory to write a WAV of each SELCAL detection.
#                       Recording is disabled when unset.
#   SELCAL_RECORD_SECONDS  Audio window saved per detection (default: 8)
#
# Note: --output decoded:json:file:path=- is always injected automatically by
# hfdl_launcher for the internal web statistics server.  Do not add it yourself.
#
# Note: IQ mode (bandwidth) is chosen automatically per window by hfdl_launcher.
# iq48 / iq96 / iq192 are selected based on how tightly channels are clustered.

set -e

args=""
[ -n "$UBERSDR_URL"        ] && args="$args -url $UBERSDR_URL"
[ -n "$PASS"               ] && args="$args -pass $PASS"
[ -n "$STATION"            ] && args="$args -station $STATION"
[ -n "$SYSTEM_TABLE"       ] && args="$args -system-table $SYSTEM_TABLE"
[ -n "$FREQ_URL"           ] && args="$args -freq-url $FREQ_URL"
[ -n "$CONFIG_PASS"        ] && args="$args -config-pass $CONFIG_PASS"
[ -n "$WEB_PORT"           ] && args="$args -web-port $WEB_PORT"
[ -n "$WEB_STATIC"         ] && args="$args -web-static $WEB_STATIC"
[ -n "$IQ_RECORD_DIR"      ] && args="$args -iq-record-dir $IQ_RECORD_DIR"
[ -n "$IQ_RECORD_SECONDS"  ] && args="$args -iq-record-seconds $IQ_RECORD_SECONDS"
[ "$DRY_RUN" = "1"         ] && args="$args -dry-run"

[ -n "$SELCAL_MAX_LISTENERS"   ] && args="$args -selcal-max-listeners $SELCAL_MAX_LISTENERS"
[ -n "$SELCAL_RECORD_DIR"      ] && args="$args -selcal-record-dir $SELCAL_RECORD_DIR"
[ -n "$SELCAL_RECORD_SECONDS"  ] && args="$args -selcal-record-seconds $SELCAL_RECORD_SECONDS"
[ "$SELCAL_AUDIO" = "0"        ] && args="$args -selcal-audio=false"

# SELCAL_FREQS may contain spaces (channel labels such as "Shannon Volmet"),
# so it is prepended to the positional parameters rather than to $args, which
# is deliberately expanded unquoted.
if [ -n "$SELCAL_FREQS" ]; then
    set -- -selcal-freqs "$SELCAL_FREQS" "$@"
fi

# Append any CLI args passed to the container, then EXTRA_ARGS after --
if [ -n "$EXTRA_ARGS" ]; then
    # shellcheck disable=SC2086
    exec /usr/local/bin/hfdl_launcher $args "$@" -- $EXTRA_ARGS
else
    # shellcheck disable=SC2086
    exec /usr/local/bin/hfdl_launcher $args "$@"
fi
