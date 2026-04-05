/* -----------------------------------------------------------------------
   UberSDR HFDL — Propagation tab
   Renders the aircraft→GS propagation matrix derived from
   HFNPDU Frequency Data (type 213) messages.
   Data source: GET /propagation
   ----------------------------------------------------------------------- */

'use strict';

// Cache of the last /propagation response.
let _propLastData = null;

// ---- Filter state ----------------------------------------------------------
let propFilterTerm = '';

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

  // Group paths by aircraft key
  const byACMap = {}; // acKey → { gsId → PropPath }
  for (const p of paths) {
    if (!byACMap[p.aircraft_key]) byACMap[p.aircraft_key] = {};
    byACMap[p.aircraft_key][p.gs_id] = p;
  }

  // Apply filter — keep only aircraft keys that match
  let acKeys = Object.keys(byACMap).sort();
  const totalAC = acKeys.length;
  if (propFilterTerm) {
    acKeys = acKeys.filter(key => {
      const anyPath = Object.values(byACMap[key])[0];
      return key.toLowerCase().includes(propFilterTerm) ||
             (anyPath.reg    || '').toLowerCase().includes(propFilterTerm) ||
             (anyPath.flight || '').toLowerCase().includes(propFilterTerm) ||
             (anyPath.icao   || '').toLowerCase().includes(propFilterTerm);
    });
  }

  // Update count label
  const countEl = document.getElementById('prop-count-label');
  if (countEl) countEl.textContent = propFilterTerm ? `${acKeys.length} / ${totalAC}` : `${totalAC}`;

  // Collect GS IDs only for the filtered aircraft
  const visibleGSIds = new Set();
  for (const key of acKeys) {
    for (const gsId of Object.keys(byACMap[key])) {
      visibleGSIds.add(parseInt(gsId, 10));
    }
  }
  const allGSIds = [...visibleGSIds].sort((a, b) => a - b);

  // Summary
  const gsCount = Object.keys(byGS).length;
  const acCount = Object.keys(byAC).length;

  let html = `<div class="prop-summary">
    <span class="prop-summary__stat">${paths.length} propagation path${paths.length !== 1 ? 's' : ''}</span>
    <span class="prop-summary__stat">${acCount} aircraft reporting</span>
    <span class="prop-summary__stat">${gsCount} ground stations heard</span>
    <span class="prop-summary__updated">Updated: ${new Date().toUTCString().replace('GMT','UTC')}</span>
  </div>`;

  if (acKeys.length === 0) {
    html += '<p class="empty" style="padding:20px">No aircraft match the filter…</p>';
    container.innerHTML = html;
    return;
  }

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
  fetch(BASE_PATH + '/propagation')
    .then(r => r.json())
    .then(snap => renderPropagationTab(snap))
    .catch(err => console.warn('propagation tab fetch error:', err));
}

// ---- Filter init -----------------------------------------------------------

function initPropagationFilter() {
  const filterEl = document.getElementById('prop-filter');
  const clearEl  = document.getElementById('prop-filter-clear');
  if (!filterEl) return;
  filterEl.addEventListener('input', () => {
    propFilterTerm = filterEl.value.trim().toLowerCase();
    if (_propLastData) renderPropagationTab(_propLastData);
  });
  clearEl.addEventListener('click', () => {
    filterEl.value = '';
    propFilterTerm = '';
    if (_propLastData) renderPropagationTab(_propLastData);
  });
}

// ---- Utility ---------------------------------------------------------------

function escProp(str) {
  if (!str) return '';
  return str.replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;');
}
