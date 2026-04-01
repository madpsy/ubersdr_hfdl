/* -----------------------------------------------------------------------
   UberSDR HFDL — Statistics Dashboard
   ----------------------------------------------------------------------- */

'use strict';

const MAX_FEED_ROWS = 200;
const MAX_MESSAGES_ROWS = 500; // Messages tab ring buffer
const MAX_EVENTS_ROWS = 500;   // Events tab ring buffer

// ---- In-memory ring buffers for new tabs -----------------------------------
// Each entry is the raw SSE data object for that event type.
const feedStore     = []; // all frames — backing store for the Live Feed tab
const messagesStore = []; // ACARS messages with msg_text
const eventsStore   = []; // logon / logoff / gs_event / notable frames

// Flag: true after the first /stats response has seeded the stores from
// data.recent so that subsequent periodic refreshes don't re-seed.
let _storesSeeded = false;

// ---- Aircraft store (kept in sync via SSE) ----------------------------------
// Keys are aircraft key strings, values are AircraftState objects from /aircraft.
const aircraftStore = {};

// ---- Uptime ticker ----------------------------------------------------------
// Stores the unix-second timestamp when the launcher started (from /stats).
// A 1-second interval updates the uptime label locally without extra fetches.
let startTimeSec = null;

// Map of window index → started_at unix seconds (0 = not running).
// Populated by renderInstances() so tickUptime() can update the DOM each second
// without waiting for the next /instances poll.
const instanceStartedAt = {};

function tickUptime() {
  if (startTimeSec != null) {
    const elapsed = Math.max(0, Math.floor(Date.now() / 1000) - startTimeSec);
    document.getElementById('uptime-label').textContent = 'Uptime: ' + fmtUptime(elapsed);
  }

  // Tick per-instance uptimes in the Instances tab
  const nowSec = Math.floor(Date.now() / 1000);
  for (const [idx, startedAt] of Object.entries(instanceStartedAt)) {
    if (!startedAt) continue;
    const el = document.getElementById(`inst-uptime-${idx}`);
    if (!el) continue;
    el.textContent = 'up ' + fmtUptimeShort(nowSec - startedAt);
  }
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

// ---- Per-frequency message counts ------------------------------------------
// Map of freq_khz → total message count across all ground stations.
// Populated from /stats frequencies[].gs_stats on each refresh and
// incremented by SSE message events. Used to show counts in the Instances tab.
const freqMsgCounts = new Map();

// ---- Destination-frequency lookup ------------------------------------------
// Built from /groundstations dst_freqs_khz on each refresh.
// Keys are gs_id (number), values are Set<freq_khz> of frequencies where this
// GS has been seen as the destination of a message.
let dstFreqsByGS = {};

function gsLabel(type, id, icao) {
  if (type === 'Ground station' && gsNames[id]) {
    return `${gsNames[id]} (${id})`;
  }
  if (type === 'Aircraft') {
    // Show ICAO if known, otherwise fall back to slot ID
    return icao ? esc(icao) : `Aircraft ${id}`;
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
  const src     = gsLabel(msg.src_type, msg.src_id, msg.src_icao);
  const dst     = msg.dst_type ? gsLabel(msg.dst_type, msg.dst_id) : '—';
  const typeCls = msg.msg_type === 'SPDU' ? 'type-spdu' : 'type-lpdu';
  const type    = esc(msg.msg_type) || '—';
  const regFlt  = [esc(msg.reg), esc(msg.flight)].filter(Boolean).join(' / ') || '';
  // Phase 1b: truncated message text (max 60 chars)
  const msgText = msg.msg_text
    ? `<span class="feed-msg-text" title="${esc(msg.msg_text)}">${esc(msg.msg_text.slice(0, 60))}${msg.msg_text.length > 60 ? '…' : ''}</span>`
    : '';
  // Section 7.1: row colour class by LPDU type
  const rowCls = msg.msg_type === 'Logon confirm'
    ? ' feed-row--logon'
    : (msg.msg_type === 'Logoff request' || msg.msg_type === 'Logon denied')
      ? ' feed-row--logoff'
      : '';
  // Section 7.1: datalink icon
  const dl = datalinkLabel(msg.current_link);

  return `<tr class="feed-new${rowCls}">
    <td>${time}</td>
    <td>${freq}</td>
    <td class="${slotCls}">${esc(msg.slot) || '—'}</td>
    <td>${bps}</td>
    <td class="${sigCls}">${sig}</td>
    <td>${src}</td>
    <td>${dst}</td>
    <td class="${typeCls}">${type}</td>
    <td>${regFlt}</td>
    <td class="mono">${dl || '—'}</td>
    <td class="feed-msg-cell">${msgText}</td>
  </tr>`;
}

function prependFeedRow(msg) {
  // Keep the backing store in sync
  feedStore.unshift(msg);
  if (feedStore.length > MAX_FEED_ROWS) feedStore.length = MAX_FEED_ROWS;

  // Only add to DOM if the row passes the current filter
  if (!feedMatchesFilter(msg)) {
    updateFeedCount();
    return;
  }

  const tbody = document.getElementById('feed-tbody');
  const empty = tbody.querySelector('.empty');
  if (empty) empty.parentElement.remove();

  tbody.insertAdjacentHTML('afterbegin', buildFeedRow(msg));

  // Trim visible rows to MAX_FEED_ROWS
  while (tbody.rows.length > MAX_FEED_ROWS) {
    tbody.deleteRow(tbody.rows.length - 1);
  }
  updateFeedCount();
}

// ---- Live Feed filter -------------------------------------------------------

const feedFilter = { reg: '', flight: '', type: '' };

function feedMatchesFilter(msg) {
  if (feedFilter.reg) {
    const t = feedFilter.reg;
    if (!(msg.reg || '').toLowerCase().includes(t) &&
        !(msg.src_icao || '').toLowerCase().includes(t)) return false;
  }
  if (feedFilter.flight && !(msg.flight || '').toLowerCase().includes(feedFilter.flight)) return false;
  if (feedFilter.type  && !(msg.msg_type || '').toLowerCase().includes(feedFilter.type))  return false;
  return true;
}

function updateFeedCount() {
  const countEl = document.getElementById('feed-count-label');
  if (!countEl) return;
  const visible = feedStore.filter(feedMatchesFilter).length;
  countEl.textContent = feedFilter.reg || feedFilter.flight || feedFilter.type
    ? `${visible} / ${feedStore.length}`
    : `${feedStore.length}`;
}

function renderFeedTable() {
  const tbody = document.getElementById('feed-tbody');
  if (!tbody) return;
  const filtered = feedStore.filter(feedMatchesFilter);
  updateFeedCount();
  if (filtered.length === 0) {
    tbody.innerHTML = `<tr><td colspan="11" class="empty">${
      feedFilter.reg || feedFilter.flight || feedFilter.type
        ? 'No messages match the filter…'
        : 'Waiting for messages…'
    }</td></tr>`;
    return;
  }
  tbody.innerHTML = filtered.map(buildFeedRow).join('');
  tbody.querySelectorAll('.feed-new').forEach(r => r.classList.remove('feed-new'));
}

function initFeedFilter() {
  const regEl    = document.getElementById('feed-filter-reg');
  const flightEl = document.getElementById('feed-filter-flight');
  const typeEl   = document.getElementById('feed-filter-type');
  const clearEl  = document.getElementById('feed-filter-clear');
  if (!regEl) return;

  function applyFilter() {
    feedFilter.reg    = regEl.value.trim().toLowerCase();
    feedFilter.flight = flightEl.value.trim().toLowerCase();
    feedFilter.type   = typeEl.value.trim().toLowerCase();
    renderFeedTable();
  }
  regEl.addEventListener('input', applyFilter);
  flightEl.addEventListener('input', applyFilter);
  typeEl.addEventListener('input', applyFilter);
  clearEl.addEventListener('click', () => {
    regEl.value = flightEl.value = typeEl.value = '';
    feedFilter.reg = feedFilter.flight = feedFilter.type = '';
    renderFeedTable();
  });
}

// ---- ARINC 620 free-text field decoder ------------------------------------
// Decodes ARINC 620 field codes (AN, FI, GL, MA, etc.) into human-readable
// key-value pairs. Used for H1 messages that contain structured free-text.

const ARINC620_FIELDS = {
  'AN': 'Aircraft', 'FI': 'Flight', 'FN': 'Flight No',
  'OR': 'Origin', 'DS': 'Destination',
  'DP': 'Departure', 'AR': 'Arrival', 'ETA': 'ETA', 'ATA': 'ATA',
  'ETD': 'ETD', 'ATD': 'ATD', 'GL': 'Gate', 'MA': 'Mach/Alt',
  'WT': 'Weight', 'FU': 'Fuel', 'FB': 'Fuel Burn', 'FR': 'Fuel Remaining',
  'FOB': 'Fuel on Board',
  'OA': 'OAT', 'WS': 'Wind Speed', 'WD': 'Wind Dir',
  'TA': 'True Airspeed', 'GS': 'Ground Speed', 'AL': 'Altitude',
  'LA': 'Latitude', 'LO': 'Longitude', 'PO': 'Position',
  'TI': 'Time', 'DT': 'Departure Data', 'RM': 'Remarks', 'TX': 'Text',
  'MR': 'Message Ref', 'ST': 'Status', 'CF': 'Config',
  'EC': 'Engine Condition', 'EI': 'Engine Indication',
  'LB': 'Load Balance', 'TD': 'Takeoff Data', 'LD': 'Landing Data',
  'CR': 'Crew', 'CA': 'Captain', 'FO': 'First Officer',
  'PA': 'Passengers', 'BG': 'Baggage', 'CG': 'Cargo',
  'MX': 'Maintenance', 'DL': 'Delay', 'DI': 'Diversion', 'EM': 'Emergency',
  'SP': 'Speed', 'HD': 'Heading', 'TK': 'Track', 'VS': 'Vert Speed',
  'FL': 'Flight Level', 'WX': 'Weather', 'TU': 'Turbulence',
  'IC': 'Icing', 'CB': 'CB Activity',
  'PRG': 'Position Report', 'OFF': 'Takeoff', 'LND': 'Landing',
  'AGM': 'Ground Message', 'ATA': 'Actual Arrival',
};

// ---- Label 16 weather/position observation decoder -----------------------
// Format: HHMMSS,AAAAA,SSSS,HHH,N DD.DDD E DDD.DDD
// Example: 170130,33994,1915, 103,N 51.007 E 51.808

function decodeLabel16(msgText) {
  if (!msgText) return null;
  const parts = msgText.trim().split(',');
  if (parts.length < 5) return null;

  const timeStr = parts[0].trim();
  const altStr  = parts[1].trim();
  const spdStr  = parts[2].trim();
  const hdgStr  = parts[3].trim();
  const posStr  = parts.slice(4).join(',').trim();

  const result = [];

  // Time: HHMMSS
  if (/^\d{6}$/.test(timeStr)) {
    result.push(`Time: ${timeStr.slice(0,2)}:${timeStr.slice(2,4)}:${timeStr.slice(4,6)} UTC`);
  }
  // Altitude in feet
  const alt = parseFloat(altStr);
  if (!isNaN(alt) && alt > 0) {
    result.push(`Alt: ${Math.round(alt).toLocaleString()} ft`);
  }
  // Speed in knots (may be encoded as tenths)
  const spd = parseFloat(spdStr);
  if (!isNaN(spd) && spd > 0) {
    result.push(`Speed: ${(spd / 10).toFixed(0)} kts`);
  }
  // Heading
  const hdg = parseFloat(hdgStr);
  if (!isNaN(hdg)) {
    result.push(`Track: ${Math.round(hdg)}°`);
  }
  // Position: N DD.DDD E DDD.DDD
  const posM = posStr.match(/([NS])\s*([\d.]+)\s*([EW])\s*([\d.]+)/);
  if (posM) {
    const lat = (posM[1] === 'S' ? '-' : '') + posM[2] + '°' + posM[1];
    const lon = (posM[3] === 'W' ? '-' : '') + posM[4] + '°' + posM[3];
    result.push(`Pos: ${lat} ${lon}`);
  }

  return result.length > 0 ? result.join(' · ') : null;
}

// These codes are message type prefixes, not field codes with values
const ARINC620_TYPE_PREFIXES = new Set(['PRG', 'OFF', 'LND', 'AGM', 'DFD', 'ARR', 'DEP']);

function decodeArinc620(msgText) {
  if (!msgText) return null;
  const lines = msgText.split(/\r?\n/);
  const pairs = [];
  let msgTypeLabel = null;

  for (const line of lines) {
    // Fields may be slash-separated on one line or on separate lines
    const segments = line.trim().split('/');
    for (const seg of segments) {
      const trimmed = seg.trim();
      if (!trimmed) continue;

      // Check if this segment is a message type prefix (no value)
      if (ARINC620_TYPE_PREFIXES.has(trimmed)) {
        if (!msgTypeLabel) msgTypeLabel = ARINC620_FIELDS[trimmed] || trimmed;
        continue;
      }

      // Try two patterns:
      // 1. "XX value" — space-separated (standard ARINC 620)
      // 2. "XXvalue"  — no space (compact format used by some airlines)
      let m = trimmed.match(/^([A-Z]{2,3})\s+(.+)$/);
      if (!m) {
        // Try matching known field codes at the start without a space
        for (const code of Object.keys(ARINC620_FIELDS)) {
          if (ARINC620_TYPE_PREFIXES.has(code)) continue; // skip type prefixes
          if (trimmed.startsWith(code) && trimmed.length > code.length) {
            const nextChar = trimmed[code.length];
            // Only match if next char is not a letter (avoid false matches like "FLIGHT" matching "FL")
            if (!/[A-Z]/.test(nextChar)) {
              const val = trimmed.slice(code.length).trim();
              if (val) {
                m = [null, code, val];
                break;
              }
            }
          }
        }
      }
      if (m) {
        const code = m[1];
        const val  = m[2].trim();
        const name = ARINC620_FIELDS[code];
        if (name && !ARINC620_TYPE_PREFIXES.has(code)) pairs.push(`${name}: ${val}`);
      }
    }
  }

  if (pairs.length === 0) return null;
  const prefix = msgTypeLabel ? `[${msgTypeLabel}] ` : '';
  return prefix + pairs.join(' · ');
}

// ---- Media Advisory (SA) decoder ------------------------------------------
// Decodes the raw msg_text of ACARS label SA messages into human-readable form.
//
// Two known formats:
//   0E<cur><HHMMSS><count><avail...>/   e.g. 0EH1735262SH/
//   0L<cur><HHMMSS><avail...>           e.g. 0LV173606HS
//
// Link codes: H=HF, V=VHF, S=SATCOM, G=GACS, 2=VHF2, 3=VHF3

const MEDIA_ADV_LINKS = {
  'H': 'HF',
  'V': 'VHF',
  'S': 'SATCOM',
  'G': 'GACS',
  '2': 'VHF 2',
  '3': 'VHF 3',
  'I': 'Iridium',
};

function decodeMediaAdvisory(msgText) {
  if (!msgText || msgText.length < 10) return null;
  // Must start with 0E or 0L (or similar 0x prefix)
  if (msgText[0] !== '0') return null;

  const subtype = msgText[1]; // E, L, or other
  const curCode = msgText[2]; // current link code
  const timeStr = msgText.slice(3, 9); // HHMMSS

  if (!/^\d{6}$/.test(timeStr)) return null;

  const hh = timeStr.slice(0, 2);
  const mm = timeStr.slice(2, 4);
  const ss = timeStr.slice(4, 6);
  const timeFormatted = `${hh}:${mm}:${ss} UTC`;

  const curName = MEDIA_ADV_LINKS[curCode] || curCode;

  // Parse available links
  let availStr = msgText.slice(9).replace(/\/$/, ''); // strip trailing /
  let availLinks = [];

  if (subtype === 'E' && availStr.length > 0) {
    // First char is count, rest are link codes
    const count = parseInt(availStr[0], 10);
    if (!isNaN(count)) {
      for (let i = 1; i <= count && i < availStr.length; i++) {
        const name = MEDIA_ADV_LINKS[availStr[i]] || availStr[i];
        availLinks.push(name);
      }
    }
  } else {
    // Each remaining char is a link code
    for (const ch of availStr) {
      if (MEDIA_ADV_LINKS[ch] || /[A-Z0-9]/.test(ch)) {
        availLinks.push(MEDIA_ADV_LINKS[ch] || ch);
      }
    }
  }

  let result = `Current: ${curName} (${timeFormatted})`;
  if (availLinks.length > 0) {
    result += ` · Available: ${availLinks.join(', ')}`;
  }
  return result;
}

// ---- ACARS sublabel lookup (H1 sublabels decoded by libacars) --------------
// Only official/standardised sublabels are listed here.
// Airline-specific ones (M1, M2, etc.) are shown as raw codes.

const ACARS_SUBLABELS = {
  'DF': 'DFDR Data',
  'MD': 'Met Dispatch',
  'WX': 'Weather Report',
  'PO': 'Position Report',
  'PR': 'Position Report',
  'FI': 'Flight Info',
  'CF': 'Company Flight Plan',
  'EC': 'Engine Condition',
  'EI': 'Engine Indication',
  'LB': 'Load Balance',
  'TD': 'Takeoff Data',
  'LD': 'Landing Data',
  'S1': 'Fuel Data',
  'S2': 'Fuel Data',
  'S3': 'Fuel Data',
  'D1': 'Departure Report',
  'A1': 'Arrival Report',
  'A2': 'Arrival Report',
  'T1': 'Takeoff Report',
  'T2': 'Takeoff Report',
  'T3': 'Takeoff Report',
  'T4': 'Takeoff Report',
  'T5': 'Takeoff Report',
  'T6': 'Takeoff Report',
  'T7': 'Takeoff Report',
  'T8': 'Takeoff Report',
  'T9': 'Takeoff Report',
};

function acarsSubLabelName(sublabel) {
  if (!sublabel) return '';
  const name = ACARS_SUBLABELS[sublabel];
  if (name) return `${name} (${sublabel})`;
  return sublabel; // raw code for unknown/airline-specific sublabels
}

// ---- ACARS label lookup ----------------------------------------------------

const ACARS_LABELS = {
  '_d': 'Downlink ACK',
  '_u': 'Uplink ACK',
  'H1': 'Position / Weather',
  'H2': 'Position',
  'SA': 'Media Advisory',
  'SQ': 'Squitter',
  'Q0': 'AOC',
  'Q1': 'AOC',
  'Q2': 'AOC',
  'Q3': 'AOC',
  'Q4': 'AOC',
  'Q5': 'AOC',
  'Q6': 'AOC',
  'Q7': 'AOC',
  'Q8': 'AOC',
  'Q9': 'AOC',
  'A6': 'ADS-C',
  'A7': 'ADS-C',
  'B6': 'Airline Ops',
  'B7': 'Airline Ops',
  'C1': 'Airline Ops',
  'C3': 'Airline Ops',
  'D0': 'ATIS',
  'E0': 'ATIS',
  'F3': 'Airline Ops',
  'FI': 'Flight Info',
  'G1': 'Airline Ops',
  'H3': 'Airline Ops',
  'I1': 'Airline Ops',
  'L1': 'Airline Ops',
  'L2': 'Airline Ops',
  'M1': 'Airline Ops',
  'M2': 'Airline Ops',
  'M3': 'Airline Ops',
  'N1': 'Airline Ops',
  'P1': 'Airline Ops',
  'P2': 'Airline Ops',
  'P3': 'Airline Ops',
  'R1': 'Airline Ops',
  'R2': 'Airline Ops',
  'R3': 'Airline Ops',
  'S1': 'Airline Ops',
  'S2': 'Airline Ops',
  'S3': 'Airline Ops',
  'T1': 'Airline Ops',
  'T2': 'Airline Ops',
  'T3': 'Airline Ops',
  'V1': 'Airline Ops',
  'W1': 'Airline Ops',
  'X1': 'Airline Ops',
  '10': 'Airline Ops',
  '11': 'Airline Ops',
  '12': 'Airline Ops',
  '13': 'Airline Ops',
  '14': 'Airline Ops',
  '15': 'Airline Ops',
  '16': 'Weather Obs',
  '17': 'Airline Ops',
  '18': 'Airline Ops',
  '19': 'Airline Ops',
  '20': 'Airline Ops',
  '21': 'Airline Ops',
  '22': 'Airline Ops',
  '5U': 'CPDLC',
  '5V': 'CPDLC',
  '5W': 'CPDLC',
  '5X': 'CPDLC',
  '5Y': 'CPDLC',
  '5Z': 'CPDLC',
};

function acarsLabelName(label) {
  if (!label) return '—';
  const name = ACARS_LABELS[label];
  if (name) return `${name} (${label})`;
  return label;
}

// ---- Messages tab (Phase 1b) -----------------------------------------------

// Active filter state
const msgFilter = { reg: '', flight: '', label: '' };

function isWeatherMsg(msg) {
  return msg.label === 'H1' && msg.sublabel === 'WX';
}

function msgMatchesFilter(msg) {
  if (msgFilter.reg && !(msg.reg || '').toLowerCase().includes(msgFilter.reg)) return false;
  if (msgFilter.flight && !(msg.flight || '').toLowerCase().includes(msgFilter.flight)) return false;
  if (msgFilter.label && !(msg.label || '').toLowerCase().includes(msgFilter.label)) return false;
  return true;
}

function renderMessagesTable() {
  const tbody = document.getElementById('messages-tbody');
  if (!tbody) return;
  const filtered = messagesStore.filter(msgMatchesFilter);
  const countEl = document.getElementById('msg-count-label');
  if (countEl) countEl.textContent = `${filtered.length} / ${messagesStore.length}`;
  if (filtered.length === 0) {
    tbody.innerHTML = '<tr><td colspan="8" class="empty">No ACARS messages match the filter…</td></tr>';
    return;
  }
  tbody.innerHTML = filtered.map(msg => {
    const gsName = msg.dst_type === 'Ground station' && gsNames[msg.dst_id]
      ? `${gsNames[msg.dst_id]} (${msg.dst_id})`
      : (msg.dst_id ? `GS ${msg.dst_id}` : '—');
    const weatherCls = isWeatherMsg(msg) ? ' msg-row--weather' : '';
    // Attempt to decode known message formats into human-readable text.
    let displayText = msg.msg_text || '';
    let decodedText = null;
    if (msg.label === 'SA' && msg.msg_text) {
      // Media Advisory — decode datalink status
      decodedText = decodeMediaAdvisory(msg.msg_text);
    } else if (msg.label === '16' && msg.msg_text) {
      // Label 16 weather/position observation
      decodedText = decodeLabel16(msg.msg_text);
    } else if (msg.msg_text && !msg.msg_text.startsWith('POS')) {
      // Try ARINC 620 field decoder for any label that may contain structured free-text
      // (H1, C1, B6, Q0, etc.) — skip POS reports which are already decoded by backend
      decodedText = decodeArinc620(msg.msg_text);
    }
    const fullText  = decodedText ? esc(decodedText) : esc(displayText);
    const shortText = !decodedText && displayText.length > 120
      ? esc(displayText.slice(0, 120)) + '…'
      : fullText;
    return `<tr class="msg-row${weatherCls}">
      <td class="mono dim">${fmtTime(msg.time)}</td>
      <td class="mono">${esc(msg.reg) || '—'}</td>
      <td class="mono">${esc(msg.flight) || '—'}</td>
      <td class="msg-label-cell">${acarsLabelName(msg.label)}</td>
      <td class="msg-sublabel-cell dim">${acarsSubLabelName(msg.sublabel)}</td>
      <td class="dim">${esc(gsName)}</td>
      <td class="mono dim">${msg.freq_khz ? msg.freq_khz.toLocaleString() : '—'}</td>
      <td class="msg-text-cell"><span class="msg-text-content" title="${fullText}">${shortText || '<em class="dim">—</em>'}</span></td>
    </tr>`;
  }).join('');
}

function addMessageEntry(msg) {
  if (!msg.msg_text && !msg.label) return; // only store ACARS frames
  messagesStore.unshift(msg);
  if (messagesStore.length > MAX_MESSAGES_ROWS) messagesStore.length = MAX_MESSAGES_ROWS;
  if (document.getElementById('tab-messages').classList.contains('active')) {
    renderMessagesTable();
  }
}

function initMessagesTab() {
  const regEl    = document.getElementById('msg-filter-reg');
  const flightEl = document.getElementById('msg-filter-flight');
  const labelEl  = document.getElementById('msg-filter-label');
  const clearEl  = document.getElementById('msg-filter-clear');
  if (!regEl) return;

  function applyFilter() {
    msgFilter.reg    = regEl.value.trim().toLowerCase();
    msgFilter.flight = flightEl.value.trim().toLowerCase();
    msgFilter.label  = labelEl.value.trim().toLowerCase();
    renderMessagesTable();
  }
  regEl.addEventListener('input', applyFilter);
  flightEl.addEventListener('input', applyFilter);
  labelEl.addEventListener('input', applyFilter);
  clearEl.addEventListener('click', () => {
    regEl.value = flightEl.value = labelEl.value = '';
    msgFilter.reg = msgFilter.flight = msgFilter.label = '';
    renderMessagesTable();
  });
}

// ---- Events tab (Phase 1c) -------------------------------------------------

const evtFilter = { type: '', reg: '', flight: '' };

function evtMatchesFilter(evt) {
  if (evtFilter.type && evt._evtType !== evtFilter.type) return false;
  if (evtFilter.reg) {
    const t = evtFilter.reg;
    if (!(evt.reg || '').toLowerCase().includes(t) &&
        !(evt.src_icao || '').toLowerCase().includes(t) &&
        !(evt.icao || '').toLowerCase().includes(t)) return false;
  }
  if (evtFilter.flight && !(evt.flight || '').toLowerCase().includes(evtFilter.flight)) return false;
  return true;
}

function evtRowClass(evt) {
  switch (evt._evtType) {
    case 'logon':    return ' evt-row--logon';
    case 'logoff':   return ' evt-row--logoff';
    case 'gs_event': return ' evt-row--gs';
    default:         return '';
  }
}

function evtTypeLabel(evt) {
  switch (evt._evtType) {
    case 'logon':    return '🟢 Logon';
    case 'logoff':   return '🔴 Logoff';
    case 'gs_event': return '⚠ GS Event';
    default:         return esc(evt._evtType || 'Frame');
  }
}

function evtActor(evt) {
  if (evt._evtType === 'gs_event') {
    return esc(evt.location || `GS ${evt.gs_id}`);
  }
  // Use reg/flight/icao if available, then src_icao from slot map, then slot ID
  const parts = [esc(evt.reg), esc(evt.flight), evt.icao ? `(${esc(evt.icao)})` : ''].filter(Boolean);
  if (parts.length > 0) return parts.join(' ');
  if (evt.src_icao) return esc(evt.src_icao);
  return evt.src_id ? `Aircraft ${evt.src_id}` : '—';
}

function evtDetail(evt) {
  if (evt._evtType === 'gs_event') return esc(evt.change_note || '');
  if (evt._evtType === 'logon') {
    const id = evt.assigned_ac_id ? `Assigned ID ${evt.assigned_ac_id}` : '';
    return id || 'Logon confirmed';
  }
  if (evt._evtType === 'logoff') {
    // reason_descr comes from lpdu.reason.descr (Section 2.3).
    // Code 6 = "Other" is the standard aircraft-initiated logoff — suppress it
    // as it's always present and adds no information. Only show non-trivial reasons.
    const r = evt.reason_descr || evt.reason || '';
    return (r && r !== 'Other' && r !== 'Reserved') ? esc(r) : '';
  }
  return esc(evt.msg_type || '');
}

function evtGS(evt) {
  // For gs_event the actor IS the GS — no separate GS column needed
  if (evt._evtType === 'gs_event') return '—';
  // For logon/logoff the destination is the ground station
  if (evt.dst_type === 'Ground station' && evt.dst_id) {
    return esc(gsNames[evt.dst_id] || `GS ${evt.dst_id}`);
  }
  // Fallback: src may be the GS (downlink frames)
  if (evt.src_type === 'Ground station' && evt.src_id) {
    return esc(gsNames[evt.src_id] || `GS ${evt.src_id}`);
  }
  return '—';
}

function renderEventsTable() {
  const tbody = document.getElementById('events-tbody');
  if (!tbody) return;
  const filtered = eventsStore.filter(evtMatchesFilter);
  const countEl = document.getElementById('evt-count-label');
  if (countEl) countEl.textContent = `${filtered.length} / ${eventsStore.length}`;
  if (filtered.length === 0) {
    tbody.innerHTML = '<tr><td colspan="6" class="empty">No events match the filter…</td></tr>';
    return;
  }
  tbody.innerHTML = filtered.map(evt => {
    const freq = evt.freq_khz ? evt.freq_khz.toLocaleString() : '—';
    return `<tr class="evt-row${evtRowClass(evt)}">
      <td class="mono dim">${fmtTime(evt.time)}</td>
      <td>${evtTypeLabel(evt)}</td>
      <td>${evtActor(evt)}</td>
      <td class="dim">${evtGS(evt)}</td>
      <td class="mono dim">${freq}</td>
      <td class="dim">${evtDetail(evt)}</td>
    </tr>`;
  }).join('');
}

function addEventEntry(type, data) {
  const entry = Object.assign({ _evtType: type }, data);
  eventsStore.unshift(entry);
  if (eventsStore.length > MAX_EVENTS_ROWS) eventsStore.length = MAX_EVENTS_ROWS;
  if (document.getElementById('tab-events').classList.contains('active')) {
    renderEventsTable();
  }
}

function initEventsTab() {
  const typeEl   = document.getElementById('evt-filter-type');
  const regEl    = document.getElementById('evt-filter-reg');
  const flightEl = document.getElementById('evt-filter-flight');
  const clearEl  = document.getElementById('evt-filter-clear');
  if (!typeEl) return;

  typeEl.addEventListener('change', () => {
    evtFilter.type = typeEl.value;
    renderEventsTable();
  });
  if (regEl) regEl.addEventListener('input', () => {
    evtFilter.reg = regEl.value.trim().toLowerCase();
    renderEventsTable();
  });
  if (flightEl) flightEl.addEventListener('input', () => {
    evtFilter.flight = flightEl.value.trim().toLowerCase();
    renderEventsTable();
  });
  clearEl.addEventListener('click', () => {
    typeEl.value = '';
    if (regEl)    regEl.value    = '';
    if (flightEl) flightEl.value = '';
    evtFilter.type = evtFilter.reg = evtFilter.flight = '';
    renderEventsTable();
  });
}

// ---- Toast notification system (Phase 1c) ----------------------------------

const toastLog = []; // last 10 gs_events for the bell log
const MAX_TOAST_LOG = 10;
let toastBellOpen = false;

function showToast(msg, durationMs = 30000) {
  const container = document.getElementById('toast-container');
  if (!container) return;
  const toast = document.createElement('div');
  toast.className = 'toast toast--new';
  toast.innerHTML =
    `<span class="toast-msg">${esc(msg)}</span>` +
    `<button class="toast-close" aria-label="Dismiss">✕</button>`;
  toast.querySelector('.toast-close').addEventListener('click', () => toast.remove());
  container.appendChild(toast);
  // Remove the animation class after it plays
  requestAnimationFrame(() => toast.classList.remove('toast--new'));
  setTimeout(() => { if (toast.parentNode) toast.remove(); }, durationMs);
}

function handleGSEvent(data) {
  const loc = data.location || `GS ${data.gs_id}`;
  const msg = `⚠ ${loc}: ${data.change_note}`;
  showToast(msg);
  toastLog.unshift({ time: data.time, msg });
  if (toastLog.length > MAX_TOAST_LOG) toastLog.length = MAX_TOAST_LOG;
  addEventEntry('gs_event', data);
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

      // Phase 1d: show dumphfdl version in the status bar
      if (data.dumphfdl_ver) {
        let verEl = document.getElementById('dumphfdl-ver-label');
        if (!verEl) {
          verEl = document.createElement('span');
          verEl.id = 'dumphfdl-ver-label';
          verEl.className = 'dumphfdl-ver';
          document.getElementById('status-bar').appendChild(verEl);
        }
        let verText = 'dumphfdl ' + data.dumphfdl_ver;
        if (data.systable_version) verText += ' · systable v' + data.systable_version;
        verEl.textContent = verText;
      }

      renderFreqTable(data.frequencies);
      if (data.aircraft_history) renderPlanesActivityChart(data.aircraft_history);

      // Build heardFreqsByGS from per-GS stats on each frequency.
      // activeFreqsKHz is only ever added to (never reset) so that frequencies
      // seen via SSE events are not lost when a stats refresh arrives.
      heardFreqsByGS = {};
      if (Array.isArray(data.frequencies)) {
        for (const freq of data.frequencies) {
          // Any entry in the frequencies array means at least one message was
          // received on that frequency, regardless of source type.
          activeFreqsKHz.add(freq.freq_khz);
          // Sum msg_count across all GS for this frequency
          let total = 0;
          if (freq.gs_stats) {
            for (const gs of Object.values(freq.gs_stats)) {
              total += gs.msg_count || 0;
            }
            for (const gsIdStr of Object.keys(freq.gs_stats)) {
              const gsId = parseInt(gsIdStr, 10);
              if (!heardFreqsByGS[gsId]) heardFreqsByGS[gsId] = new Set();
              heardFreqsByGS[gsId].add(freq.freq_khz);
            }
          }
          // Only update if the authoritative count from /stats is higher
          // (SSE increments may have run ahead; take the max).
          const prev = freqMsgCounts.get(freq.freq_khz) || 0;
          freqMsgCounts.set(freq.freq_khz, Math.max(prev, total));
        }
      }
      // Always fetch ground stations after stats so heardFreqsByGS is ready
      loadGroundStations();
      // Re-render instances tab so activeFreqsKHz highlights are up to date
      if (cachedInstancesData) renderInstances(cachedInstancesData);

      if (data.recent && data.recent.length > 0) {
        // Pre-populate Live Feed backing store (newest-first) then render
        if (feedStore.length === 0) {
          for (let i = data.recent.length - 1; i >= 0; i--) {
            feedStore.push(data.recent[i]);
          }
          renderFeedTable();
        }

        // Seed Messages and Events stores from the ring buffer on first load only.
        // Use a flag rather than a length check so that early SSE arrivals don't
        // prevent seeding (SSE fires before /stats resolves on a fast connection).
        if (!_storesSeeded) {
          _storesSeeded = true;

          // Messages tab — seed with ACARS frames (newest-first)
          // data.recent is oldest-first, so iterate in reverse
          for (let i = data.recent.length - 1; i >= 0; i--) {
            const msg = data.recent[i];
            if (msg.msg_text || msg.label) {
              messagesStore.push(msg);
              if (messagesStore.length >= MAX_MESSAGES_ROWS) break;
            }
          }
          renderMessagesTable();

          // Events tab — seed with logon/logoff entries from data.recent (newest-first)
          for (let i = data.recent.length - 1; i >= 0; i--) {
            const msg = data.recent[i];
            let evtType = null;
            if (msg.msg_type === 'Logon confirm') {
              evtType = 'logon';
            } else if (msg.msg_type === 'Logoff request' || msg.msg_type === 'Logon denied') {
              evtType = 'logoff';
            }
            if (evtType) {
              eventsStore.push(Object.assign({ _evtType: evtType }, msg));
              if (eventsStore.length >= MAX_EVENTS_ROWS) break;
            }
          }
          // Also seed gs_event entries from the dedicated ring buffer (newest-first)
          if (data.recent_events && data.recent_events.length > 0) {
            for (let i = data.recent_events.length - 1; i >= 0; i--) {
              eventsStore.push(Object.assign({ _evtType: 'gs_event' }, data.recent_events[i]));
              if (eventsStore.length >= MAX_EVENTS_ROWS) break;
            }
            // Re-sort by time descending so logon/logoff and gs_events are interleaved correctly
            eventsStore.sort((a, b) => (b.time || 0) - (a.time || 0));
          }
          renderEventsTable();
        }
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
      freqMsgCounts.set(data.freq_khz, (freqMsgCounts.get(data.freq_khz) || 0) + 1);
      if (cachedInstancesData) renderInstances(cachedInstancesData);
      // Feed the live-activity overlay on the map
      if (typeof recordBandActivity === 'function') {
        recordBandActivity(data.freq_khz);
      }
    }
    // Phase 1b: feed Messages tab if this frame has ACARS content
    if (data.msg_text || data.label) {
      addMessageEntry(data);
    }
    // Phase 1c: feed Events tab for logon / logoff frames
    if (data.msg_type === 'Logon confirm') {
      addEventEntry('logon', data);
    } else if (data.msg_type === 'Logoff request' || data.msg_type === 'Logon denied') {
      addEventEntry('logoff', data);
    }

  } else if (type === 'position') {
    // Update local store and re-render planes table if visible
    aircraftStore[data.key] = data;
    if (document.getElementById('tab-planes').classList.contains('active')) {
      renderAircraftTable();
    }
    // Record link quality sample for the LQ tab sparklines
    if (typeof recordLQSample === 'function') recordLQSample(data);
    // Re-render LQ tab if visible
    if (document.getElementById('tab-lq') && document.getElementById('tab-lq').classList.contains('active')) {
      if (typeof renderLinkQualityTab === 'function') renderLinkQualityTab();
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

  } else if (type === 'gs_event') {
    // Phase 1c: ground station state change — show toast and log to Events tab
    handleGSEvent(data);
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

// ---- Planes activity chart -------------------------------------------------

function renderPlanesActivityChart(buckets) {
  const container = document.getElementById('planes-activity-chart');
  if (!container) return;

  if (!Array.isArray(buckets) || buckets.length === 0) {
    container.innerHTML = '';
    return;
  }

  const maxCount = Math.max(...buckets.map(b => b.n));
  if (maxCount === 0) {
    container.innerHTML = '';
    return;
  }

  // Build bars oldest-left → newest-right
  const bars = buckets.map(b => {
    const heightPct = Math.max(4, Math.round((b.n / maxCount) * 100));
    const intensity = b.n / maxCount; // 0..1
    const alpha = 0.15 + intensity * 0.85; // 0.15 → 1.0
    const barColor = `rgba(88,166,255,${alpha.toFixed(2)})`;

    const d = new Date(b.t * 1000);
    const hh = String(d.getUTCHours()).padStart(2, '0');
    const mm = String(d.getUTCMinutes()).padStart(2, '0');
    const endD = new Date((b.t + 1800) * 1000);
    const ehh = String(endD.getUTCHours()).padStart(2, '0');
    const emm = String(endD.getUTCMinutes()).padStart(2, '0');
    const label = `${hh}:${mm}–${ehh}:${emm} UTC · ${b.n} aircraft`;

    return `<div class="planes-activity-bar" title="${label}" style="height:${heightPct}%;background:${barColor};"></div>`;
  }).join('');

  // Y-axis: 3 ticks — round the top tick up to a clean number so the axis
  // doesn't look like it contradicts the live aircraft count in the table.
  // e.g. maxCount=9 → topTick=10, maxCount=36 → topTick=40
  function niceMax(n) {
    if (n <= 0) return 1;
    const mag = Math.pow(10, Math.floor(Math.log10(n)));
    const steps = [1, 2, 5, 10];
    for (const s of steps) {
      const candidate = Math.ceil(n / (mag * s)) * (mag * s);
      if (candidate >= n) return candidate;
    }
    return Math.ceil(n / mag) * mag;
  }
  const topTick = niceMax(maxCount);
  const midTick = Math.round(topTick / 2);
  const axis =
    `<div class="planes-activity-axis">` +
      `<span class="planes-activity-tick">${topTick}</span>` +
      `<span class="planes-activity-tick">${midTick}</span>` +
      `<span class="planes-activity-tick">0</span>` +
    `</div>`;

  // Guide lines at 50% and 100% from bottom (top = 0%, bottom = 100%)
  const guides =
    `<div class="planes-activity-guide" style="top:0%"></div>` +
    `<div class="planes-activity-guide" style="top:50%"></div>`;

  container.innerHTML =
    axis +
    `<div class="planes-activity-bars-wrap">` +
      guides +
      `<div class="planes-activity-bars">${bars}</div>` +
    `</div>`;
}

// ---- Planes tab ------------------------------------------------------------

// Returns a datalink icon + label for the current_link code.
function datalinkLabel(code) {
  if (!code) return '';
  switch (code.toUpperCase()) {
    case 'HF':     return '📻 HF';
    case 'VHF':    return '📶 VHF';
    case 'SATCOM': return '🛰 SAT';
    default:       return esc(code);
  }
}

// Returns a colour class for an error rate percentage.
function errRateClass(pct) {
  if (pct >= 20) return 'err-high';
  if (pct >= 5)  return 'err-mid';
  return 'err-low';
}

// Planes tab filter state
let planesFilterTerm = '';

function initPlanesFilter() {
  const filterEl = document.getElementById('planes-filter');
  const clearEl  = document.getElementById('planes-filter-clear');
  if (!filterEl) return;
  filterEl.addEventListener('input', () => {
    planesFilterTerm = filterEl.value.trim().toLowerCase();
    renderAircraftTable();
  });
  clearEl.addEventListener('click', () => {
    filterEl.value = '';
    planesFilterTerm = '';
    renderAircraftTable();
  });
}

function renderAircraftTable() {
  const tbody = document.getElementById('planes-tbody');
  let list = Object.values(aircraftStore);

  // Apply filter
  if (planesFilterTerm) {
    list = list.filter(ac =>
      (ac.icao   || '').toLowerCase().includes(planesFilterTerm) ||
      (ac.reg    || '').toLowerCase().includes(planesFilterTerm) ||
      (ac.flight || '').toLowerCase().includes(planesFilterTerm)
    );
  }

  const countEl = document.getElementById('planes-count-label');
  if (countEl) {
    const total = Object.keys(aircraftStore).length;
    countEl.textContent = planesFilterTerm ? `${list.length} / ${total}` : `${total}`;
  }

  if (list.length === 0) {
    tbody.innerHTML = `<tr><td colspan="13" class="empty">${planesFilterTerm ? 'No aircraft match the filter…' : 'No aircraft seen yet…'}</td></tr>`;
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
    // Phase 3c: datalink — show all available links with current in bold
    const cur = ac.current_link ? ac.current_link.toUpperCase() : null;
    let dlCell;
    if (ac.available_links && ac.available_links.length > 0) {
      // Build a set of all links: available_links union current (in case current isn't listed)
      const allLinks = [...ac.available_links];
      if (cur && !allLinks.map(l => l.toUpperCase()).includes(cur)) {
        allLinks.unshift(ac.current_link);
      }
      dlCell = allLinks.map(l =>
        l.toUpperCase() === cur
          ? `<strong>${esc(l)}</strong>`
          : esc(l)
      ).join(', ');
    } else {
      dlCell = cur ? `<strong>${esc(ac.current_link)}</strong>` : '—';
    }
    // Phase 3b: link quality
    let lqCell = '—';
    if (ac.error_rate != null && (ac.mpdu_rx || ac.mpdu_tx)) {
      const cls = errRateClass(ac.error_rate);
      lqCell = `<span class="err-rate ${cls}">${ac.error_rate.toFixed(1)}%</span>`;
    }
    // Phase 3a: freq change cause (truncated)
    const fcc = ac.last_freq_change_cause
      ? `<span title="${esc(ac.last_freq_change_cause)}">${esc(ac.last_freq_change_cause.slice(0, 20))}${ac.last_freq_change_cause.length > 20 ? '…' : ''}</span>`
      : '—';
    return `<tr>
      <td class="mono">${esc(ac.icao) || '—'}</td>
      <td class="mono">${esc(ac.reg)  || '—'}</td>
      <td class="mono">${esc(ac.flight) || '—'}</td>
      <td class="mono dim">${lat}</td>
      <td class="mono dim">${lon}</td>
      <td class="mono">${freq}</td>
      <td>${esc(gsName)}</td>
      <td class="mono">${dlCell}</td>
      <td class="mono ${sigCls}">${sig !== '—' ? sig + ' dBFS' : '—'}</td>
      <td>${lqCell}</td>
      <td class="dim">${fcc}</td>
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
     const heardSet  = heardFreqsByGS[gs.gs_id];
     const dstSet    = dstFreqsByGS[gs.gs_id];
     const activeSet = gs.active_freqs_khz ? new Set(gs.active_freqs_khz) : new Set();
     const freqs = (gs.frequencies || [])
       .map(f => {
         const isDisabled  = f.enabled === false;
         const isHeard     = !isDisabled && heardSet && heardSet.has(f.freq_khz);
         const isDst       = !isDisabled && !isHeard && dstSet && dstSet.has(f.freq_khz);
         const isNetActive = !isDisabled && activeSet.has(f.freq_khz);
         const cls = isDisabled ? ' gs-freq--disabled'
                   : isHeard   ? ' gs-freq--heard'
                   : isDst     ? ' gs-freq--dst'
                   : '';
         // Phase 2c: dot indicator if SPDU advertises this freq as active but not yet heard
         const netDot = isNetActive && !isHeard
           ? '<span class="gs-freq-net-dot" title="Advertised active in SPDU">·</span>'
           : '';
         return `<span class="gs-freq${cls}">${f.freq_khz.toLocaleString()}${netDot}<span class="gs-slot">T${f.timeslot}</span></span>`;
       })
       .join('');
    const heard = gs.last_heard > 0;
    const heardBadge = heard
      ? `<span class="gs-heard" title="Last heard ${fmtDateTime(gs.last_heard)}">● heard ${fmtTime(gs.last_heard)}</span>`
      : '';
    // Phase 2c: SPDU network active badge
    const spduBadge = gs.spdu_active
      ? `<span class="gs-spdu-badge gs-spdu-badge--active" title="Last SPDU: ${fmtDateTime(gs.spdu_last_seen)}">📡 SPDU</span>`
      : '';
    // Phase 2c: UTC sync indicator
    const syncBadge = gs.spdu_last_seen
      ? (gs.utc_sync
          ? `<span class="gs-sync-badge gs-sync-badge--ok" title="UTC synchronised">✓ UTC</span>`
          : `<span class="gs-sync-badge gs-sync-badge--bad" title="Not UTC synchronised">✗ UTC</span>`)
      : '';
    // Phase 4b: "Heard by N aircraft" from propagation data
    const heardByCount = propHeardByGS[gs.gs_id] || 0;
    const heardByBadge = heardByCount > 0
      ? `<span class="gs-heardby-badge" title="Heard by ${heardByCount} aircraft (from Frequency Data reports)">👂 ${heardByCount} ac</span>`
      : '';
    // Section 7.3: active slots row — green chips for SPDU-advertised active freqs
    // that are NOT already shown as heard (to avoid duplication).
    // Only show if freq > 0 (system table loaded) and not already in configured list.
    let activeSlotsRow = '';
    if (gs.active_freqs_khz && gs.active_freqs_khz.length > 0) {
      const configuredKHz = new Set((gs.frequencies || []).map(f => f.freq_khz));
      const extraActive = gs.active_freqs_khz.filter(k => k > 0 && !configuredKHz.has(k));
      if (extraActive.length > 0) {
        const chips = extraActive.map(k =>
          `<span class="gs-freq gs-freq--active-slot">${k.toLocaleString()}</span>`
        ).join('');
        activeSlotsRow = `<div class="gs-active-slots"><span class="gs-active-slots__label">SPDU active:</span>${chips}</div>`;
      }
    }
    // heardBadge goes on the top row (primary status).
    // SPDU/UTC/heard-by go on the secondary badges row.
    const hasSecondaryBadges = !!(gs.spdu_active || gs.spdu_last_seen || heardByCount > 0);
    return `<div class="gs-card${heard ? ' gs-card--heard' : ''}${gs.spdu_active ? ' gs-card--spdu' : ''}">
      <div class="gs-card-header">
        <div class="gs-card-header__top">
          <span class="gs-id">GS ${gs.gs_id}</span>
          <span class="gs-location">${esc(gs.location)}</span>
          ${heardBadge}
        </div>
        ${hasSecondaryBadges ? `<div class="gs-card-header__badges">${spduBadge}${syncBadge}${heardByBadge}</div>` : ''}
      </div>
      <div class="gs-freqs">${freqs || '<span class="gs-no-freqs">No frequencies</span>'}</div>
      ${activeSlotsRow}
    </div>`;
  }).join('');
}

// Phase 4b: gs_id → number of aircraft that reported hearing this GS
let propHeardByGS = {}; // populated by loadGroundStations()

function loadGroundStations() {
  // Fetch both /groundstations and /propagation in parallel so we can show
  // "Heard by N aircraft" counts on each GS card.
  Promise.all([
    fetch('/groundstations').then(r => r.json()),
    fetch('/propagation').then(r => r.json()).catch(() => null),
  ])
    .then(([data, propSnap]) => {
      // Build dstFreqsByGS from the dst_freqs_khz field on each station
      dstFreqsByGS = {};
      for (const gs of data) {
        if (Array.isArray(gs.dst_freqs_khz) && gs.dst_freqs_khz.length > 0) {
          dstFreqsByGS[gs.gs_id] = new Set(gs.dst_freqs_khz);
        }
      }
      // Phase 4b: build heard-by counts from propagation snapshot
      propHeardByGS = {};
      if (propSnap && propSnap.by_gs) {
        for (const [gsIdStr, acKeys] of Object.entries(propSnap.by_gs)) {
          propHeardByGS[parseInt(gsIdStr, 10)] = acKeys.length;
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

// fmtUptimeShort formats elapsed seconds as "Xh Ym Zs", omitting leading zeros.
function fmtUptimeShort(secs) {
  if (secs < 0) secs = 0;
  const h = Math.floor(secs / 3600);
  const m = Math.floor((secs % 3600) / 60);
  const s = secs % 60;
  if (h > 0) return `${h}h ${m}m ${s}s`;
  if (m > 0) return `${m}m ${s}s`;
  return `${s}s`;
}

function renderInstances(data) {
  const extraEl = document.getElementById('instances-extra-args');
  const windowsEl = document.getElementById('instances-windows');
  if (!extraEl || !windowsEl) return;

  // Show the Apply toolbar always, but swap content based on apply_enabled.
  const applyToolbar = document.getElementById('instances-apply-toolbar');
  if (applyToolbar) {
    applyToolbar.style.display = '';
    const applyButtons = document.getElementById('instances-apply-buttons');
    const applyHint    = document.getElementById('instances-apply-hint');
    if (data.apply_enabled) {
      if (applyButtons) applyButtons.style.display = '';
      if (applyHint)    applyHint.style.display    = 'none';
    } else {
      if (applyButtons) applyButtons.style.display = 'none';
      if (applyHint)    applyHint.style.display    = '';
    }
  }

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

  // Frequency overview stats
  const windows = Array.isArray(data.windows) ? data.windows : [];
  const disabledFreqs = Array.isArray(data.disabled_freqs) ? data.disabled_freqs : [];

  // Collect all configured frequencies across all windows
  const allConfiguredFreqs = new Set();
  for (const w of windows) {
    for (const f of (w.freqs_khz || [])) {
      allConfiguredFreqs.add(f);
    }
  }

  const totalConfigured = allConfiguredFreqs.size;
  const totalDisabled   = disabledFreqs.length;
  const totalEnabled    = totalConfigured; // windows only contain enabled freqs
  const totalActive     = [...allConfiguredFreqs].filter(f => activeFreqsKHz.has(f)).length;
  const totalInactive   = totalEnabled - totalActive;

  let overviewHtml = `<div class="instances-section-title">Frequency Overview</div>`;
  overviewHtml += `<div class="freq-overview">`;
  overviewHtml += `<div class="freq-overview__stat">
    <span class="freq-overview__value">${(totalEnabled + totalDisabled).toLocaleString()}</span>
    <span class="freq-overview__label">Total</span>
  </div>`;
  overviewHtml += `<div class="freq-overview__stat freq-overview__stat--active">
    <span class="freq-overview__value">${totalActive.toLocaleString()}</span>
    <span class="freq-overview__label">Active</span>
  </div>`;
  overviewHtml += `<div class="freq-overview__stat freq-overview__stat--inactive">
    <span class="freq-overview__value">${totalInactive.toLocaleString()}</span>
    <span class="freq-overview__label">Inactive</span>
  </div>`;
  overviewHtml += `<div class="freq-overview__stat freq-overview__stat--disabled">
    <span class="freq-overview__value">${totalDisabled.toLocaleString()}</span>
    <span class="freq-overview__label">Disabled</span>
  </div>`;
  if (totalEnabled > 0) {
    const pct = Math.round((totalActive / totalEnabled) * 100);
    overviewHtml += `<div class="freq-overview__bar-wrap">
      <div class="freq-overview__bar-track">
        <div class="freq-overview__bar-fill" style="width:${pct}%"></div>
      </div>
      <span class="freq-overview__bar-label">${pct}% of enabled frequencies active this session</span>
    </div>`;

    // Show a contextual hint based on uptime and active-frequency percentage.
    const uptimeSecs = startTimeSec ? Math.max(0, Math.floor(Date.now() / 1000) - startTimeSec) : 0;
    if (uptimeSecs <= 86400) {
      // Not yet 24 hours — tell the user to come back later
      overviewHtml += `<div class="freq-overview__notice freq-overview__notice--info">
        <span class="freq-overview__notice-icon">ℹ</span>
        <span class="freq-overview__notice-text">
          Once the service has been running for <strong>24 hours</strong>, optimisation suggestions will be displayed here.
        </span>
      </div>`;
    } else if (pct < 70) {
      // > 24 h uptime and fewer than 70% active — suggest optimising
      overviewHtml += `<div class="freq-overview__notice freq-overview__notice--warn">
        <span class="freq-overview__notice-icon">⚠</span>
        <span class="freq-overview__notice-text">
          Only <strong>${pct}%</strong> of enabled frequencies have been active after
          more than 24 hours of uptime. Running
          <strong>↺ Apply Active Frequencies</strong> could reduce unnecessary CPU usage
          by disabling the ${totalInactive.toLocaleString()} inactive
          frequenc${totalInactive === 1 ? 'y' : 'ies'}.
        </span>
      </div>`;
    } else {
      // > 24 h uptime and ≥ 70% active — looking efficient
      overviewHtml += `<div class="freq-overview__notice freq-overview__notice--ok">
        <span class="freq-overview__notice-icon">✓</span>
        <span class="freq-overview__notice-text">
          <strong>${pct}%</strong> of enabled frequencies are active — your frequency
          configuration looks efficient enough. No changes are needed.
        </span>
      </div>`;
    }
  }

  // Per-MHz message count chart — group freqMsgCounts by MHz band (same logic
  // as map.js freqBand(): Math.floor(freqKhz / 1000)).
  if (freqMsgCounts.size > 0) {
    // Aggregate message counts per MHz band
    const bandCounts = new Map(); // band (int MHz) → total messages
    for (const [freqKhz, count] of freqMsgCounts) {
      if (count <= 0) continue;
      const band = Math.floor(freqKhz / 1000);
      bandCounts.set(band, (bandCounts.get(band) || 0) + count);
    }

    if (bandCounts.size > 0) {
      const sortedBands = [...bandCounts.entries()].sort((a, b) => a[0] - b[0]);
      const maxCount = Math.max(...sortedBands.map(([, c]) => c));

      overviewHtml += `<div class="freq-overview__mhz-chart-wrap">`;
      overviewHtml += `<div class="freq-overview__mhz-chart-title">Messages per MHz band</div>`;
      overviewHtml += `<div class="freq-overview__mhz-chart">`;
      for (const [band, count] of sortedBands) {
        const widthPct = maxCount > 0 ? Math.max(1, Math.round((count / maxCount) * 100)) : 0;
        overviewHtml +=
          `<div class="freq-overview__mhz-row">` +
            `<span class="freq-overview__mhz-label">${band} MHz</span>` +
            `<div class="freq-overview__mhz-bar-track">` +
              `<div class="freq-overview__mhz-bar-fill" style="width:${widthPct}%"></div>` +
            `</div>` +
            `<span class="freq-overview__mhz-count">${count.toLocaleString()}</span>` +
          `</div>`;
      }
      overviewHtml += `</div>`; // .freq-overview__mhz-chart
      overviewHtml += `</div>`; // .freq-overview__mhz-chart-wrap
    }
  }

  overviewHtml += `</div>`;

  // Windows section
  if (windows.length === 0) {
    windowsEl.innerHTML = overviewHtml + `<div class="instances-section-title">IQ Windows</div><p class="empty">No windows configured.</p>`;
    return;
  }

  const nowSec = Math.floor(Date.now() / 1000);

  // Reset the started-at map so stale entries don't linger after a re-render.
  for (const k of Object.keys(instanceStartedAt)) delete instanceStartedAt[k];

  let html = overviewHtml + `<div class="instances-section-title">IQ Windows (${windows.length})</div>`;
  html += `<div class="instances-grid">`;
  for (let i = 0; i < windows.length; i++) {
    const w = windows[i];
    const freqs = (w.freqs_khz || []).map(f => {
      const active = activeFreqsKHz.has(f);
      const count = freqMsgCounts.get(f) || 0;
      const countLabel = ` (${count.toLocaleString()})`;
      return `<span class="instances-freq${active ? ' instances-freq--active' : ''}">${f.toLocaleString()}${countLabel}</span>`;
    }).join('');

    // Sum message counts for all frequencies in this window
    const windowTotal = (w.freqs_khz || []).reduce((sum, f) => sum + (freqMsgCounts.get(f) || 0), 0);

    // Store started_at so the 1-second ticker can update the uptime span live.
    instanceStartedAt[i] = (w.running && w.started_at) ? w.started_at : 0;

    // Health status badge
    let statusHtml;
    if (w.running) {
      const uptimeSecs = w.started_at ? nowSec - w.started_at : 0;
      statusHtml =
        `<span class="inst-status inst-status--running">● Running</span>` +
        `<span class="inst-uptime" id="inst-uptime-${i}">up ${fmtUptimeShort(uptimeSecs)}</span>`;
    } else if (w.last_healthy_at) {
      statusHtml =
        `<span class="inst-status inst-status--stopped">✗ Stopped</span>` +
        `<span class="inst-uptime inst-uptime--dim">last healthy ${fmtDateTime(w.last_healthy_at)}</span>`;
    } else {
      statusHtml = `<span class="inst-status inst-status--stopped">✗ Not started</span>`;
    }

    // Reconnection badge (only shown when > 0)
    const reconnectHtml = w.reconnections > 0
      ? `<span class="inst-reconnect-badge" title="Pipeline has restarted ${w.reconnections} time${w.reconnections === 1 ? '' : 's'}">` +
          `⚠ ${w.reconnections} reconnection${w.reconnections === 1 ? '' : 's'}` +
        `</span>`
      : '';

    html +=
      `<div class="instances-card">` +
        `<div class="instances-card__header">` +
          `<span class="instances-card__centre">${w.center_khz.toLocaleString()} kHz (${windowTotal.toLocaleString()})</span>` +
          `<span class="instances-card__mode">${esc(w.iq_mode)} · ${w.bandwidth_khz} kHz BW</span>` +
        `</div>` +
        `<div class="instances-card__health">` +
          `<div class="inst-status-row">${statusHtml}</div>` +
          (reconnectHtml ? `<div class="inst-reconnect-row">${reconnectHtml}</div>` : '') +
        `</div>` +
        `<div class="instances-card__freqs">${freqs}</div>` +
      `</div>`;
  }
  html += `</div>`;

  // Disabled frequencies — shown as a flat list of red chips below the windows
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
    // Destroy all existing charts before clearing the DOM
    for (const [key, ch] of Object.entries(signalCharts)) {
      ch.destroy();
      delete signalCharts[key];
    }
    container.innerHTML = '<p id="signal-empty">No signal history yet — data accumulates in 30-minute buckets.</p>';
    return;
  }

  // Remove stale empty message if present
  const empty = container.querySelector('#signal-empty');
  if (empty) empty.remove();

  // Build a set of keys present in the new response so we can detect stale charts
  const incomingKeys = new Set();

  // Group series by gs_id, preserving insertion order
  const groups = new Map(); // gs_id → { location, items: [s, …] }
  for (const s of series) {
    if (!groups.has(s.gs_id)) {
      groups.set(s.gs_id, { location: s.location, items: [] });
    }
    groups.get(s.gs_id).items.push(s);
    incomingKeys.add(`${s.gs_id}:${s.freq_khz}`);
  }

  // Destroy and remove any charts whose GS/frequency is no longer in the response
  for (const [key, ch] of Object.entries(signalCharts)) {
    if (!incomingKeys.has(key)) {
      ch.destroy();
      delete signalCharts[key];
      // Remove the card element from the DOM
      const card = container.querySelector(`.sig-chart-card[data-key="${CSS.escape(key)}"]`);
      if (card) card.remove();
    }
  }

  // Remove group sections that are now empty (all their charts were removed)
  container.querySelectorAll('.sig-group').forEach(groupEl => {
    if (groupEl.querySelector('.sig-group__grid').children.length === 0) {
      groupEl.remove();
    }
  });

  for (const [gsId, group] of groups) {
    // Ensure a group section exists for this GS
    const groupId = `sig-group-${gsId}`;
    let groupEl = container.querySelector(`#${groupId}`);
    if (!groupEl) {
      groupEl = document.createElement('div');
      groupEl.className = 'sig-group';
      groupEl.id = groupId;
      groupEl.innerHTML =
        `<div class="sig-group__header">` +
          `<span class="sig-group__name">${esc(group.location)}</span>` +
          `<span class="sig-group__id">GS ${gsId}</span>` +
        `</div>` +
        `<div class="sig-group__grid"></div>`;
      container.appendChild(groupEl);
    }
    const grid = groupEl.querySelector('.sig-group__grid');

    for (const s of group.items) {
      const key = `${s.gs_id}:${s.freq_khz}`;
      const labels = s.buckets.map(b => {
        const d = new Date(b.t * 1000);
        return d.toUTCString().slice(17, 22); // "HH:MM"
      });
      const avgData  = s.buckets.map(b => b.avg);
      const minData  = s.buckets.map(b => b.min);
      const maxData  = s.buckets.map(b => b.max);
      const msgData  = s.buckets.map(b => b.n || 0);

      if (signalCharts[key]) {
        // Update existing chart in-place
        const ch = signalCharts[key];
        ch.data.labels = labels;
        ch.data.datasets[0].data = msgData;
        ch.data.datasets[1].data = avgData;
        ch.data.datasets[2].data = minData;
        ch.data.datasets[3].data = maxData;
        ch.update('none');
        continue;
      }

      // Create card + canvas inside the group grid
      const card = document.createElement('div');
      card.className = 'sig-chart-card';
      card.dataset.key = key;
      card.innerHTML =
        `<div class="sig-chart-card__title">${(s.freq_khz / 1000).toFixed(3)} MHz</div>` +
        `<canvas id="sig-canvas-${key.replace(':', '-')}"></canvas>`;
      grid.appendChild(card);

      const canvas = card.querySelector('canvas');
      const chart = new Chart(canvas, {
        type: 'bar',
        data: {
          labels,
          datasets: [
            {
              label: 'Msgs (30m)',
              data: msgData,
              type: 'bar',
              yAxisID: 'yMsg',
              backgroundColor: 'rgba(187,128,9,0.35)',
              borderColor: 'rgba(187,128,9,0.7)',
              borderWidth: 1,
              order: 2,
            },
            {
              label: 'Avg (dBFS)',
              data: avgData,
              type: 'line',
              yAxisID: 'ySig',
              borderColor: '#58a6ff',
              backgroundColor: 'rgba(88,166,255,0.12)',
              borderWidth: 2,
              pointRadius: 2,
              tension: 0.3,
              fill: false,
              order: 1,
            },
            {
              label: 'Min',
              data: minData,
              type: 'line',
              yAxisID: 'ySig',
              borderColor: '#f78166',
              borderWidth: 1,
              borderDash: [4, 3],
              pointRadius: 2,
              tension: 0.3,
              fill: false,
              order: 1,
            },
            {
              label: 'Max',
              data: maxData,
              type: 'line',
              yAxisID: 'ySig',
              borderColor: '#3fb950',
              borderWidth: 1,
              borderDash: [4, 3],
              pointRadius: 2,
              tension: 0.3,
              fill: false,
              order: 1,
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
                label: ctx => {
                  if (ctx.dataset.yAxisID === 'yMsg') {
                    return `${ctx.dataset.label}: ${ctx.parsed.y}`;
                  }
                  return `${ctx.dataset.label}: ${ctx.parsed.y.toFixed(1)} dBFS`;
                },
              },
            },
          },
          scales: {
            x: {
              ticks: { color: '#8b949e', font: { size: 10 }, maxRotation: 0 },
              grid:  { color: 'rgba(48,54,61,0.8)' },
            },
            ySig: {
              type: 'linear',
              position: 'left',
              ticks: {
                color: '#8b949e',
                font: { size: 10 },
                callback: v => v.toFixed(0) + ' dB',
              },
              grid: { color: 'rgba(48,54,61,0.8)' },
            },
            yMsg: {
              type: 'linear',
              position: 'right',
              beginAtZero: true,
              ticks: {
                color: '#bb8009',
                font: { size: 10 },
                precision: 0,
              },
              grid: { drawOnChartArea: false },
            },
          },
        },
      });
      signalCharts[key] = chart;
    } // end inner for (group.items)
  } // end outer for (groups)
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

function exportLatestFrequencies() {
  const a = document.createElement('a');
  a.href = '/export/frequencies/latest';
  a.download = 'hfdl_frequencies.jsonl';
  document.body.appendChild(a);
  a.click();
  document.body.removeChild(a);
}

// ---- Apply modal -----------------------------------------------------------

// openApplyModal shows the password modal for the given Apply endpoint.
// title and desc are displayed inside the modal.
// On successful authentication the endpoint is POSTed and the modal shows
// a success message before closing.  The password is never stored or logged.
function openApplyModal(endpoint, title, desc) {
  const overlay  = document.getElementById('apply-modal');
  const titleEl  = document.getElementById('modal-title');
  const descEl   = document.getElementById('modal-desc');
  const passEl   = document.getElementById('modal-pass');
  const errorEl  = document.getElementById('modal-error');
  const confirmBtn = document.getElementById('modal-confirm');
  const cancelBtn  = document.getElementById('modal-cancel');

  // Reset state
  titleEl.textContent = title;
  descEl.textContent  = desc;
  passEl.value        = '';
  errorEl.hidden      = true;
  errorEl.textContent = '';
  confirmBtn.disabled = false;
  confirmBtn.textContent = 'Confirm';
  overlay.hidden = false;
  passEl.focus();

  function close() {
    overlay.hidden = true;
    passEl.value   = '';
    confirmBtn.removeEventListener('click', onConfirm);
    cancelBtn.removeEventListener('click', onCancel);
    overlay.removeEventListener('click', onOverlayClick);
    passEl.removeEventListener('keydown', onKeydown);
  }

  function onCancel() { close(); }

  function onOverlayClick(e) {
    if (e.target === overlay) close();
  }

  function onKeydown(e) {
    if (e.key === 'Enter') onConfirm();
    if (e.key === 'Escape') close();
  }

  function onConfirm() {
    const pass = passEl.value;
    passEl.value = '';
    errorEl.hidden = true;
    confirmBtn.disabled = true;
    confirmBtn.textContent = 'Applying…';

    fetch(endpoint, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ pass }),
    })
      .then(r => {
        if (r.ok) {
          // Success — show message, then reload the page after a short delay
          // to allow Docker to restart the container.
          confirmBtn.textContent = '✓ Done — restarting…';
          descEl.textContent = 'The frequency file has been updated. The service is restarting — this page will reload in 5 seconds.';
          cancelBtn.style.display = 'none';
          setTimeout(() => { location.reload(); }, 5000);
        } else if (r.status === 401) {
          errorEl.textContent = '✗ Incorrect password';
          errorEl.hidden = false;
          confirmBtn.disabled = false;
          confirmBtn.textContent = 'Confirm';
          passEl.focus();
        } else if (r.status === 403) {
          errorEl.textContent = '✗ Apply is not enabled on this server';
          errorEl.hidden = false;
          confirmBtn.disabled = false;
          confirmBtn.textContent = 'Confirm';
        } else {
          r.json().catch(() => ({})).then(body => {
            errorEl.textContent = '✗ ' + (body.error || `Server error (HTTP ${r.status})`);
            errorEl.hidden = false;
            confirmBtn.disabled = false;
            confirmBtn.textContent = 'Confirm';
          });
        }
      })
      .catch(() => {
        errorEl.textContent = '✗ Network error — please try again';
        errorEl.hidden = false;
        confirmBtn.disabled = false;
        confirmBtn.textContent = 'Confirm';
      });
  }

  confirmBtn.addEventListener('click', onConfirm);
  cancelBtn.addEventListener('click', onCancel);
  overlay.addEventListener('click', onOverlayClick);
  passEl.addEventListener('keydown', onKeydown);
}

// ---- Receiver description --------------------------------------------------

/**
 * Fetch /receiver/description (which proxies /api/description on the UberSDR
 * backend) and, on success, hand the result to map.js to place the receiver
 * marker.  Failures are silently ignored so the rest of the UI is unaffected.
 */
function fetchReceiverDescription() {
  fetch('/receiver/description')
    .then(r => {
      if (!r.ok) return null;
      return r.json();
    })
    .then(info => {
      if (!info) return;
      if (typeof placeReceiverMarker === 'function') {
        placeReceiverMarker(info);
      }
    })
    .catch(err => console.warn('receiver description fetch error:', err));
}

// ---- Boot ------------------------------------------------------------------

document.addEventListener('DOMContentLoaded', () => {
  initTabs();
  // Initialise new tab filter controls
  initFeedFilter();
  initMessagesTab();
  initEventsTab();
  initPlanesFilter();
  if (typeof initLinkQualityTab   === 'function') initLinkQualityTab();
  if (typeof initPropagationFilter === 'function') initPropagationFilter();
  // Chain instances → stats so that cachedInstancesData is set before loadStats()
  // calls renderInstances(), ensuring frequency chips are coloured correctly on
  // the first render after a page load.
  loadInstances().then(() => loadStats());
  loadAircraftTab();
  loadSignalHistory();
  setInterval(loadSignalHistory, 60_000);
  // Poll /instances every 10 s so health status (running, uptime, reconnections) stays live.
  setInterval(loadInstances, 10_000);
  connectSSE();
  startPeriodicRefresh(15000);
  setInterval(tickUptime, 1_000);

  document.getElementById('btn-export-freqs').addEventListener('click', exportActiveFrequencies);
  document.getElementById('btn-export-all-freqs').addEventListener('click', exportAllFrequencies);
  document.getElementById('btn-export-latest-freqs').addEventListener('click', exportLatestFrequencies);

  document.getElementById('btn-apply-freqs').addEventListener('click', () =>
    openApplyModal(
      '/apply/frequencies',
      'Apply Active Frequencies',
      'This will overwrite the frequency file with only the frequencies that have been active during this session, then restart the service.'
    )
  );
  document.getElementById('btn-apply-all-freqs').addEventListener('click', () =>
    openApplyModal(
      '/apply/frequencies/all',
      'Apply All Frequencies',
      'This will overwrite the frequency file with every frequency marked as enabled, then restart the service.'
    )
  );
  document.getElementById('btn-apply-latest-freqs').addEventListener('click', () =>
    openApplyModal(
      '/apply/frequencies/latest',
      'Apply Latest Frequencies',
      'This will fetch the latest frequency list from ubersdr.org, overwrite the frequency file, then restart the service.'
    )
  );

  // Re-render tabs when switching to them
  document.addEventListener('tabchange', (e) => {
    if (e.detail === 'feed')     renderFeedTable();
    if (e.detail === 'planes')   renderAircraftTable();
    if (e.detail === 'signal')   loadSignalHistory();
    if (e.detail === 'messages') renderMessagesTable();
    if (e.detail === 'events')      renderEventsTable();
    if (e.detail === 'network'      && typeof loadNetworkTab      === 'function') loadNetworkTab();
    if (e.detail === 'propagation'  && typeof loadPropagationTab  === 'function') loadPropagationTab();
    if (e.detail === 'lq'           && typeof renderLinkQualityTab === 'function') renderLinkQualityTab();
  });

  // Initialise the Leaflet map (defined in map.js)
  if (typeof initMap === 'function') {
    initMap();
  }

  // Fetch receiver info and place the SDR receiver marker on the map.
  fetchReceiverDescription();
});
