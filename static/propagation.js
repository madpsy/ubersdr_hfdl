/* -----------------------------------------------------------------------
   UberSDR HFDL — Propagation tab
   Renders the aircraft→GS propagation matrix derived from
   HFNPDU Frequency Data (type 213) messages.
   Data source: GET /propagation
   ----------------------------------------------------------------------- */

'use strict';

// Cache of the last /propagation response.
let _propLastData = null;

// ---- Rendering -------------------------------------------------------------

function renderPropagationTab(snap) {
  const container = document.getElementById('propagation-grid');
  if (!container) return;
  _propLastData = snap;

  if (!snap || !snap.paths || snap.paths.length === 0) {
    container.innerHTML = '<p class="empty" style="padding:20px">No propagation data yet — waiting for Frequency Data messages (HFNPDU type 213)…</p>';
    return;
  }

  const paths = snap.paths;
  const byGS  = snap.by_gs  || {};
  const byAC  = snap.by_aircraft || {};

  // Summary
  const gsCount = Object.keys(byGS).length;
  const acCount = Object.keys(byAC).length;

  let html = `<div class="prop-summary">
    <span class="prop-summary__stat">${paths.length} propagation path${paths.length !== 1 ? 's' : ''}</span>
    <span class="prop-summary__stat">${acCount} aircraft reporting</span>
    <span class="prop-summary__stat">${gsCount} ground stations heard</span>
    <span class="prop-summary__updated">Updated: ${new Date().toUTCString().replace('GMT','UTC')}</span>
  </div>`;

  // Matrix: rows = aircraft, columns = GS IDs they can hear
  // Collect all GS IDs seen
  const allGSIds = [...new Set(paths.map(p => p.gs_id))].sort((a, b) => a - b);

  // Group paths by aircraft key
  const byACMap = {}; // acKey → { gsId → PropPath }
  for (const p of paths) {
    if (!byACMap[p.aircraft_key]) byACMap[p.aircraft_key] = {};
    byACMap[p.aircraft_key][p.gs_id] = p;
  }
  const acKeys = Object.keys(byACMap).sort();

  // Build matrix table
  html += `<div class="prop-matrix-wrap">`;
  html += `<table class="prop-matrix">`;
  html += `<thead><tr><th class="prop-ac-col">Aircraft</th>`;
  for (const gsId of allGSIds) {
    const loc = (typeof gsNames !== 'undefined' && gsNames[gsId]) || `GS ${gsId}`;
    html += `<th class="prop-gs-col" title="${escProp(loc)}">GS ${gsId}<div class="prop-gs-name">${escProp(loc.split(',')[0])}</div></th>`;
  }
  html += `</tr></thead><tbody>`;

  for (const acKey of acKeys) {
    const acPaths = byACMap[acKey];
    // Get identity from any path
    const anyPath = Object.values(acPaths)[0];
    const label = [anyPath.reg, anyPath.flight, anyPath.icao ? `(${anyPath.icao})` : '']
      .filter(Boolean).join(' ') || acKey;

    html += `<tr><td class="prop-ac-cell mono">${escProp(label)}</td>`;
    for (const gsId of allGSIds) {
      const p = acPaths[gsId];
      if (p) {
        const sig = p.sig_level ? p.sig_level.toFixed(1) : '?';
        const freq = p.freq_khz ? (p.freq_khz / 1000).toFixed(3) + ' MHz' : '';
        const cls = p.sig_level >= -30 ? 'prop-cell--strong'
                  : p.sig_level >= -45 ? 'prop-cell--ok'
                  : 'prop-cell--weak';
        html += `<td class="prop-cell ${cls}" title="${freq} · ${sig} dBFS · ${escProp(p.gs_location)}">✓<div class="prop-sig">${sig}</div></td>`;
      } else {
        html += `<td class="prop-cell prop-cell--none"></td>`;
      }
    }
    html += `</tr>`;
  }

  html += `</tbody></table></div>`;
  container.innerHTML = html;
}

// ---- Data loading ----------------------------------------------------------

function loadPropagationTab() {
  fetch('/propagation')
    .then(r => r.json())
    .then(snap => renderPropagationTab(snap))
    .catch(err => console.warn('propagation tab fetch error:', err));
}

// ---- Utility ---------------------------------------------------------------

function escProp(str) {
  if (!str) return '';
  return str.replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;');
}
