/* -----------------------------------------------------------------------
   HFDL Launcher — Aircraft Map (Leaflet)
   ----------------------------------------------------------------------- */

'use strict';

// Initialised once the DOM is ready (called from app.js DOMContentLoaded)
let hfdlMap = null;
const aircraftMarkers = {}; // key → L.marker
const aircraftData    = {}; // key → latest ac object (for icon rebuilds)

// ---- Receiver marker -------------------------------------------------------
let receiverMarker = null;
let receiverLatLng = null; // set by placeReceiverMarker(), used for range lines

// ---- Receiver range line ---------------------------------------------------
let rxRangeLine = null; // L.polyline drawn on aircraft hover, removed on mouseout

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
        // Pan to the GS marker if it exists
        const marker = gsMarkers[gsId];
        if (marker) hfdlMap.panTo(marker.getLatLng());
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
let showAcLabels  = false;
let showPlanes    = true;
let showArcLayer  = true;

// ---- Coverage arc layer ----------------------------------------------------
let arcLayer = null; // L.polygon drawn from receiver showing bearing/distance coverage

/**
 * Given a centre point, a radius in km, and a bearing in degrees,
 * return the [lat, lon] of the point at that distance and bearing
 * using the spherical Earth formula.
 */
function destinationPoint(lat, lon, radiusKm, bearingDeg) {
  const R  = 6371;
  const δ  = radiusKm / R;           // angular distance
  const θ  = bearingDeg * DEG;
  const φ1 = lat * DEG;
  const λ1 = lon * DEG;
  const φ2 = Math.asin(
    Math.sin(φ1) * Math.cos(δ) +
    Math.cos(φ1) * Math.sin(δ) * Math.cos(θ)
  );
  const λ2 = λ1 + Math.atan2(
    Math.sin(θ) * Math.sin(δ) * Math.cos(φ1),
    Math.cos(δ) - Math.sin(φ1) * Math.sin(φ2)
  );
  return [φ2 * RAD, ((λ2 * RAD) + 540) % 360 - 180];
}

/**
 * Build a radar-footprint polygon centred on the receiver.
 *
 * Strategy: divide 360° into STEPS equal slices. For each slice, find the
 * furthest visible aircraft whose bearing falls within ±halfWidth of the
 * slice centre. If no aircraft falls in that slice, use a small fallback
 * radius so the polygon stays connected to the receiver area.
 *
 * The result is a spiky, organic closed shape that bulges out toward every
 * aircraft and pulls back where there are none — every aircraft is guaranteed
 * to be inside or on the boundary.
 */
function buildRadarFootprint(rxLat, rxLon, acList) {
  const STEPS     = 72;   // one point every 5°
  const HALF_W    = 10;   // ±10° search window per slice
  const FALLBACK  = 50;   // km — minimum radius when no aircraft in slice

  const pts = [];
  for (let i = 0; i < STEPS; i++) {
    const centreBearing = (360 * i / STEPS);
    let maxD = FALLBACK;

    for (const { d, b } of acList) {
      // Angular difference, wrapped to [-180, 180]
      let diff = ((b - centreBearing) + 540) % 360 - 180;
      if (Math.abs(diff) <= HALF_W && d > maxD) maxD = d;
    }

    pts.push(destinationPoint(rxLat, rxLon, maxD, centreBearing));
  }
  return pts;
}

/**
 * Recompute and redraw the radar-footprint coverage polygon based on
 * currently visible aircraft.
 * Called from renderDistanceStats() and placeReceiverMarker().
 */
function updateArcLayer() {
  if (!hfdlMap || !receiverLatLng) {
    if (arcLayer) { arcLayer.remove(); arcLayer = null; }
    return;
  }

  // Collect {d, b} for every visible aircraft
  const acList = [];
  for (const [key, ac] of Object.entries(aircraftData)) {
    if (!ac.lat || !ac.lon) continue;
    const marker = aircraftMarkers[key];
    if (!marker || !hfdlMap.hasLayer(marker)) continue;
    const d = distanceToReceiverKm(ac.lat, ac.lon);
    const b = bearingToReceiver(ac.lat, ac.lon);
    if (d !== null && b !== null) acList.push({ d, b });
  }

  if (acList.length === 0) {
    if (arcLayer) { arcLayer.remove(); arcLayer = null; }
    return;
  }

  const pts = buildRadarFootprint(
    receiverLatLng[0], receiverLatLng[1], acList
  );

  if (arcLayer) {
    arcLayer.setLatLngs(pts);
  } else {
    arcLayer = L.polygon(pts, {
      color:       '#58a6ff',
      weight:      1.5,
      opacity:     0.55,
      fillColor:   '#58a6ff',
      fillOpacity: 0.07,
      interactive: false,
      dashArray:   '5 4',
    });
    if (showArcLayer) arcLayer.addTo(hfdlMap);
  }
}

function toggleArcLayer(visible) {
  showArcLayer = visible;
  if (!arcLayer) return;
  if (visible) {
    if (!hfdlMap.hasLayer(arcLayer)) arcLayer.addTo(hfdlMap);
  } else {
    if (hfdlMap.hasLayer(arcLayer)) arcLayer.remove();
  }
}

// ---- Frequency band filter -------------------------------------------------
// Keys are MHz integers (e.g. 8, 11, 17), values are booleans (true = visible).
// New bands default to true (visible) when first seen.
const freqBandFilter = {};

let freqBandControl = null;

/** Return the MHz band integer for a kHz frequency value. */
function freqBand(freqKhz) {
  return Math.floor(freqKhz / 1000);
}

/** True if the aircraft's frequency band is currently enabled (or unknown). */
function isBandVisible(ac) {
  if (!ac.freq_khz) return true;
  const band = freqBand(ac.freq_khz);
  return freqBandFilter[band] !== false;
}

function renderFreqBandControl() {
  if (!hfdlMap) return;

  // Collect all bands present in current aircraft data
  const bands = new Set();
  for (const ac of Object.values(aircraftData)) {
    if (ac.freq_khz) bands.add(freqBand(ac.freq_khz));
  }

  // Register any new bands as visible by default
  for (const band of bands) {
    if (!(band in freqBandFilter)) {
      freqBandFilter[band] = true;
    }
  }

  // Build HTML
  const sorted = [...bands].sort((a, b) => a - b);

  let html = `<div class="map-freqband-ctrl__title">Freq Bands</div>`;
  if (sorted.length === 0) {
    html += `<div class="map-freqband-ctrl__empty">No aircraft yet</div>`;
  } else {
    for (const band of sorted) {
      const checked = freqBandFilter[band] !== false ? 'checked' : '';
      html +=
        `<label class="map-layer-ctrl__row">` +
        `<input type="checkbox" class="freqband-cb" data-band="${band}" ${checked}>` +
        `<span>${band} MHz</span>` +
        `</label>`;
    }
    html +=
      `<div class="map-freqband-ctrl__actions">` +
      `<button class="map-freqband-ctrl__btn" id="freqband-all">All</button>` +
      `<button class="map-freqband-ctrl__btn" id="freqband-none">None</button>` +
      `</div>`;
  }

  if (!freqBandControl) {
    freqBandControl = L.control({ position: 'topright' });
    freqBandControl.onAdd = function () {
      const div = L.DomUtil.create('div', 'map-layer-ctrl map-freqband-ctrl');
      L.DomEvent.disableClickPropagation(div);
      L.DomEvent.disableScrollPropagation(div);
      return div;
    };
    freqBandControl.addTo(hfdlMap);
  }

  freqBandControl.getContainer().innerHTML = html;

  // Attach checkbox change handlers
  freqBandControl.getContainer().querySelectorAll('.freqband-cb').forEach(cb => {
    cb.addEventListener('change', (e) => {
      const band = parseInt(e.target.dataset.band, 10);
      freqBandFilter[band] = e.target.checked;
      applyBandFilter();
    });
  });

  // All / None buttons
  const allBtn  = freqBandControl.getContainer().querySelector('#freqband-all');
  const noneBtn = freqBandControl.getContainer().querySelector('#freqband-none');
  if (allBtn) {
    allBtn.addEventListener('click', () => {
      for (const band of Object.keys(freqBandFilter)) freqBandFilter[band] = true;
      applyBandFilter();
      renderFreqBandControl();
    });
  }
  if (noneBtn) {
    noneBtn.addEventListener('click', () => {
      for (const band of Object.keys(freqBandFilter)) freqBandFilter[band] = false;
      applyBandFilter();
      renderFreqBandControl();
    });
  }
}

/** Show/hide all aircraft markers according to the current band filter and showPlanes state. */
function applyBandFilter() {
  for (const [key, marker] of Object.entries(aircraftMarkers)) {
    const ac = aircraftData[key];
    if (!ac) continue;
    const visible = showPlanes && isBandVisible(ac);
    if (visible) {
      if (!hfdlMap.hasLayer(marker)) marker.addTo(hfdlMap);
    } else {
      if (hfdlMap.hasLayer(marker)) marker.remove();
    }
  }
  renderDistanceStats();
}

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
          ? new Date(gs.last_heard * 1000).toUTCString().replace('GMT', 'UTC')
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

          m.on('mouseover', () => {
            // Rebuild popup with live distance (receiverLatLng may have arrived
            // after the marker was first created).
            const distKm = distanceToReceiverKm(gs.lat, gs.lon);
            const distLine = distKm !== null
              ? `<br>Distance: ${fmtKm(distKm)}`
              : '';
            const lastHeardStr2 = gs.last_heard
              ? new Date(gs.last_heard * 1000).toUTCString().replace('GMT', 'UTC')
              : 'Never';
            const freqLine2 = gs.heard_freqs_khz && gs.heard_freqs_khz.length
              ? `<br>Heard on: ${gs.heard_freqs_khz.map(f => (f / 1000).toFixed(3) + ' MHz').join(', ')}`
              : '';
            const sigLine2 = gs.last_sig_level
              ? `<br>Last signal: ${gs.last_sig_level.toFixed(1)} dBFS`
              : '';
            m.setPopupContent(
              `<div class="gs-popup">` +
              `<strong>${esc(gs.location)}</strong><br>` +
              `GS ID: ${gs.gs_id}<br>` +
              `Last heard: ${lastHeardStr2}` +
              sigLine2 +
              freqLine2 +
              distLine +
              `</div>`
            );
            m.openPopup();
            showRxLine(gs.lat, gs.lon);
          });
          m.on('mouseout', () => {
            m.closePopup();
            hideRxLine();
          });
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

function togglePlanes(visible) {
  showPlanes = visible;
  // Respect band filter when re-showing planes
  applyBandFilter();
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
        `<input type="checkbox" id="lyr-planes" checked>` +
        `<span>Planes</span>` +
      `</label>` +
      `<label class="map-layer-ctrl__row">` +
        `<input type="checkbox" id="lyr-labels">` +
        `<span>Plane labels</span>` +
      `</label>` +
      `<label class="map-layer-ctrl__row">` +
        `<input type="checkbox" id="lyr-greyline" checked>` +
        `<span>Grey line</span>` +
      `</label>` +
      `<label class="map-layer-ctrl__row">` +
        `<input type="checkbox" id="lyr-arc" checked>` +
        `<span>Coverage arc</span>` +
      `</label>`;

    div.querySelector('#lyr-gs').addEventListener('change', e => {
      toggleGSMarkers(e.target.checked);
    });
    div.querySelector('#lyr-planes').addEventListener('change', e => {
      togglePlanes(e.target.checked);
    });
    div.querySelector('#lyr-labels').addEventListener('change', e => {
      toggleAcLabels(e.target.checked);
    });
    div.querySelector('#lyr-greyline').addEventListener('change', e => {
      toggleGreyline(e.target.checked);
    });
    div.querySelector('#lyr-arc').addEventListener('change', e => {
      toggleArcLayer(e.target.checked);
    });

    return div;
  };
  layerControl.addTo(hfdlMap);
}

// ---- Receiver marker -------------------------------------------------------

// ---- Geodesic (great-circle) line helpers ----------------------------------

const DEG = Math.PI / 180;
const RAD = 180 / Math.PI;

/**
 * Interpolate N+1 points along the great-circle arc between two lat/lon pairs.
 * Returns an array of [lat, lon] suitable for L.polyline.
 */
function greatCirclePoints(lat1, lon1, lat2, lon2, steps) {
  const φ1 = lat1 * DEG, λ1 = lon1 * DEG;
  const φ2 = lat2 * DEG, λ2 = lon2 * DEG;

  const dφ = φ2 - φ1;
  const dλ = λ2 - λ1;
  const a  = Math.sin(dφ / 2) ** 2 +
             Math.cos(φ1) * Math.cos(φ2) * Math.sin(dλ / 2) ** 2;
  const d  = 2 * Math.atan2(Math.sqrt(a), Math.sqrt(1 - a)); // central angle

  if (d < 1e-10) return [[lat1, lon1]]; // same point

  const pts = [];
  for (let i = 0; i <= steps; i++) {
    const f  = i / steps;
    const A  = Math.sin((1 - f) * d) / Math.sin(d);
    const B  = Math.sin(f * d)       / Math.sin(d);
    const x  = A * Math.cos(φ1) * Math.cos(λ1) + B * Math.cos(φ2) * Math.cos(λ2);
    const y  = A * Math.cos(φ1) * Math.sin(λ1) + B * Math.cos(φ2) * Math.sin(λ2);
    const z  = A * Math.sin(φ1)                 + B * Math.sin(φ2);
    const φi = Math.atan2(z, Math.sqrt(x * x + y * y));
    const λi = Math.atan2(y, x);
    pts.push([φi * RAD, λi * RAD]);
  }
  return pts;
}

/** Draw a dashed great-circle line from the receiver to [lat, lon]. */
function showRxLine(lat, lon) {
  if (!hfdlMap || !receiverLatLng) return;
  const pts = greatCirclePoints(
    receiverLatLng[0], receiverLatLng[1], lat, lon, 64
  );
  if (rxRangeLine) {
    rxRangeLine.setLatLngs(pts);
  } else {
    rxRangeLine = L.polyline(pts, {
      color:     '#58a6ff',
      weight:    1.5,
      opacity:   0.7,
      dashArray: '6 5',
      interactive: false,
    }).addTo(hfdlMap);
  }
}

/** Remove the receiver range line. */
function hideRxLine() {
  if (rxRangeLine) {
    rxRangeLine.remove();
    rxRangeLine = null;
  }
}

/**
 * Return the great-circle distance in km between two lat/lon pairs.
 * Returns null if receiverLatLng is not yet set.
 */
function distanceToReceiverKm(lat, lon) {
  if (!receiverLatLng) return null;
  const R   = 6371;
  const φ1  = receiverLatLng[0] * DEG, φ2 = lat * DEG;
  const dφ  = (lat - receiverLatLng[0]) * DEG;
  const dλ  = (lon - receiverLatLng[1]) * DEG;
  const a   = Math.sin(dφ / 2) ** 2 +
              Math.cos(φ1) * Math.cos(φ2) * Math.sin(dλ / 2) ** 2;
  return R * 2 * Math.atan2(Math.sqrt(a), Math.sqrt(1 - a));
}

/** Format a km distance as "1,234 km" or "987 km". */
function fmtKm(km) {
  return km.toLocaleString(undefined, { maximumFractionDigits: 0 }) + ' km';
}

/**
 * Compute the initial bearing (degrees, 0–360) from the receiver to [lat, lon].
 * Returns null if receiverLatLng is not set.
 */
function bearingToReceiver(lat, lon) {
  if (!receiverLatLng) return null;
  const φ1 = receiverLatLng[0] * DEG;
  const φ2 = lat * DEG;
  const dλ = (lon - receiverLatLng[1]) * DEG;
  const y = Math.sin(dλ) * Math.cos(φ2);
  const x = Math.cos(φ1) * Math.sin(φ2) - Math.sin(φ1) * Math.cos(φ2) * Math.cos(dλ);
  return ((Math.atan2(y, x) * RAD) + 360) % 360;
}

/** Convert a bearing in degrees to a 16-point compass abbreviation. */
function bearingToCardinal(deg) {
  const pts = ['N','NNE','NE','ENE','E','ESE','SE','SSE','S','SSW','SW','WSW','W','WNW','NW','NNW'];
  return pts[Math.round(deg / 22.5) % 16];
}

// ---- Distance / bearing stats control (bottom-left, above plane count) -----
let distStatsControl = null;

function renderDistanceStats() {
  if (!hfdlMap || !receiverLatLng) return;

  // Collect distances and bearings for all *visible* aircraft
  // (respects showPlanes + band filter — checks whether marker is on the map)
  const distances = [];
  const bearings  = [];

  for (const [key, ac] of Object.entries(aircraftData)) {
    if (!ac.lat || !ac.lon) continue;
    const marker = aircraftMarkers[key];
    if (!marker || !hfdlMap.hasLayer(marker)) continue;

    const d = distanceToReceiverKm(ac.lat, ac.lon);
    const b = bearingToReceiver(ac.lat, ac.lon);
    if (d !== null) distances.push({ d, b, key });
    if (b !== null) bearings.push(b);
  }

  if (!distStatsControl) {
    distStatsControl = L.control({ position: 'bottomleft' });
    distStatsControl.onAdd = function () {
      this._div = L.DomUtil.create('div', '');
      L.DomEvent.disableClickPropagation(this._div);
      return this._div;
    };
    distStatsControl.addTo(hfdlMap);
  }

  if (distances.length === 0) {
    distStatsControl._div.innerHTML = '';
    updateArcLayer();
    return;
  }

  distances.sort((a, b) => a.d - b.d);
  const minEntry = distances[0];
  const maxEntry = distances[distances.length - 1];
  const avgD = distances.reduce((s, e) => s + e.d, 0) / distances.length;

  const minCard = minEntry.b != null ? bearingToCardinal(minEntry.b) : '—';
  const maxCard = maxEntry.b != null ? bearingToCardinal(maxEntry.b) : '—';

  // Arc coverage: sort bearings, find largest gap, arc = 360 - gap
  let arcDeg = 0;
  if (bearings.length > 1) {
    const sorted = [...bearings].sort((a, b) => a - b);
    let maxGap = (sorted[0] + 360) - sorted[sorted.length - 1]; // wrap-around gap
    for (let i = 1; i < sorted.length; i++) {
      maxGap = Math.max(maxGap, sorted[i] - sorted[i - 1]);
    }
    arcDeg = Math.round(360 - maxGap);
  }

  const minAc = aircraftData[minEntry.key];
  const maxAc = aircraftData[maxEntry.key];
  const minLabel = minAc ? (minAc.flight || minAc.reg || minAc.icao || minEntry.key) : minEntry.key;
  const maxLabel = maxAc ? (maxAc.flight || maxAc.reg || maxAc.icao || maxEntry.key) : maxEntry.key;
  const minBand  = minAc && minAc.freq_khz ? `${freqBand(minAc.freq_khz)} MHz` : '';
  const maxBand  = maxAc && maxAc.freq_khz ? `${freqBand(maxAc.freq_khz)} MHz` : '';

  const html =
    `<div class="map-dist-stats">` +
    `<div class="map-dist-stats__title">Distance &amp; Bearing` +
    ` <span class="map-dist-stats__count">${distances.length} ac</span></div>` +
    `<div class="map-dist-stats__row">` +
      `<span class="map-dist-stats__lbl">Min</span>` +
      `<span class="map-dist-stats__val">${fmtKm(minEntry.d)}</span>` +
      `<span class="map-dist-stats__dir">${minCard}</span>` +
      `<span class="map-dist-stats__ac map-dist-stats__ac--link" data-key="${esc(minEntry.key)}">${esc(minLabel.toUpperCase())}</span>` +
      (minBand ? `<span class="map-dist-stats__band">${minBand}</span>` : '') +
    `</div>` +
    `<div class="map-dist-stats__row">` +
      `<span class="map-dist-stats__lbl">Avg</span>` +
      `<span class="map-dist-stats__val">${fmtKm(avgD)}</span>` +
    `</div>` +
    `<div class="map-dist-stats__row">` +
      `<span class="map-dist-stats__lbl">Max</span>` +
      `<span class="map-dist-stats__val">${fmtKm(maxEntry.d)}</span>` +
      `<span class="map-dist-stats__dir">${maxCard}</span>` +
      `<span class="map-dist-stats__ac map-dist-stats__ac--link" data-key="${esc(maxEntry.key)}">${esc(maxLabel.toUpperCase())}</span>` +
      (maxBand ? `<span class="map-dist-stats__band">${maxBand}</span>` : '') +
    `</div>` +
    (bearings.length > 1
      ? `<div class="map-dist-stats__row map-dist-stats__row--arc">` +
          `<span class="map-dist-stats__lbl">Arc</span>` +
          `<span class="map-dist-stats__val">${arcDeg}°</span>` +
        `</div>`
      : '') +
    `</div>`;

  distStatsControl._div.innerHTML = html;

  // Make callsign labels clickable — select the aircraft and pan to it
  distStatsControl._div.querySelectorAll('.map-dist-stats__ac--link').forEach(el => {
    L.DomEvent.on(el, 'click', (e) => {
      L.DomEvent.stopPropagation(e);
      const key = el.dataset.key;
      if (!key) return;
      selectAircraft(key);
      const marker = aircraftMarkers[key];
      if (marker) hfdlMap.panTo(marker.getLatLng());
    });
  });

  // Keep the coverage arc in sync with visible aircraft
  updateArcLayer();
}

/**
 * Place (or update) the SDR receiver marker on the map.
 * Called from app.js after a successful /receiver/description fetch.
 *
 * @param {object} info  { callsign, antenna, name, lat, lon }
 */
function placeReceiverMarker(info) {
  if (!hfdlMap) return;
  if (!info.lat || !info.lon) return;

  // Store for use by showRxLine()
  receiverLatLng = [info.lat, info.lon];

  const icon = L.divIcon({
    className: '',
    html: `<div class="rx-marker" title="${esc(info.callsign)}">` +
          `<span class="rx-marker__icon">🏠</span>` +
          `<div class="rx-marker__label">${esc(info.callsign)}</div>` +
          `</div>`,
    iconSize:   [28, 28],
    iconAnchor: [14, 14],
    popupAnchor: [0, -18],
  });

  const popup =
    `<div class="rx-popup">` +
    `<strong>${esc(info.callsign)}</strong><br>` +
    `${info.name   ? `<span class="rx-popup__name">${esc(info.name)}</span><br>` : ''}` +
    `${info.antenna ? `Antenna: ${esc(info.antenna)}<br>` : ''}` +
    `Lat: ${info.lat.toFixed(5)}, Lon: ${info.lon.toFixed(5)}` +
    `</div>`;

  if (receiverMarker) {
    receiverMarker.setLatLng([info.lat, info.lon]).setIcon(icon).setPopupContent(popup);
  } else {
    receiverMarker = L.marker([info.lat, info.lon], { icon, zIndexOffset: 500 })
      .bindPopup(popup, { autoPan: false })
      .addTo(hfdlMap);

    receiverMarker.on('mouseover', () => receiverMarker.openPopup());
    receiverMarker.on('mouseout',  () => receiverMarker.closePopup());
  }

  // Now that we have a receiver position, render distance stats and arc for any
  // aircraft that were already on the map before the receiver was known.
  renderDistanceStats();
  updateArcLayer();
}

function initMap() {
  hfdlMap = L.map('map', {
    center: [30, 0],
    zoom: 3,
    zoomControl: true,
  });

  // Apply default layer states that differ from the CSS baseline
  // (plane labels are off by default)

  L.tileLayer('https://{s}.tile.openstreetmap.org/{z}/{x}/{y}.png', {
    attribution: '© <a href="https://www.openstreetmap.org/copyright">OpenStreetMap</a> contributors',
    maxZoom: 18,
  }).addTo(hfdlMap);

  // Click on the map background deselects
  hfdlMap.on('click', () => deselectAircraft());

  // Apply the default "labels off" state to the map container
  hfdlMap.getContainer().classList.add('hide-ac-labels');

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
  const lastSeen = ac.last_seen ? new Date(ac.last_seen * 1000).toUTCString().replace('GMT', 'UTC') : '—';
  const gsName = ac.gs_id && typeof gsNames !== 'undefined' && gsNames[ac.gs_id]
    ? gsNames[ac.gs_id]
    : ac.gs_id ? `GS ${ac.gs_id}` : null;
  const sigHtml = ac.sig_level != null && ac.sig_level !== 0
    ? `Signal: <span style="${sigClass(ac.sig_level)}">${ac.sig_level.toFixed(1)} dBFS</span><br>`
    : '';
  const distKm = ac.lat && ac.lon ? distanceToReceiverKm(ac.lat, ac.lon) : null;
  const distHtml = distKm !== null
    ? `Distance: ${fmtKm(distKm)}<br>`
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
      ${distHtml}
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
      .bindPopup(popup, { autoPan: false });
    if (showPlanes && isBandVisible(ac)) marker.addTo(hfdlMap);

    marker.on('mouseover', () => {
      const live = aircraftData[ac.key];
      if (live) {
        // Refresh popup so distance reflects current receiverLatLng
        marker.setPopupContent(buildPopup(live));
        showRxLine(live.lat, live.lon);
      }
      marker.openPopup();
    });
    marker.on('mouseout', () => {
      marker.closePopup();
      hideRxLine();
    });
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
  renderFreqBandControl();
  renderDistanceStats();
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
    renderFreqBandControl();
    renderDistanceStats();
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
