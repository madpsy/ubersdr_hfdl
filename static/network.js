/* -----------------------------------------------------------------------
   UberSDR HFDL — Network tab
   Renders the live HFDL ground station network topology derived from
   SPDU gs_status[] beacons.  Data source: GET /network
   ----------------------------------------------------------------------- */

'use strict';

// All known frequencies from the frequency list, keyed by kHz.
// Populated by loadNetworkTab() from /groundstations so we can show
// the full slot grid even for GS not yet seen in SPDUs.
let _netAllFreqsByGS = {}; // gs_id → [{ freq_khz, timeslot }]

// Cache of the last /network response for periodic refresh.
let _netLastData = null;

// ---- Rendering -------------------------------------------------------------

function renderNetworkTab(networkData, gsData) {
  const container = document.getElementById('network-grid');
  if (!container) return;

  // Build a lookup of all configured frequencies per GS from /groundstations
  if (Array.isArray(gsData)) {
    _netAllFreqsByGS = {};
    for (const gs of gsData) {
      _netAllFreqsByGS[gs.gs_id] = gs.frequencies || [];
    }
  }

  // Build a lookup of SPDU-advertised active freqs/slots per GS
  const activeKHzByGS  = {}; // gs_id → Set<freq_khz>  (only when system table loaded)
  const activeSlotByGS = {}; // gs_id → Set<slot_id>   (always available)
  const stateByGS      = {}; // gs_id → NetworkGSState
  if (Array.isArray(networkData)) {
    for (const n of networkData) {
      stateByGS[n.gs_id] = n;
      activeKHzByGS[n.gs_id]  = new Set(n.active_freqs_khz || []);
      activeSlotByGS[n.gs_id] = new Set(n.active_slot_ids  || []);
    }
  }

  // Collect all GS IDs (union of configured + seen in SPDUs)
  const allGSIds = new Set([
    ...Object.keys(_netAllFreqsByGS).map(Number),
    ...Object.keys(stateByGS).map(Number),
  ]);
  const sortedIds = [...allGSIds].sort((a, b) => a - b);

  if (sortedIds.length === 0) {
    container.innerHTML = '<p class="empty" style="padding:20px">No network data yet — waiting for SPDU beacons…</p>';
    return;
  }

  // Summary bar — only count UTC-synced GS that are currently active (SPDU seen
  // within the last 10 min), since stale utc_sync values are not meaningful.
  const totalGS   = sortedIds.length;
  const activeGS  = sortedIds.filter(id => stateByGS[id]?.spdu_active).length;
  const syncedGS  = sortedIds.filter(id => stateByGS[id]?.spdu_active && stateByGS[id]?.utc_sync).length;

  let html = `<div class="net-summary">
    <span class="net-summary__stat net-summary__stat--active">${activeGS} / ${totalGS} GS active (SPDU)</span>
    <span class="net-summary__stat">${syncedGS} UTC-synced</span>
    <span class="net-summary__updated">Updated: ${new Date().toUTCString().replace('GMT','UTC')}</span>
  </div>`;

  // Grid of GS cards
  html += '<div class="net-gs-grid">';
  for (const gsId of sortedIds) {
    const state    = stateByGS[gsId];
    const active   = state?.spdu_active ?? false;
    const utcSync  = state?.utc_sync ?? false;
    const lastSeen = state?.spdu_last_seen ?? 0;
    const loc      = state?.location || (typeof gsNames !== 'undefined' && gsNames[gsId]) || `GS ${gsId}`;
    const configFreqs = _netAllFreqsByGS[gsId] || [];

    // Status badges
    const activeBadge = active
      ? `<span class="net-badge net-badge--active">● SPDU active</span>`
      : `<span class="net-badge net-badge--inactive">○ Not seen</span>`;
    // UTC sync badge: only show definitive ✓/✗ when the GS is currently active.
    // When inactive (stale), show a neutral "unknown" badge so the user knows
    // the value may not reflect current state.
    let syncBadge = '';
    if (state) {
      if (active) {
        syncBadge = utcSync
          ? `<span class="net-badge net-badge--sync">✓ UTC sync</span>`
          : `<span class="net-badge net-badge--nosync">✗ No UTC sync</span>`;
      } else {
        syncBadge = `<span class="net-badge net-badge--stale">⊘ UTC sync unknown</span>`;
      }
    }
    // Last-seen: show as relative age ("3h ago") so staleness is immediately obvious.
    const lastSeenStr = lastSeen
      ? `<span class="net-last-seen">Last SPDU: ${netRelativeAge(lastSeen)}</span>`
      : '';

    // Frequency chips — only highlight green when the GS is currently active.
    // If the GS is inactive (stale SPDU data), render all chips grey to avoid
    // implying those frequencies are currently in use.
    const slotSet = active ? (activeSlotByGS[gsId] || new Set()) : new Set();
    const kHzSet  = active ? (activeKHzByGS[gsId]  || new Set()) : new Set();
    let freqChips = '';
    if (configFreqs.length > 0) {
      freqChips = configFreqs.map(f => {
        // A frequency is active if its timeslot matches a SPDU slot ID,
        // OR if its kHz value is in the resolved active freq list.
        const isActive = slotSet.has(f.timeslot) || kHzSet.has(f.freq_khz);
        const cls = isActive ? 'net-freq net-freq--active' : 'net-freq';
        return `<span class="${cls}">${f.freq_khz.toLocaleString()}<span class="net-slot">T${f.timeslot}</span></span>`;
      }).join('');
    } else if (active && state && state.active_freqs_khz && state.active_freqs_khz.length > 0) {
      // GS seen in SPDU but not in our frequency list — show what SPDU advertises.
      // Only highlight when active; if stale, fall through to "no frequency data".
      freqChips = state.active_freqs_khz.map(khz =>
        `<span class="net-freq net-freq--active">${khz.toLocaleString()}</span>`
      ).join('');
    } else {
      freqChips = '<span class="net-no-freqs">No frequency data</span>';
    }

    html += `<div class="net-gs-card${active ? ' net-gs-card--active' : ''}">
      <div class="net-gs-card__header">
        <span class="net-gs-id">GS ${gsId}</span>
        <span class="net-gs-loc">${escNet(loc)}</span>
      </div>
      <div class="net-gs-card__badges">${activeBadge}${syncBadge}</div>
      ${lastSeenStr}
      <div class="net-gs-card__freqs">${freqChips}</div>
    </div>`;
  }
  html += '</div>';

  container.innerHTML = html;
}

// ---- Data loading ----------------------------------------------------------

function loadNetworkTab() {
  Promise.all([
    fetch(BASE_PATH + '/network').then(r => r.json()),
    fetch(BASE_PATH + '/groundstations').then(r => r.json()),
  ])
    .then(([netData, gsData]) => {
      _netLastData = netData;
      renderNetworkTab(netData, gsData);
    })
    .catch(err => console.warn('network tab fetch error:', err));
}

// ---- Utility ---------------------------------------------------------------

function escNet(str) {
  if (!str) return '';
  return str.replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;');
}

// netRelativeAge returns a human-readable relative age string for a unix
// timestamp, e.g. "just now", "4m ago", "2h ago", "3d ago".
function netRelativeAge(unixSec) {
  const diffSec = Math.floor(Date.now() / 1000) - unixSec;
  if (diffSec < 60)  return 'just now';
  if (diffSec < 3600) return `${Math.floor(diffSec / 60)}m ago`;
  if (diffSec < 86400) return `${Math.floor(diffSec / 3600)}h ago`;
  return `${Math.floor(diffSec / 86400)}d ago`;
}
