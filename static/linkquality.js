/* -----------------------------------------------------------------------
   UberSDR HFDL — Link Quality tab
   Per-aircraft MPDU error rate table with inline sparkline charts.
   Data source: GET /aircraft (error_rate, mpdu_rx, mpdu_tx, mpdu_err fields)
   ----------------------------------------------------------------------- */

'use strict';

// Rolling history of error_rate samples per aircraft key.
// Map: acKey → [{ t, rate }, …] (newest first, max 20 entries)
const lqHistory = {};
const LQ_MAX_SAMPLES = 20;

// ---- Filter state ----------------------------------------------------------
let lqFilterTerm = '';

// ---- Called from app.js SSE position handler to update history ------------

function recordLQSample(ac) {
  if (ac.error_rate == null || (!ac.mpdu_rx && !ac.mpdu_tx)) return;
  const key = ac.key;
  if (!lqHistory[key]) lqHistory[key] = [];
  lqHistory[key].unshift({ t: ac.last_seen || Math.floor(Date.now() / 1000), rate: ac.error_rate });
  if (lqHistory[key].length > LQ_MAX_SAMPLES) lqHistory[key].length = LQ_MAX_SAMPLES;
}

// ---- Rendering -------------------------------------------------------------

function renderLinkQualityTab() {
  const container = document.getElementById('lq-content');
  if (!container) return;

  // Collect all aircraft that have PDU stats
  let list = Object.values(typeof aircraftStore !== 'undefined' ? aircraftStore : {})
    .filter(ac => ac.mpdu_rx || ac.mpdu_tx);

  const total = list.length;

  // Apply filter
  if (lqFilterTerm) {
    list = list.filter(ac =>
      (ac.icao   || '').toLowerCase().includes(lqFilterTerm) ||
      (ac.reg    || '').toLowerCase().includes(lqFilterTerm) ||
      (ac.flight || '').toLowerCase().includes(lqFilterTerm)
    );
  }

  list.sort((a, b) => (b.error_rate || 0) - (a.error_rate || 0)); // worst first

  // Update count label
  const countEl = document.getElementById('lq-count-label');
  if (countEl) countEl.textContent = lqFilterTerm ? `${list.length} / ${total}` : `${total}`;

  if (total === 0) {
    container.innerHTML = '<p class="empty" style="padding:20px">No link quality data yet — waiting for Performance Data messages (HFNPDU type 209)…</p>';
    return;
  }
  if (list.length === 0) {
    container.innerHTML = '<p class="empty" style="padding:20px">No aircraft match the filter…</p>';
    return;
  }

  let html = `<table class="lq-table">
    <thead>
      <tr>
        <th>ICAO</th>
        <th>Reg</th>
        <th>Flight</th>
        <th>GS</th>
        <th>Freq (kHz)</th>
        <th>MPDU Rx</th>
        <th>MPDU Tx</th>
        <th>Errors</th>
        <th>Error Rate</th>
        <th>Trend</th>
        <th>Last Seen</th>
      </tr>
    </thead>
    <tbody>`;

  for (const ac of list) {
    const gsName = ac.gs_id && (typeof gsNames !== 'undefined') && gsNames[ac.gs_id]
      ? `${gsNames[ac.gs_id]} (${ac.gs_id})`
      : (ac.gs_id ? `GS ${ac.gs_id}` : '—');
    const freq = ac.freq_khz ? ac.freq_khz.toLocaleString() : '—';
    const rate = ac.error_rate != null ? ac.error_rate.toFixed(1) + '%' : '—';
    const rateCls = ac.error_rate >= 20 ? 'err-high' : ac.error_rate >= 5 ? 'err-mid' : 'err-low';
    const sparkSvg = buildSparkline(lqHistory[ac.key] || []);
    const lqEsc = typeof esc === 'function' ? esc : s => s;
    const fmtDT = typeof fmtDateTime === 'function' ? fmtDateTime : () => '—';

    html += `<tr>
      <td class="mono">${lqEsc(ac.icao) || '—'}</td>
      <td class="mono">${lqEsc(ac.reg) || '—'}</td>
      <td class="mono">${lqEsc(ac.flight) || '—'}</td>
      <td>${lqEsc(gsName)}</td>
      <td class="mono">${freq}</td>
      <td class="mono">${ac.mpdu_rx || 0}</td>
      <td class="mono">${ac.mpdu_tx || 0}</td>
      <td class="mono">${ac.mpdu_err || 0}</td>
      <td class="mono ${rateCls}">${rate}</td>
      <td class="lq-spark">${sparkSvg}</td>
      <td class="dim">${fmtDT(ac.last_seen)}</td>
    </tr>`;
  }

  html += `</tbody></table>`;
  container.innerHTML = html;
}

// Build a tiny SVG sparkline from an array of { t, rate } samples (newest first).
function buildSparkline(samples) {
  if (samples.length < 2) {
    return `<span class="lq-spark-empty">—</span>`;
  }
  const W = 80, H = 24, PAD = 2;
  const vals = [...samples].reverse().map(s => s.rate); // oldest first
  const maxV = Math.max(...vals, 1);
  const pts = vals.map((v, i) => {
    const x = PAD + (i / (vals.length - 1)) * (W - PAD * 2);
    const y = H - PAD - (v / maxV) * (H - PAD * 2);
    return `${x.toFixed(1)},${y.toFixed(1)}`;
  }).join(' ');
  // Colour based on max value
  const colour = maxV >= 20 ? '#f85149' : maxV >= 5 ? '#d29922' : '#3fb950';
  return `<svg width="${W}" height="${H}" viewBox="0 0 ${W} ${H}" class="lq-sparkline">` +
    `<polyline points="${pts}" fill="none" stroke="${colour}" stroke-width="1.5"/>` +
    `</svg>`;
}

// ---- Boot ------------------------------------------------------------------

function initLinkQualityTab() {
  const filterEl = document.getElementById('lq-filter');
  const clearEl  = document.getElementById('lq-filter-clear');
  if (!filterEl) return;
  filterEl.addEventListener('input', () => {
    lqFilterTerm = filterEl.value.trim().toLowerCase();
    renderLinkQualityTab();
  });
  clearEl.addEventListener('click', () => {
    filterEl.value = '';
    lqFilterTerm = '';
    renderLinkQualityTab();
  });
}
