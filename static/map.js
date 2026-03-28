/* -----------------------------------------------------------------------
   HFDL Launcher — Aircraft Map (Leaflet)
   ----------------------------------------------------------------------- */

'use strict';

// Initialised once the DOM is ready (called from app.js DOMContentLoaded)
let hfdlMap = null;
const aircraftMarkers = {}; // key → L.marker
const aircraftData    = {}; // key → latest ac object (for icon rebuilds)

// ---- Selection state -------------------------------------------------------
let selectedKey  = null;    // currently selected aircraft key, or null
let selectedGS   = null;    // currently selected GS id (number), or null
let trackPolyline = null;   // L.polyline for the selected aircraft's track

// ---- GS colour palette -----------------------------------------------------
// 16 visually distinct colours that work on a dark map background.
const GS_PALETTE = [
  '#58a6ff', // blue
  '#3fb950', // green
  '#f78166', // red-orange
  '#d2a8ff', // lavender
  '#ffa657', // amber
  '#79c0ff', // sky
  '#56d364', // lime
  '#ff7b72', // coral
  '#bc8cff', // purple
  '#e3b341', // gold
  '#39d353', // bright green
  '#ff9bce', // pink
  '#87ceeb', // light blue
  '#f0883e', // orange
  '#a5d6ff', // pale blue
  '#7ee787', // mint
];

const gsColorMap = {}; // gs_id (number) → colour string
let   gsColorIdx = 0;

function gsColorFor(gsId) {
  if (!gsId) return '#aaaaaa'; // unknown GS — grey
  if (!gsColorMap[gsId]) {
    gsColorMap[gsId] = GS_PALETTE[gsColorIdx % GS_PALETTE.length];
    gsColorIdx++;
  }
  return gsColorMap[gsId];
}

// ---- Recent positions history ----------------------------------------------
const MAX_HISTORY = 20;
const posHistory  = []; // [{key, label, gsId, time, posCount}] newest-first

let historyControl = null;
let historyTickTimer = null;

function acLabel(ac) {
  return (ac.flight || ac.reg || ac.icao || ac.key || '').toUpperCase();
}

function timeAgo(unixSec) {
  const secs = Math.max(0, Math.floor(Date.now() / 1000) - unixSec);
  if (secs < 60)  return `${secs}s ago`;
  if (secs < 3600) return `${Math.floor(secs / 60)}m ago`;
  return `${Math.floor(secs / 3600)}h ago`;
}

function pushHistory(ac) {
  // Remove any existing entry for this aircraft so it moves to the top
  const idx = posHistory.findIndex(e => e.key === ac.key);
  const prevCount = idx !== -1 ? posHistory[idx].posCount : 0;
  if (idx !== -1) posHistory.splice(idx, 1);
  posHistory.unshift({
    key: ac.key,
    label: acLabel(ac),
    gsId: ac.gs_id,
    time: ac.last_seen || Math.floor(Date.now() / 1000),
    posCount: prevCount + 1,
  });
  if (posHistory.length > MAX_HISTORY) posHistory.length = MAX_HISTORY;
  renderHistory();
}

function renderHistory() {
  if (!hfdlMap) return;

  let html = '<div class="map-history"><div class="map-history__title">Recent positions</div>';
  if (posHistory.length === 0) {
    html += '<div class="map-history__empty">No positions yet</div>';
  } else {
    for (const entry of posHistory) {
      const colour    = gsColorFor(entry.gsId);
      const hasMarker = !!aircraftMarkers[entry.key];
      const clickable = hasMarker ? ' map-history__row--clickable' : '';
      html += `<div class="map-history__row${clickable}" data-key="${esc(entry.key)}">` +
              `<span class="map-history__dot" style="background:${colour}"></span>` +
              `<span class="map-history__ac">${esc(entry.label)}</span>` +
              `<span class="map-history__pos-count" title="unique positions">${entry.posCount}</span>` +
              `<span class="map-history__age">${timeAgo(entry.time)}</span>` +
              `</div>`;
    }
  }
  html += '</div>';

  if (!historyControl) {
    historyControl = L.control({ position: 'topleft' });
    historyControl.onAdd = function () {
      this._div = L.DomUtil.create('div', '');
      L.DomEvent.disableClickPropagation(this._div);
      L.DomEvent.disableScrollPropagation(this._div);
      return this._div;
    };
    historyControl.addTo(hfdlMap);

    // Tick every second to keep time-ago strings real-time
    historyTickTimer = setInterval(renderHistory, 1_000);
  }
  historyControl._div.innerHTML = html;

  // Attach click handlers to clickable rows.
  // Use L.DomEvent.on() so clicks are received even though
  // disableClickPropagation() is set on the parent container.
  historyControl._div.querySelectorAll('.map-history__row--clickable').forEach(row => {
    L.DomEvent.on(row, 'click', (e) => {
      L.DomEvent.stopPropagation(e);
      const key = row.dataset.key;
      if (!key) return;
      selectAircraft(key);
      const marker = aircraftMarkers[key];
      if (marker) hfdlMap.panTo(marker.getLatLng());
    });
  });
}

// ---- Legend control --------------------------------------------------------
let legendControl = null;

function renderLegend() {
  if (!hfdlMap) return;

  // Collect the set of GS IDs that have at least one visible aircraft
  const activeGS = new Map(); // gs_id → colour
  for (const ac of Object.values(aircraftData)) {
    if (ac.gs_id) {
      activeGS.set(ac.gs_id, gsColorFor(ac.gs_id));
    }
  }

  // Build legend HTML
  let html = '<div class="map-legend">';
  if (activeGS.size === 0) {
    html += '<span class="map-legend__empty">No aircraft</span>';
  } else {
    const sorted = [...activeGS.entries()].sort((a, b) => a[0] - b[0]);
    for (const [gsId, colour] of sorted) {
      const name = (typeof gsNames !== 'undefined' && gsNames[gsId])
        ? gsNames[gsId]
        : `GS ${gsId}`;
      const isSelected = selectedGS === gsId;
      const selCls = isSelected ? ' map-legend__row--selected' : '';
      html += `<div class="map-legend__row map-legend__row--clickable${selCls}" data-gs-id="${gsId}">` +
              `<span class="map-legend__swatch" style="background:${colour}"></span>` +
              `<span class="map-legend__label">${esc(name)}</span>` +
              `</div>`;
    }
  }
  html += '</div>';

  if (!legendControl) {
    legendControl = L.control({ position: 'bottomright' });
    legendControl.onAdd = function () {
      this._div = L.DomUtil.create('div', '');
      L.DomEvent.disableClickPropagation(this._div);
      return this._div;
    };
    legendControl.addTo(hfdlMap);
  }
  legendControl._div.innerHTML = html;

  // Attach click handlers to legend rows.
  // Use L.DomEvent.on() rather than addEventListener so that clicks are
  // correctly received even though disableClickPropagation() is set on the
  // parent container (native addEventListener can be swallowed in some
  // Leaflet builds when propagation is stopped at the container level).
  legendControl._div.querySelectorAll('.map-legend__row--clickable').forEach(row => {
    L.DomEvent.on(row, 'click', (e) => {
      L.DomEvent.stopPropagation(e);
      const gsId = parseInt(row.dataset.gsId, 10);
      if (selectedGS === gsId) {
        deselectGS();
        renderLegend();
      } else {
        selectGS(gsId);
        renderLegend();
        // Pan to the GS marker if it exists and trigger the same behaviour as
        // clicking the marker directly
        const marker = gsMarkers[gsId];
        if (marker) {
          hfdlMap.panTo(marker.getLatLng());
          marker.fire('click');
        }
      }
    });
  });
}

// ---- Icon builder ----------------------------------------------------------

function makePlaneIcon(ac, selected) {
  const labelText = ac.flight || ac.reg || ac.icao || ac.key || '';
  const labelHtml = labelText
    ? `<div class="ac-label">${esc(labelText.toUpperCase())}</div>`
    : '';
  const bearing = ac.bearing || 0;
  const colour  = gsColorFor(ac.gs_id);
  // Dim if an aircraft is selected and this isn't it,
  // OR a GS is selected and this aircraft isn't associated with it.
  const isDimmed = (selectedKey && !selected) ||
                   (selectedGS !== null && ac.gs_id !== selectedGS);
  const dimmed  = isDimmed ? ' ac-marker--dim' : '';
  const selCls  = selected ? ' ac-marker--selected' : '';
  return L.divIcon({
    className: '',
    html: `<div class="ac-marker${dimmed}${selCls}" style="position:relative;color:${colour}">` +
          `<span style="display:inline-block;transform:rotate(${bearing - 90}deg);font-size:22px">✈</span>` +
          labelHtml +
          `</div>`,
    iconSize: [36, 36],
    iconAnchor: [18, 18],
    popupAnchor: [0, -20],
  });
}

// ---- Layer visibility state ------------------------------------------------
let showGSMarkers = true;
let showAcLabels  = true;

// ---- Map stats counter (bottom-left) ---------------------------------------
let statsCountControl = null;

function renderMapStats() {
  if (!hfdlMap) return;

  const planeCount = Object.keys(aircraftMarkers).length;
  const heardCount = gsHeardSet.size;

  const html =
    `<div class="map-stats-ctrl">` +
    `<span class="map-stats-ctrl__item">✈ <strong>${planeCount}</strong> plane${planeCount !== 1 ? 's' : ''}</span>` +
    `<span class="map-stats-ctrl__sep">·</span>` +
    `<span class="map-stats-ctrl__item">📡 <strong>${heardCount}</strong> active GS</span>` +
    `</div>`;

  if (!statsCountControl) {
    statsCountControl = L.control({ position: 'bottomleft' });
    statsCountControl.onAdd = function () {
      this._div = L.DomUtil.create('div', '');
      L.DomEvent.disableClickPropagation(this._div);
      return this._div;
    };
    statsCountControl.addTo(hfdlMap);
  }
  statsCountControl._div.innerHTML = html;
}

// Set of GS IDs that have been heard (last_heard > 0), populated by loadGSMarkers
const gsHeardSet = new Set();

// ---- Map init --------------------------------------------------------------

// ---- Ground station markers ------------------------------------------------
const gsMarkers = {}; // gs_id → L.marker

function makeGSIcon(gs) {
  const heard   = gs.last_heard && gs.last_heard > 0;
  const colour  = gsColorFor(gs.gs_id);
  const opacity = heard ? 1 : 0.35;
  return L.divIcon({
    className: '',
    html: `<div class="gs-marker" style="color:${colour};opacity:${opacity}" title="${esc(gs.location)}">` +
          `<span style="font-size:20px">📡</span>` +
          `<div class="gs-marker__label">${esc(gs.location)}</div>` +
          `</div>`,
    iconSize: [28, 28],
    iconAnchor: [14, 14],
    popupAnchor: [0, -18],
  });
}

function loadGSMarkers() {
  fetch('/groundstations')
    .then(r => r.json())
    .then(list => {
      if (!Array.isArray(list)) return;
      for (const gs of list) {
        if (!gs.lat || !gs.lon) continue;
        const icon   = makeGSIcon(gs);
        const lastHeardStr = gs.last_heard
          ? new Date(gs.last_heard * 1000).toUTCString()
          : 'Never';
        const freqLine = gs.heard_freqs_khz && gs.heard_freqs_khz.length
          ? `<br>Heard on: ${gs.heard_freqs_khz.map(f => (f / 1000).toFixed(3) + ' MHz').join(', ')}`
          : '';
        const sigLine = gs.last_sig_level
          ? `<br>Last signal: ${gs.last_sig_level.toFixed(1)} dBFS`
          : '';
        const popup = `<div class="gs-popup">` +
          `<strong>${esc(gs.location)}</strong><br>` +
          `GS ID: ${gs.gs_id}<br>` +
          `Last heard: ${lastHeardStr}` +
          sigLine +
          freqLine +
          `</div>`;
        if (gsMarkers[gs.gs_id]) {
          gsMarkers[gs.gs_id].setIcon(icon).setPopupContent(popup);
        } else {
          const m = L.marker([gs.lat, gs.lon], { icon, zIndexOffset: -100 })
            .bindPopup(popup, { autoPan: false });
          if (showGSMarkers) m.addTo(hfdlMap);
          gsMarkers[gs.gs_id] = m;

          m.on('mouseover', () => m.openPopup());
          m.on('mouseout',  () => m.closePopup());
          // Click: select this GS (toggle off if already selected)
          m.on('click', (e) => {
            L.DomEvent.stopPropagation(e);
            if (selectedGS === gs.gs_id) {
              deselectGS();
            } else {
              selectGS(gs.gs_id);
            }
          });
        }
        // Track heard GS for the stats counter
        if (gs.last_heard && gs.last_heard > 0) {
          gsHeardSet.add(gs.gs_id);
        }
      }
      renderMapStats();
    })
    .catch(err => console.warn('gs markers fetch error:', err));
}

// ---- Grey-line terminator --------------------------------------------------
// Computes the night-side polygon using solar position math and renders it as
// a semi-transparent Leaflet polygon.  Updated every minute.

let greylineLayer  = null;
let showGreyline   = true;

/** Solar declination in radians for a given Date. */
function solarDeclination(date) {
  const dayOfYear = (Date.UTC(date.getUTCFullYear(), date.getUTCMonth(), date.getUTCDate())
    - Date.UTC(date.getUTCFullYear(), 0, 0)) / 86400000;
  return -23.45 * (Math.PI / 180) * Math.cos(2 * Math.PI * (dayOfYear + 10) / 365);
}

/**
 * Build a polygon covering the night side of the Earth.
 * Returns an array of [lat, lon] pairs forming a closed ring.
 * Strategy: walk every longitude and compute the latitude of the terminator,
 * then close the polygon over the night pole.
 */
function nightPolygon(date) {
  const decl = solarDeclination(date);

  // UTC hour fraction → solar hour angle of the anti-solar point (night centre)
  const utcHours = date.getUTCHours() + date.getUTCMinutes() / 60 + date.getUTCSeconds() / 3600;
  // Longitude of the sub-solar point
  const solarLon = (180 - utcHours * 15) % 360;

  const pts = [];
  const STEPS = 360;
  for (let i = 0; i <= STEPS; i++) {
    const lon = -180 + (360 * i / STEPS);
    // Hour angle at this longitude relative to the sub-solar point
    const ha = (lon - solarLon) * (Math.PI / 180);
    // Latitude of the terminator: cos(ha)*cos(decl)*cos(lat) + sin(decl)*sin(lat) = 0
    // → tan(lat) = -cos(ha)*cos(decl) / sin(decl)
    let lat;
    if (Math.abs(Math.sin(decl)) < 1e-10) {
      // Near equinox — terminator is a meridian; use ±90 fallback
      lat = (Math.cos(ha) >= 0) ? 90 : -90;
    } else {
      lat = Math.atan(-Math.cos(ha) * Math.cos(decl) / Math.sin(decl)) * (180 / Math.PI);
    }
    pts.push([lat, lon]);
  }

  // Close over the night pole (the pole that is in darkness)
  const nightPole = decl > 0 ? -90 : 90;
  pts.push([nightPole, 180]);
  pts.push([nightPole, -180]);

  return pts;
}

function updateGreyline() {
  if (!hfdlMap) return;
  const pts = nightPolygon(new Date());
  if (greylineLayer) {
    greylineLayer.setLatLngs(pts);
  } else {
    greylineLayer = L.polygon(pts, {
      color:       'transparent',
      fillColor:   '#000033',
      fillOpacity: 0.35,
      interactive: false,
    });
    if (showGreyline) greylineLayer.addTo(hfdlMap);
  }
}

function toggleGreyline(visible) {
  showGreyline = visible;
  if (!greylineLayer) return;
  if (visible) {
    if (!hfdlMap.hasLayer(greylineLayer)) greylineLayer.addTo(hfdlMap);
  } else {
    if (hfdlMap.hasLayer(greylineLayer)) greylineLayer.remove();
  }
}

// ---- Layer toggle functions ------------------------------------------------

function toggleGSMarkers(visible) {
  showGSMarkers = visible;
  for (const m of Object.values(gsMarkers)) {
    if (visible) {
      if (!hfdlMap.hasLayer(m)) m.addTo(hfdlMap);
    } else {
      if (hfdlMap.hasLayer(m)) m.remove();
    }
  }
}

function toggleAcLabels(visible) {
  showAcLabels = visible;
  const container = hfdlMap.getContainer();
  if (visible) {
    container.classList.remove('hide-ac-labels');
  } else {
    container.classList.add('hide-ac-labels');
  }
}

// ---- Layer toggle control --------------------------------------------------
let layerControl = null;

function initLayerControl() {
  layerControl = L.control({ position: 'topright' });
  layerControl.onAdd = function () {
    const div = L.DomUtil.create('div', 'map-layer-ctrl');
    L.DomEvent.disableClickPropagation(div);
    L.DomEvent.disableScrollPropagation(div);

    div.innerHTML =
      `<div class="map-layer-ctrl__title">Layers</div>` +
      `<label class="map-layer-ctrl__row">` +
        `<input type="checkbox" id="lyr-gs" checked>` +
        `<span>Ground stations</span>` +
      `</label>` +
      `<label class="map-layer-ctrl__row">` +
        `<input type="checkbox" id="lyr-labels" checked>` +
        `<span>Plane labels</span>` +
      `</label>` +
      `<label class="map-layer-ctrl__row">` +
        `<input type="checkbox" id="lyr-greyline" checked>` +
        `<span>Grey line</span>` +
      `</label>`;

    div.querySelector('#lyr-gs').addEventListener('change', e => {
      toggleGSMarkers(e.target.checked);
    });
    div.querySelector('#lyr-labels').addEventListener('change', e => {
      toggleAcLabels(e.target.checked);
    });
    div.querySelector('#lyr-greyline').addEventListener('change', e => {
      toggleGreyline(e.target.checked);
    });

    return div;
  };
  layerControl.addTo(hfdlMap);
}

function initMap() {
  hfdlMap = L.map('map', {
    center: [30, 0],
    zoom: 3,
    zoomControl: true,
  });

  L.tileLayer('https://{s}.tile.openstreetmap.org/{z}/{x}/{y}.png', {
    attribution: '© <a href="https://www.openstreetmap.org/copyright">OpenStreetMap</a> contributors',
    maxZoom: 18,
  }).addTo(hfdlMap);

  // Click on the map background deselects
  hfdlMap.on('click', () => deselectAircraft());

  loadAircraft();
  loadGSMarkers();
  setInterval(loadGSMarkers, 30_000);
  updateGreyline();
  setInterval(updateGreyline, 60_000);
  initLayerControl();
}

// ---- Load initial aircraft positions ---------------------------------------

function loadAircraft() {
  fetch('/aircraft')
    .then(r => r.json())
    .then(list => {
      if (!Array.isArray(list)) return;
      list.forEach(ac => { upsertMarker(ac); pushHistory(ac); });
      updateAircraftCount();
      renderHistory();
    })
    .catch(err => console.warn('aircraft fetch error:', err));
}

// ---- Popup builder ---------------------------------------------------------

function sigClass(dbfs) {
  if (dbfs >= -10) return 'color:#3fb950';  // strong — green
  if (dbfs >= -20) return 'color:#e3b341';  // medium — amber
  return 'color:#f78166';                   // weak — red-orange
}

function buildPopup(ac) {
  const label = [ac.flight, ac.reg, ac.icao].filter(Boolean).join(' / ') || ac.key;
  const lastSeen = ac.last_seen ? new Date(ac.last_seen * 1000).toUTCString() : '—';
  const gsName = ac.gs_id && typeof gsNames !== 'undefined' && gsNames[ac.gs_id]
    ? gsNames[ac.gs_id]
    : ac.gs_id ? `GS ${ac.gs_id}` : null;
  const sigHtml = ac.sig_level != null && ac.sig_level !== 0
    ? `Signal: <span style="${sigClass(ac.sig_level)}">${ac.sig_level.toFixed(1)} dBFS</span><br>`
    : '';
  return `
    <div class="ac-popup">
      <strong>${esc(label)}</strong><br>
      ${ac.icao   ? `ICAO: <code>${esc(ac.icao)}</code><br>` : ''}
      ${ac.reg    ? `Reg: ${esc(ac.reg)}<br>` : ''}
      ${ac.flight ? `Flight: ${esc(ac.flight)}<br>` : ''}
      Freq: ${ac.freq_khz ? ac.freq_khz.toLocaleString() + ' kHz' : '—'}<br>
      ${gsName ? `Via: ${esc(gsName)}<br>` : ''}
      ${sigHtml}
      ${ac.msg_count ? `Messages: ${ac.msg_count.toLocaleString()}<br>` : ''}
      Last seen: ${lastSeen}
    </div>`;
}

// ---- Marker management ----------------------------------------------------

function upsertMarker(ac, fromSSE = false) {
  if (!hfdlMap) return;
  if (!ac.lat || !ac.lon) return;

  // Always keep the latest data for icon rebuilds
  aircraftData[ac.key] = ac;

  const selected = ac.key === selectedKey;
  const icon  = makePlaneIcon(ac, selected);
  const popup = buildPopup(ac);

  const isNew = !aircraftMarkers[ac.key];
  if (!isNew) {
    aircraftMarkers[ac.key]
      .setLatLng([ac.lat, ac.lon])
      .setIcon(icon)
      .setPopupContent(popup);
  } else {
    const marker = L.marker([ac.lat, ac.lon], { icon })
      .bindPopup(popup, { autoPan: false })
      .addTo(hfdlMap);

    marker.on('mouseover', () => marker.openPopup());
    marker.on('mouseout',  () => marker.closePopup());
    marker.on('click', (e) => {
      L.DomEvent.stopPropagation(e);
      selectAircraft(ac.key);
    });

    aircraftMarkers[ac.key] = marker;
  }

  // Pulse on every SSE-driven update/addition.
  // New markers need a short delay for Leaflet to attach the element to the DOM.
  if (fromSSE) {
    if (isNew) {
      setTimeout(() => pulseMarker(ac.key), 50);
    } else {
      pulseMarker(ac.key);
    }
  }

  // If this is the selected aircraft, extend the live track polyline
  if (selected && trackPolyline) {
    const latlngs = trackPolyline.getLatLngs();
    latlngs.push(L.latLng(ac.lat, ac.lon));
    trackPolyline.setLatLngs(latlngs);
  }

  renderLegend();
}

// ---- Selection / track -----------------------------------------------------

function selectAircraft(key) {
  // Clear any GS selection first (mutually exclusive)
  selectedGS  = null;
  selectedKey = key;

  // Redraw all markers to apply dim/highlight
  for (const [k, marker] of Object.entries(aircraftMarkers)) {
    const ac = aircraftData[k];
    if (ac) marker.setIcon(makePlaneIcon(ac, k === key));
  }

  // Remove old track
  if (trackPolyline) {
    trackPolyline.remove();
    trackPolyline = null;
  }

  // Fetch and draw the track for the newly selected aircraft
  fetch(`/aircraft/${encodeURIComponent(key)}/track`)
    .then(r => r.json())
    .then(track => {
      if (!Array.isArray(track) || track.length < 2) return;
      const latlngs = track.map(p => [p.lat, p.lon]);
      trackPolyline = L.polyline(latlngs, {
        color: '#58a6ff',
        weight: 2,
        opacity: 0.85,
        dashArray: '5 5',
      }).addTo(hfdlMap);
    })
    .catch(err => console.warn('track fetch error:', err));
}

function deselectAircraft() {
  if (!selectedKey && selectedGS === null) return;
  selectedKey = null;
  selectedGS  = null;

  if (trackPolyline) {
    trackPolyline.remove();
    trackPolyline = null;
  }

  // Redraw all markers without dim
  for (const [k, marker] of Object.entries(aircraftMarkers)) {
    const ac = aircraftData[k];
    if (ac) marker.setIcon(makePlaneIcon(ac, false));
  }
}

// ---- GS selection ----------------------------------------------------------

function selectGS(gsId) {
  // Clear any aircraft selection first (mutually exclusive)
  if (selectedKey) {
    selectedKey = null;
    if (trackPolyline) { trackPolyline.remove(); trackPolyline = null; }
  }
  selectedGS = gsId;

  // Redraw all aircraft markers to apply GS-based dimming
  for (const [k, marker] of Object.entries(aircraftMarkers)) {
    const ac = aircraftData[k];
    if (ac) marker.setIcon(makePlaneIcon(ac, false));
  }
}

function deselectGS() {
  if (selectedGS === null) return;
  selectedGS = null;

  // Redraw all aircraft markers without dim
  for (const [k, marker] of Object.entries(aircraftMarkers)) {
    const ac = aircraftData[k];
    if (ac) marker.setIcon(makePlaneIcon(ac, false));
  }
}

// ---- Aircraft count --------------------------------------------------------

function updateAircraftCount() {
  const count = Object.keys(aircraftMarkers).length;
  const el = document.getElementById('aircraft-label');
  if (el) el.textContent = `Aircraft: ${count}`;
  renderMapStats();
}

// ---- Handle SSE events -----------------------------------------------------

function handlePositionEvent(ac) {
  upsertMarker(ac, true);
  updateAircraftCount();
  pushHistory(ac);
}

function handlePurgeEvent(key) {
  if (aircraftMarkers[key]) {
    aircraftMarkers[key].remove();
    delete aircraftMarkers[key];
    delete aircraftData[key];
    if (selectedKey === key) deselectAircraft();
    updateAircraftCount();
    renderLegend();
  }
}

// ---- Utility ---------------------------------------------------------------

function esc(str) {
  if (!str) return '';
  return str.replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;');
}

// Briefly add the pulse class to a marker's DOM element to trigger the CSS animation.
// setIcon() replaces the element, so we use requestAnimationFrame to wait one frame
// for Leaflet to attach the new element before adding the class.
function pulseMarker(key) {
  const marker = aircraftMarkers[key];
  if (!marker) return;
  requestAnimationFrame(() => {
    const el = marker.getElement();
    if (!el) return;
    const inner = el.querySelector('.ac-marker');
    if (!inner) return;
    inner.classList.remove('ac-marker--pulse');
    // Force reflow so removing+re-adding the class restarts the animation
    void inner.offsetWidth;
    inner.classList.add('ac-marker--pulse');
    inner.addEventListener('animationend', () => {
      inner.classList.remove('ac-marker--pulse');
    }, { once: true });
  });
}

// Invalidate map size when the map tab becomes visible
document.addEventListener('tabchange', (e) => {
  if (e.detail === 'map' && hfdlMap) {
    setTimeout(() => hfdlMap.invalidateSize(), 50);
  }
});
