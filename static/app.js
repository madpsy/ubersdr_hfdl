/* -----------------------------------------------------------------------
   UberSDR HFDL — Statistics Dashboard
   ----------------------------------------------------------------------- */

'use strict';

const MAX_FEED_ROWS = 100;

// ---- Aircraft store (kept in sync via SSE) ----------------------------------
// Keys are aircraft key strings, values are AircraftState objects from /aircraft.
const aircraftStore = {};

// ---- Uptime ticker ----------------------------------------------------------
// Stores the unix-second timestamp when the launcher started (from /stats).
// A 1-second interval updates the uptime label locally without extra fetches.
let startTimeSec = null;

function tickUptime() {
  if (startTimeSec == null) return;
  const elapsed = Math.max(0, Math.floor(Date.now() / 1000) - startTimeSec);
  document.getElementById('uptime-label').textContent = 'Uptime: ' + fmtUptime(elapsed);
}

// ---- Ground station name lookup --------------------------------------------
// Populated from /stats ground_stations field on first load.
// Keys are numeric gs_id (as numbers), values are location strings.
let gsNames = {};

// ---- Heard-frequency lookup ------------------------------------------------
// Built from /stats frequencies[].gs_stats on each stats refresh.
// Keys are gs_id (number), values are Set<freq_khz> of frequencies we have
// actually received a message from that ground station on (as source).
let heardFreqsByGS = {};

// ---- Active-frequency set --------------------------------------------------
// Built from /stats frequencies[] on each stats refresh.
// Contains freq_khz values where at least one message has been received
// (regardless of ground station). Used to highlight frequencies in the
// Instances tab.
let activeFreqsKHz = new Set();

// ---- Destination-frequency lookup ------------------------------------------
// Built from /groundstations dst_freqs_khz on each refresh.
// Keys are gs_id (number), values are Set<freq_khz> of frequencies where this
// GS has been seen as the destination of a message.
let dstFreqsByGS = {};

function gsLabel(type, id) {
  if (type === 'Ground station' && gsNames[id]) {
    return `${gsNames[id]} (${id})`;
  }
  if (type === 'Aircraft') {
    return `Aircraft ${id}`;
  }
  return type ? `${esc(type)} ${id}` : '—';
}

// ---- Utility ---------------------------------------------------------------

function fmtTime(unixSec) {
  if (!unixSec) return '—';
  const d = new Date(unixSec * 1000);
  return d.toUTCString().replace(/.*(\d{2}:\d{2}:\d{2}).*/, '$1');
}

function fmtDateTime(unixSec) {
  if (!unixSec) return '—';
  const d = new Date(unixSec * 1000);
  return d.toUTCString().replace('GMT', 'UTC');
}

function fmtUptime(secs) {
  const h = Math.floor(secs / 3600);
  const m = Math.floor((secs % 3600) / 60);
  const s = secs % 60;
  return `${h}h ${m}m ${s}s`;
}

function sigClass(dbfs) {
  if (dbfs >= -25) return 'sig-good';
  if (dbfs >= -40) return 'sig-ok';
  return 'sig-weak';
}

function esc(str) {
  if (!str) return '';
  return str.replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;');
}

// ---- Tab switching ---------------------------------------------------------

function initTabs() {
  document.querySelectorAll('.tab-btn').forEach(btn => {
    btn.addEventListener('click', () => {
      const target = btn.dataset.tab;

      document.querySelectorAll('.tab-btn').forEach(b => b.classList.remove('active'));
      document.querySelectorAll('.tab-panel').forEach(p => p.classList.remove('active'));

      btn.classList.add('active');
      document.getElementById('tab-' + target).classList.add('active');

      // Notify map.js so it can invalidate Leaflet size
      document.dispatchEvent(new CustomEvent('tabchange', { detail: target }));
    });
  });
}

// ---- Frequency table -------------------------------------------------------

function gsNamesForFreq(gsIds) {
  if (!gsIds || gsIds.length === 0) return '';
  return gsIds.map(id => gsNames[id] ? `${gsNames[id]} (${id})` : `GS ${id}`).join(', ');
}

function renderFreqTable(frequencies) {
  const tbody = document.getElementById('freq-tbody');
  if (!frequencies || frequencies.length === 0) {
    tbody.innerHTML = '<tr><td colspan="2" class="empty">No data yet…</td></tr>';
    return;
  }

  // Only show frequencies where at least one GS has been heard
  const active = frequencies.filter(f => f.gs_stats && Object.keys(f.gs_stats).length > 0);
  if (active.length === 0) {
    tbody.innerHTML = '<tr><td colspan="2" class="empty">No ground stations heard yet…</td></tr>';
    return;
  }

  // Sort by frequency Hz ascending
  const sorted = [...active].sort((a, b) => a.freq_hz - b.freq_hz);

  tbody.innerHTML = sorted.map(f => {
    // Sort GS entries by msg_count descending
    const gsEntries = Object.values(f.gs_stats)
      .sort((a, b) => b.msg_count - a.msg_count);

    const gsRows = gsEntries.map(gs => {
      const name = gsNames[gs.gs_id] || `GS ${gs.gs_id}`;
      const sigCls = sigClass(gs.avg_sig_level);
      const lastSeen = gs.last_seen ? fmtDateTime(gs.last_seen) : '—';
      return `<div class="gs-stat-row">
        <span class="gs-stat-name">${esc(name)}</span>
        <span class="gs-stat-msgs">${gs.msg_count.toLocaleString()} msg</span>
        <span class="gs-stat-sig ${sigCls}">${gs.avg_sig_level.toFixed(1)} dBFS</span>
        <span class="gs-stat-time">${lastSeen}</span>
      </div>`;
    }).join('');

    return `<tr>
      <td class="freq-cell">${f.freq_khz.toLocaleString()}</td>
      <td class="gs-stat-cell">${gsRows}</td>
    </tr>`;
  }).join('');
}

// ---- Live feed table -------------------------------------------------------

function buildFeedRow(msg) {
  const time    = fmtTime(msg.time);
  const freq    = msg.freq_khz ? msg.freq_khz.toLocaleString() : '—';
  const slotCls = msg.slot === 'S' ? 'slot-s' : 'slot-d';
  const bps     = msg.bit_rate || '—';
  const sigCls  = sigClass(msg.sig_level);
  const sig     = msg.sig_level != null ? msg.sig_level.toFixed(1) : '—';
  const src     = gsLabel(msg.src_type, msg.src_id);
  const dst     = msg.dst_type ? gsLabel(msg.dst_type, msg.dst_id) : '—';
  const typeCls = msg.msg_type === 'SPDU' ? 'type-spdu' : 'type-lpdu';
  const type    = esc(msg.msg_type) || '—';
  const regFlt  = [esc(msg.reg), esc(msg.flight)].filter(Boolean).join(' / ') || '';

  return `<tr class="feed-new">
    <td>${time}</td>
    <td>${freq}</td>
    <td class="${slotCls}">${esc(msg.slot) || '—'}</td>
    <td>${bps}</td>
    <td class="${sigCls}">${sig}</td>
    <td>${src}</td>
    <td>${dst}</td>
    <td class="${typeCls}">${type}</td>
    <td>${regFlt}</td>
  </tr>`;
}

function prependFeedRow(msg) {
  const tbody = document.getElementById('feed-tbody');

  const empty = tbody.querySelector('.empty');
  if (empty) empty.parentElement.remove();

  tbody.insertAdjacentHTML('afterbegin', buildFeedRow(msg));

  while (tbody.rows.length > MAX_FEED_ROWS) {
    tbody.deleteRow(tbody.rows.length - 1);
  }
}

// ---- Initial stats load ----------------------------------------------------

function loadStats() {
  fetch('/stats')
    .then(r => r.json())
    .then(data => {
      // Populate ground station name lookup (keys come as strings from JSON)
      if (data.ground_stations) {
        gsNames = {};
        for (const [k, v] of Object.entries(data.ground_stations)) {
          gsNames[parseInt(k, 10)] = v;
        }
      }

      // Store start_time so the 1-second ticker can compute uptime locally
      if (data.start_time) {
        startTimeSec = data.start_time;
        tickUptime();
      }
      document.getElementById('total-label').textContent =
        'Total messages: ' + (data.total_messages || 0).toLocaleString();

      renderFreqTable(data.frequencies);

      // Build heardFreqsByGS from per-GS stats on each frequency.
      // activeFreqsKHz is only ever added to (never reset) so that frequencies
      // seen via SSE events are not lost when a stats refresh arrives.
      heardFreqsByGS = {};
      if (Array.isArray(data.frequencies)) {
        for (const freq of data.frequencies) {
          // Any entry in the frequencies array means at least one message was
          // received on that frequency, regardless of source type.
          activeFreqsKHz.add(freq.freq_khz);
          if (!freq.gs_stats) continue;
          for (const gsIdStr of Object.keys(freq.gs_stats)) {
            const gsId = parseInt(gsIdStr, 10);
            if (!heardFreqsByGS[gsId]) heardFreqsByGS[gsId] = new Set();
            heardFreqsByGS[gsId].add(freq.freq_khz);
          }
        }
      }
      // Always fetch ground stations after stats so heardFreqsByGS is ready
      loadGroundStations();
      // Re-render instances tab so activeFreqsKHz highlights are up to date
      if (cachedInstancesData) renderInstances(cachedInstancesData);

      if (data.recent && data.recent.length > 0) {
        const tbody = document.getElementById('feed-tbody');
        tbody.innerHTML = '';
        const reversed = [...data.recent].reverse();
        reversed.forEach(msg => {
          tbody.insertAdjacentHTML('beforeend', buildFeedRow(msg));
        });
        tbody.querySelectorAll('.feed-new').forEach(r => r.classList.remove('feed-new'));
      }
    })
    .catch(err => console.warn('stats fetch error:', err));
}

// ---- SSE live feed ---------------------------------------------------------

let evtSource = null;
let reconnectTimer = null;

function setIndicator(state) {
  const el = document.getElementById('conn-indicator');
  el.className = 'indicator ' + state;
  el.title = state.charAt(0).toUpperCase() + state.slice(1);
}

function handleSSEEvent(raw) {
  let envelope;
  try {
    envelope = JSON.parse(raw);
  } catch (err) {
    console.warn('SSE parse error:', err);
    return;
  }

  const { type, data } = envelope;

  if (type === 'message') {
    prependFeedRow(data);
    // Bump total counter
    const lbl = document.getElementById('total-label');
    const cur = parseInt(lbl.textContent.replace(/\D/g, ''), 10) || 0;
    lbl.textContent = 'Total messages: ' + (cur + 1).toLocaleString();
    // Mark this frequency as active immediately and re-render instances chips
    if (data.freq_khz) {
      activeFreqsKHz.add(data.freq_khz);
      if (cachedInstancesData) renderInstances(cachedInstancesData);
    }

  } else if (type === 'position') {
    // Update local store and re-render planes table if visible
    aircraftStore[data.key] = data;
    if (document.getElementById('tab-planes').classList.contains('active')) {
      renderAircraftTable();
    }
    // Delegate to map.js
    if (typeof handlePositionEvent === 'function') {
      handlePositionEvent(data);
    }

  } else if (type === 'purge') {
    // Remove from local store and re-render planes table if visible
    delete aircraftStore[data];
    if (document.getElementById('tab-planes').classList.contains('active')) {
      renderAircraftTable();
    }
    // data is the aircraft key string
    if (typeof handlePurgeEvent === 'function') {
      handlePurgeEvent(data);
    }
  }
}

function connectSSE() {
  if (evtSource) {
    evtSource.close();
    evtSource = null;
  }

  setIndicator('connecting');
  evtSource = new EventSource('/events');

  evtSource.onopen = () => {
    setIndicator('connected');
    if (reconnectTimer) {
      clearTimeout(reconnectTimer);
      reconnectTimer = null;
    }
  };

  evtSource.onmessage = (e) => handleSSEEvent(e.data);

  evtSource.onerror = () => {
    setIndicator('disconnected');
    evtSource.close();
    evtSource = null;
    reconnectTimer = setTimeout(connectSSE, 5000);
  };
}

// ---- Periodic stats refresh ------------------------------------------------

function startPeriodicRefresh(intervalMs) {
  setInterval(() => {
    loadStats();
  }, intervalMs);
}

// ---- Planes tab ------------------------------------------------------------

function renderAircraftTable() {
  const tbody = document.getElementById('planes-tbody');
  const list = Object.values(aircraftStore);
  if (list.length === 0) {
    tbody.innerHTML = '<tr><td colspan="10" class="empty">No aircraft seen yet…</td></tr>';
    return;
  }

  // Sort by last_seen descending
  list.sort((a, b) => (b.last_seen || 0) - (a.last_seen || 0));

  tbody.innerHTML = list.map(ac => {
    const gsName = ac.gs_id && gsNames[ac.gs_id]
      ? `${gsNames[ac.gs_id]} (${ac.gs_id})`
      : (ac.gs_id ? `GS ${ac.gs_id}` : '—');
    const lat  = ac.lat  ? ac.lat.toFixed(4)  : '—';
    const lon  = ac.lon  ? ac.lon.toFixed(4)  : '—';
    const freq = ac.freq_khz ? ac.freq_khz.toLocaleString() : '—';
    const sig  = ac.sig_level != null ? ac.sig_level.toFixed(1) : '—';
    const sigCls = ac.sig_level != null ? sigClass(ac.sig_level) : '';
    return `<tr>
      <td class="mono">${esc(ac.icao) || '—'}</td>
      <td class="mono">${esc(ac.reg)  || '—'}</td>
      <td class="mono">${esc(ac.flight) || '—'}</td>
      <td class="mono dim">${lat}</td>
      <td class="mono dim">${lon}</td>
      <td class="mono">${freq}</td>
      <td>${esc(gsName)}</td>
      <td class="mono ${sigCls}">${sig !== '—' ? sig + ' dBFS' : '—'}</td>
      <td class="mono">${ac.msg_count || 0}</td>
      <td class="dim">${fmtDateTime(ac.last_seen)}</td>
    </tr>`;
  }).join('');
}

function loadAircraftTab() {
  fetch('/aircraft')
    .then(r => r.json())
    .then(list => {
      if (!Array.isArray(list)) return;
      for (const ac of list) aircraftStore[ac.key] = ac;
      renderAircraftTable();
    })
    .catch(err => console.warn('aircraft fetch error:', err));
}

// ---- Ground stations tab ---------------------------------------------------

function renderGroundStations(stations) {
  const container = document.getElementById('gs-grid');
  if (!stations || stations.length === 0) {
    container.innerHTML = '<p class="empty" style="padding:20px">No ground station data loaded.</p>';
    return;
  }

  container.innerHTML = stations.map(gs => {
     const heardSet = heardFreqsByGS[gs.gs_id];
     const dstSet   = dstFreqsByGS[gs.gs_id];
     const freqs = (gs.frequencies || [])
       .map(f => {
         const isDisabled = f.enabled === false;
         const isHeard = !isDisabled && heardSet && heardSet.has(f.freq_khz);
         const isDst   = !isDisabled && !isHeard && dstSet && dstSet.has(f.freq_khz);
         const cls = isDisabled ? ' gs-freq--disabled'
                   : isHeard   ? ' gs-freq--heard'
                   : isDst     ? ' gs-freq--dst'
                   : '';
         return `<span class="gs-freq${cls}">${f.freq_khz.toLocaleString()}<span class="gs-slot">T${f.timeslot}</span></span>`;
       })
       .join('');
    const heard = gs.last_heard > 0;
    const heardBadge = heard
      ? `<span class="gs-heard" title="Last heard ${fmtDateTime(gs.last_heard)}">● heard ${fmtTime(gs.last_heard)}</span>`
      : '';
    return `<div class="gs-card${heard ? ' gs-card--heard' : ''}">
      <div class="gs-card-header">
        <span class="gs-id">GS ${gs.gs_id}</span>
        <span class="gs-location">${esc(gs.location)}</span>
        ${heardBadge}
      </div>
      <div class="gs-freqs">${freqs || '<span class="gs-no-freqs">No frequencies</span>'}</div>
    </div>`;
  }).join('');
}

function loadGroundStations() {
  fetch('/groundstations')
    .then(r => r.json())
    .then(data => {
      // Build dstFreqsByGS from the dst_freqs_khz field on each station
      dstFreqsByGS = {};
      for (const gs of data) {
        if (Array.isArray(gs.dst_freqs_khz) && gs.dst_freqs_khz.length > 0) {
          dstFreqsByGS[gs.gs_id] = new Set(gs.dst_freqs_khz);
        }
      }
      renderGroundStations(data);
    })
    .catch(err => console.warn('groundstations fetch error:', err));
}

// ---- Instances tab ---------------------------------------------------------

// Cache the last /instances response so we can re-render when activeFreqsKHz
// is updated by a stats refresh.
let cachedInstancesData = null;

function loadInstances() {
  return fetch('/instances')
    .then(r => r.json())
    .then(data => {
      cachedInstancesData = data;
      renderInstances(data);
    })
    .catch(err => console.warn('instances fetch error:', err));
}

function renderInstances(data) {
  const extraEl = document.getElementById('instances-extra-args');
  const windowsEl = document.getElementById('instances-windows');
  if (!extraEl || !windowsEl) return;

  // Frequency source URL
  const freqURL = data.freq_url || '';
  let extraHtml =
    `<div class="instances-section-title">Frequency source</div>` +
    `<div class="instances-args-box"><code>${esc(freqURL) || '<em>default</em>'}</code></div>`;

  // Extra args section
  const args = Array.isArray(data.extra_args) ? data.extra_args : [];
  if (args.length > 0) {
    extraHtml +=
      `<div class="instances-section-title">Extra dumphfdl arguments</div>` +
      `<div class="instances-args-box"><code>${esc(args.join(' '))}</code></div>`;
  } else {
    extraHtml +=
      `<div class="instances-section-title">Extra dumphfdl arguments</div>` +
      `<div class="instances-args-box instances-args-box--empty">None</div>`;
  }
  extraEl.innerHTML = extraHtml;

  // Windows section
  const windows = Array.isArray(data.windows) ? data.windows : [];
  if (windows.length === 0) {
    windowsEl.innerHTML = `<div class="instances-section-title">IQ Windows</div><p class="empty">No windows configured.</p>`;
    return;
  }

  let html = `<div class="instances-section-title">IQ Windows (${windows.length})</div>`;
  html += `<div class="instances-grid">`;
  for (const w of windows) {
    const freqs = (w.freqs_khz || []).map(f => {
      const active = activeFreqsKHz.has(f);
      return `<span class="instances-freq${active ? ' instances-freq--active' : ''}">${f.toLocaleString()}</span>`;
    }).join('');
    html +=
      `<div class="instances-card">` +
        `<div class="instances-card__header">` +
          `<span class="instances-card__centre">${w.center_khz.toLocaleString()} kHz</span>` +
          `<span class="instances-card__mode">${esc(w.iq_mode)} · ${w.bandwidth_khz} kHz BW</span>` +
        `</div>` +
        `<div class="instances-card__freqs">${freqs}</div>` +
      `</div>`;
  }
  html += `</div>`;

  // Disabled frequencies — shown as a flat list of red chips below the windows
  const disabledFreqs = Array.isArray(data.disabled_freqs) ? data.disabled_freqs : [];
  if (disabledFreqs.length > 0) {
    html += `<div class="instances-section-title">Disabled Frequencies (${disabledFreqs.length})</div>`;
    html += `<div class="instances-disabled-freqs">`;
    html += disabledFreqs.map(f =>
      `<span class="instances-freq instances-freq--disabled">${f.toLocaleString()}</span>`
    ).join('');
    html += `</div>`;
  }

  windowsEl.innerHTML = html;
}

// ---- Signal history charts -------------------------------------------------

// Map of "gsId:freqKhz" → Chart instance (so we can update in place)
const signalCharts = {};

function loadSignalHistory() {
  fetch('/signal')
    .then(r => r.json())
    .then(renderSignalCharts)
    .catch(err => console.warn('signal fetch error:', err));
}

function renderSignalCharts(series) {
  const container = document.getElementById('signal-charts');
  if (!container) return;

  if (!Array.isArray(series) || series.length === 0) {
    container.innerHTML = '<p id="signal-empty">No signal history yet — data accumulates in 30-minute buckets.</p>';
    return;
  }

  // Remove stale empty message if present
  const empty = container.querySelector('#signal-empty');
  if (empty) empty.remove();

  for (const s of series) {
    const key = `${s.gs_id}:${s.freq_khz}`;
    const labels = s.buckets.map(b => {
      const d = new Date(b.t * 1000);
      return d.toUTCString().slice(17, 22); // "HH:MM"
    });
    const avgData = s.buckets.map(b => b.avg);
    const minData = s.buckets.map(b => b.min);
    const maxData = s.buckets.map(b => b.max);

    if (signalCharts[key]) {
      // Update existing chart
      const ch = signalCharts[key];
      ch.data.labels = labels;
      ch.data.datasets[0].data = avgData;
      ch.data.datasets[1].data = minData;
      ch.data.datasets[2].data = maxData;
      ch.update('none');
      continue;
    }

    // Create card + canvas
    const card = document.createElement('div');
    card.className = 'sig-chart-card';
    card.dataset.key = key;
    card.innerHTML =
      `<div class="sig-chart-card__title">${esc(s.location)}</div>` +
      `<div class="sig-chart-card__subtitle">${(s.freq_khz / 1000).toFixed(3)} MHz · GS ${s.gs_id}</div>` +
      `<canvas id="sig-canvas-${key.replace(':', '-')}"></canvas>`;
    container.appendChild(card);

    const canvas = card.querySelector('canvas');
    const chart = new Chart(canvas, {
      type: 'line',
      data: {
        labels,
        datasets: [
          {
            label: 'Avg (dBFS)',
            data: avgData,
            borderColor: '#58a6ff',
            backgroundColor: 'rgba(88,166,255,0.12)',
            borderWidth: 2,
            pointRadius: 2,
            tension: 0.3,
            fill: false,
          },
          {
            label: 'Min',
            data: minData,
            borderColor: '#f78166',
            borderWidth: 1,
            borderDash: [4, 3],
            pointRadius: 2,
            tension: 0.3,
            fill: false,
          },
          {
            label: 'Max',
            data: maxData,
            borderColor: '#3fb950',
            borderWidth: 1,
            borderDash: [4, 3],
            pointRadius: 2,
            tension: 0.3,
            fill: false,
          },
        ],
      },
      options: {
        animation: false,
        responsive: true,
        maintainAspectRatio: false,
        plugins: {
          legend: {
            labels: { color: '#c9d1d9', font: { size: 11 } },
          },
          tooltip: {
            callbacks: {
              label: ctx => `${ctx.dataset.label}: ${ctx.parsed.y.toFixed(1)} dBFS`,
            },
          },
        },
        scales: {
          x: {
            ticks: { color: '#8b949e', font: { size: 10 }, maxRotation: 0 },
            grid:  { color: 'rgba(48,54,61,0.8)' },
          },
          y: {
            ticks: {
              color: '#8b949e',
              font: { size: 10 },
              callback: v => v.toFixed(0) + ' dB',
            },
            grid: { color: 'rgba(48,54,61,0.8)' },
          },
        },
      },
    });
    signalCharts[key] = chart;
  }
}

// ---- Export active frequencies ---------------------------------------------

function exportActiveFrequencies() {
  const a = document.createElement('a');
  a.href = '/export/frequencies';
  a.download = 'hfdl_frequencies.jsonl';
  document.body.appendChild(a);
  a.click();
  document.body.removeChild(a);
}

function exportAllFrequencies() {
  const a = document.createElement('a');
  a.href = '/export/frequencies/all';
  a.download = 'hfdl_frequencies.jsonl';
  document.body.appendChild(a);
  a.click();
  document.body.removeChild(a);
}

// ---- Boot ------------------------------------------------------------------

document.addEventListener('DOMContentLoaded', () => {
  initTabs();
  // Chain instances → stats so that cachedInstancesData is set before loadStats()
  // calls renderInstances(), ensuring frequency chips are coloured correctly on
  // the first render after a page load.
  loadInstances().then(() => loadStats());
  loadAircraftTab();
  loadSignalHistory();
  setInterval(loadSignalHistory, 60_000);
  connectSSE();
  startPeriodicRefresh(15000);
  setInterval(tickUptime, 1_000);

  document.getElementById('btn-export-freqs').addEventListener('click', exportActiveFrequencies);
  document.getElementById('btn-export-all-freqs').addEventListener('click', exportAllFrequencies);

  // Re-render planes table when switching to that tab
  document.addEventListener('tabchange', (e) => {
    if (e.detail === 'planes') renderAircraftTable();
    if (e.detail === 'signal') loadSignalHistory();
  });

  // Initialise the Leaflet map (defined in map.js)
  if (typeof initMap === 'function') {
    initMap();
  }
});
