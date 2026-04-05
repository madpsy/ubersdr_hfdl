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
let selectedKey   = null;    // currently selected aircraft key, or null
let selectedGS    = null;    // currently selected GS id (number), or null
let trackPolyline = null;    // L.polyline for the selected aircraft's track
let trackDotLayer = null;    // L.layerGroup of circle markers at each track position

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

// ---- Top positions panel ---------------------------------------------------
const MAX_HISTORY   = 10;
const posCountStore = {}; // key → posCount for ALL aircraft (never evicted)

let historyControl = null;
let historyTickTimer = null;

// ---- Max track points kept in the live-track polyline ----------------------
const MAX_TRACK_POINTS = 500;

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
  // Increment the persistent per-aircraft position counter.
  posCountStore[ac.key] = (posCountStore[ac.key] || 0) + 1;
  renderHistory();
}

function renderHistory() {
  if (!hfdlMap) return;

  // Build top-10 list from posCountStore, restricted to aircraft currently on the map.
  const entries = Object.entries(posCountStore)
    .filter(([key]) => !!aircraftData[key])   // skip purged aircraft
    .sort((a, b) => b[1] - a[1])              // descending by position count
    .slice(0, MAX_HISTORY)
    .map(([key, posCount]) => {
      const ac = aircraftData[key];
      return {
        key,
        label:    acLabel(ac),
        gsId:     ac.gs_id,
        time:     ac.last_seen || Math.floor(Date.now() / 1000),
        posCount,
      };
    });

  let html = '<div class="map-history"><div class="map-history__title">Top Positions</div>';
  if (entries.length === 0) {
    html += '<div class="map-history__empty">No positions yet</div>';
  } else {
    for (const entry of entries) {
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

      // Single delegated click listener on the stable container — set up once,
      // never re-attached, so it cannot accumulate on every renderHistory() call.
      L.DomEvent.on(this._div, 'click', (e) => {
        const row = e.target.closest('.map-history__row--clickable');
        if (!row) return;
        L.DomEvent.stopPropagation(e);
        const key = row.dataset.key;
        if (!key) return;
        selectAircraft(key, true);
        const marker = aircraftMarkers[key];
      });

      // Delegated mouseover: show popup and range line for the hovered aircraft.
      L.DomEvent.on(this._div, 'mouseover', (e) => {
        const row = e.target.closest('.map-history__row--clickable');
        if (!row) return;
        const key = row.dataset.key;
        if (!key) return;
        const marker = aircraftMarkers[key];
        const ac = aircraftData[key];
        if (marker && ac) {
          marker.setPopupContent(buildPopup(ac));
          showRxLine(ac.lat, ac.lon);
          marker.openPopup();
        }
      });

      // Delegated mouseout: close popup and hide range line.
      L.DomEvent.on(this._div, 'mouseout', (e) => {
        const row = e.target.closest('.map-history__row--clickable');
        if (!row) return;
        const key = row.dataset.key;
        if (!key) return;
        const marker = aircraftMarkers[key];
        if (marker) marker.closePopup();
        hideRxLine();
      });

      return this._div;
    };
    historyControl.addTo(hfdlMap);

    // Add live-activity control immediately after historyControl so Leaflet
    // stacks it below the recent-positions panel in the topleft corner.
    if (liveActivityControl) {
      // Already created in initMap() — re-add it now so DOM order is correct.
      // Remove and re-add to force correct stacking after historyControl.
      liveActivityControl.remove();
      liveActivityControl.addTo(hfdlMap);
      if (!showLiveActivity) liveActivityControl.getContainer().style.display = 'none';
    }

    // Tick every second to keep time-ago strings real-time
    historyTickTimer = setInterval(renderHistory, 1_000);
  }
  // Only update the HTML — the delegated listener on the container persists.
  historyControl._div.innerHTML = html;
}

// ---- Legend control --------------------------------------------------------
let legendControl = null;

// gs_id → setTimeout handle — cleared when the flash expires
const _gsFlashTimers = {};

/**
 * Briefly mark a GS as "active" in the legend (bold label) for 1000 ms.
 * Called from app.js whenever a message arrives with that GS as source.
 */
function flashGSLegend(gsId) {
  if (!gsId) return;
  // If a timer is already running, clear it so the 1 s window resets
  if (_gsFlashTimers[gsId]) clearTimeout(_gsFlashTimers[gsId]);
  _gsFlashTimers[gsId] = setTimeout(() => {
    delete _gsFlashTimers[gsId];
    renderLegend(); // re-render to remove the bold class
  }, 1000);
  renderLegend(); // re-render immediately to add the bold class
}

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
      const isFlashing = !!_gsFlashTimers[gsId];
      const selCls     = isSelected ? ' map-legend__row--selected' : '';
      const flashCls   = isFlashing ? ' map-legend__row--active'   : '';
      html += `<div class="map-legend__row map-legend__row--clickable${selCls}${flashCls}" data-gs-id="${gsId}">` +
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

      // Single delegated click listener on the stable container — set up once,
      // never re-attached on each renderLegend() call.
      L.DomEvent.on(this._div, 'click', (e) => {
        const row = e.target.closest('.map-legend__row--clickable');
        if (!row) return;
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

      return this._div;
    };
    legendControl.addTo(hfdlMap);
  }
  // Only update the HTML — the delegated listener on the container persists.
  legendControl._div.innerHTML = html;
}

// ---- Icon builder ----------------------------------------------------------

// ---- Map search ------------------------------------------------------------
let mapSearchTerm = ''; // current search term (lowercase)

function acMatchesMapSearch(ac) {
  if (!mapSearchTerm) return true;
  return (ac.icao   || '').toLowerCase().includes(mapSearchTerm) ||
         (ac.reg    || '').toLowerCase().includes(mapSearchTerm) ||
         (ac.flight || '').toLowerCase().includes(mapSearchTerm);
}

function acExactMatchesMapSearch(ac) {
  if (!mapSearchTerm) return false;
  const t = mapSearchTerm;
  return (ac.icao   || '').toLowerCase() === t ||
         (ac.reg    || '').toLowerCase() === t ||
         (ac.flight || '').toLowerCase() === t;
}

function applyMapSearch() {
  if (!mapSearchTerm) {
    // Clear search — restore normal state (deselect if search was driving selection)
    for (const [k, marker] of Object.entries(aircraftMarkers)) {
      const ac = aircraftData[k];
      if (ac) marker.setIcon(makePlaneIcon(ac, k === selectedKey));
    }
    return;
  }

  // Find exact match first
  let exactKey = null;
  for (const [k, ac] of Object.entries(aircraftData)) {
    if (acExactMatchesMapSearch(ac)) { exactKey = k; break; }
  }

  // Redraw all markers — dim non-matching ones
  for (const [k, marker] of Object.entries(aircraftMarkers)) {
    const ac = aircraftData[k];
    if (ac) marker.setIcon(makePlaneIcon(ac, k === selectedKey));
  }

  // If exact match found, select it (shows popup, draws track)
  if (exactKey) {
    selectAircraft(exactKey);
    const m = aircraftMarkers[exactKey];
    if (m && hfdlMap) {
      hfdlMap.panTo(m.getLatLng());
      m.openPopup();
    }
  }
}

function initMapSearch() {
  const input = document.getElementById('map-search');
  const clear = document.getElementById('map-search-clear');
  if (!input) return;

  input.addEventListener('input', () => {
    mapSearchTerm = input.value.trim().toLowerCase();
    applyMapSearch();
  });
  input.addEventListener('keydown', (e) => {
    if (e.key === 'Escape') {
      input.value = '';
      mapSearchTerm = '';
      applyMapSearch();
      deselectAircraft();
    }
  });
  clear.addEventListener('click', () => {
    input.value = '';
    mapSearchTerm = '';
    applyMapSearch();
    deselectAircraft();
  });
}

function makePlaneIcon(ac, selected) {
  const labelText = ac.flight || ac.reg || ac.icao || ac.key || '';
  const labelHtml = labelText
    ? `<div class="ac-label">${esc(labelText.toUpperCase())}</div>`
    : '';
  const bearing = ac.bearing || 0;
  const colour  = gsColorFor(ac.gs_id);
  // Dim if:
  // - an aircraft is selected and this isn't it
  // - a GS is selected and this aircraft isn't associated with it
  // - a map search is active and this aircraft doesn't match
  const isDimmed = (selectedKey && !selected) ||
                   (selectedGS !== null && ac.gs_id !== selectedGS) ||
                   (mapSearchTerm && !acMatchesMapSearch(ac));
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
let autoFit       = true;

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
  const STEPS    = 180;  // one point every 2° — smoother polygon outline
  const SMOOTH   = 8;    // spread each aircraft ±8 slices (±16°) from its nearest slice
  const FALLBACK = 50;   // km — minimum radius when no aircraft in slice

  // Pass 1: assign each aircraft to its nearest slice
  const sliceMax = new Array(STEPS).fill(FALLBACK);
  for (const { d, b } of acList) {
    const i = Math.round(b / (360 / STEPS)) % STEPS;
    if (d > sliceMax[i]) sliceMax[i] = d;
  }

  // Pass 2: smooth outward — each slice takes the max within ±SMOOTH neighbours.
  // This guarantees every aircraft is inside the polygon without over-inflating
  // slices that are far from any aircraft.
  const pts = [];
  for (let i = 0; i < STEPS; i++) {
    let maxD = FALLBACK;
    for (let j = -SMOOTH; j <= SMOOTH; j++) {
      const idx = (i + j + STEPS) % STEPS;
      if (sliceMax[idx] > maxD) maxD = sliceMax[idx];
    }
    pts.push(destinationPoint(rxLat, rxLon, maxD, 360 * i / STEPS));
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

// ---- Live band activity overlay --------------------------------------------
// Tracks message arrivals per MHz band over a rolling window so the map can
// show a "live activity" bar chart that decays smoothly over time.

const ACTIVITY_WINDOW_MS = 10_000; // keep events for 10 seconds
const ACTIVITY_TICK_MS   =    400; // redraw interval

// Array of { band: int, ts: number } — one entry per received message.
const activityEvents = [];

// Set of all MHz bands ever seen — grows monotonically so the overlay height
// stays stable once a band has appeared (shows 0 when the window is empty).
const seenActivityBands = new Set();

let liveActivityControl = null;

/**
 * Record a new message arrival for the given kHz frequency.
 * Called from app.js whenever an SSE 'message' event arrives.
 */
function recordBandActivity(freqKhz) {
  if (!freqKhz) return;
  const band = Math.floor(freqKhz / 1000);
  activityEvents.push({ band, ts: Date.now() });
  seenActivityBands.add(band);
}

/** Prune events older than ACTIVITY_WINDOW_MS and re-render the control. */
function tickLiveActivity() {
  const cutoff = Date.now() - ACTIVITY_WINDOW_MS;
  // Remove stale entries from the front (they are appended in order)
  while (activityEvents.length > 0 && activityEvents[0].ts < cutoff) {
    activityEvents.shift();
  }
  renderLiveActivity();
}

function renderLiveActivity() {
  if (!hfdlMap || !liveActivityControl) return;

  // Aggregate counts per band from the current window
  const bandCounts = new Map(); // band → count (only bands with activity this window)
  for (const { band } of activityEvents) {
    bandCounts.set(band, (bandCounts.get(band) || 0) + 1);
  }

  // Build HTML — always render all ever-seen bands so height stays stable
  const total = activityEvents.length;
  let html = `<div class="map-live-activity">` +
    `<div class="map-live-activity__title">Live activity` +
    `<span class="map-live-activity__window">10 s</span></div>`;

  if (seenActivityBands.size === 0) {
    html += `<div class="map-live-activity__empty">No messages yet</div>`;
  } else {
    const maxCount = bandCounts.size > 0 ? Math.max(...bandCounts.values()) : 0;
    const sorted = [...seenActivityBands].sort((a, b) => a - b);
    for (const band of sorted) {
      const count = bandCounts.get(band) || 0;
      const widthPct = maxCount > 0 ? Math.max(3, Math.round((count / maxCount) * 100)) : 0;
      const dimCls = count === 0 ? ' map-live-activity__row--dim' : '';
      html +=
        `<div class="map-live-activity__row${dimCls}">` +
          `<span class="map-live-activity__label">${band} MHz</span>` +
          `<div class="map-live-activity__track">` +
            `<div class="map-live-activity__fill" style="width:${count > 0 ? widthPct : 0}%"></div>` +
          `</div>` +
          `<span class="map-live-activity__count">${count > 0 ? count : ''}</span>` +
        `</div>`;
    }
    html += `<div class="map-live-activity__total">${total} msg / 10 s</div>`;
  }
  html += `</div>`;

  liveActivityControl.getContainer().innerHTML = html;
}

// ---- Frequency band filter -------------------------------------------------
// Keys are MHz integers (e.g. 8, 11, 17), values are booleans (true = visible).
// New bands default to true (visible) when first seen.
const freqBandFilter = {};

let freqBandControl = null;
let showLiveActivity = true; // toggled by the "Live" checkbox in the freq-band header

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

  // Collect all bands present in current aircraft data and count aircraft per band
  const bands    = new Set();
  const bandCount = {};
  for (const ac of Object.values(aircraftData)) {
    if (ac.freq_khz) {
      const b = freqBand(ac.freq_khz);
      bands.add(b);
      bandCount[b] = (bandCount[b] || 0) + 1;
    }
  }

  // Register any new bands as visible by default
  for (const band of bands) {
    if (!(band in freqBandFilter)) {
      freqBandFilter[band] = true;
    }
  }

  // Build HTML
  const sorted = [...bands].sort((a, b) => a - b);

  const liveChecked = showLiveActivity ? 'checked' : '';
  let html =
    `<div class="map-freqband-ctrl__title">` +
      `Bands` +
      `<label class="map-freqband-ctrl__live-toggle">` +
        `<input type="checkbox" id="freqband-live-cb" ${liveChecked}>` +
        `<span>Live</span>` +
      `</label>` +
    `</div>`;
  if (sorted.length === 0) {
    html += `<div class="map-freqband-ctrl__empty">No aircraft yet</div>`;
  } else {
    for (const band of sorted) {
      const checked = freqBandFilter[band] !== false ? 'checked' : '';
      const count   = bandCount[band] || 0;
      html +=
        `<label class="map-layer-ctrl__row">` +
        `<input type="checkbox" class="freqband-cb" data-band="${band}" ${checked}>` +
        `<span>${band} MHz</span>` +
        `<span class="map-freqband-ctrl__count">${count}</span>` +
        `</label>`;
    }
    html +=
      `<div class="map-freqband-ctrl__actions">` +
      `<button class="map-freqband-ctrl__btn" data-freqband-action="all">All</button>` +
      `<button class="map-freqband-ctrl__btn" data-freqband-action="none">None</button>` +
      `</div>`;
  }

  if (!freqBandControl) {
    freqBandControl = L.control({ position: 'topright' });
    freqBandControl.onAdd = function () {
      const div = L.DomUtil.create('div', 'map-layer-ctrl map-freqband-ctrl');
      L.DomEvent.disableClickPropagation(div);
      L.DomEvent.disableScrollPropagation(div);

      // Single delegated change listener for checkboxes — set up once on the
      // stable container so it is never re-attached on each render call.
      div.addEventListener('change', (e) => {
        if (e.target.id === 'freqband-live-cb') {
          showLiveActivity = e.target.checked;
          const wrap = liveActivityControl && liveActivityControl.getContainer();
          if (wrap) wrap.style.display = showLiveActivity ? '' : 'none';
          return;
        }
        if (!e.target.classList.contains('freqband-cb')) return;
        const band = parseInt(e.target.dataset.band, 10);
        freqBandFilter[band] = e.target.checked;
        applyBandFilter();
      });

      // Single delegated click listener for All / None buttons.
      div.addEventListener('click', (e) => {
        const action = e.target.dataset.freqbandAction;
        if (!action) return;
        const enable = action === 'all';
        for (const band of Object.keys(freqBandFilter)) freqBandFilter[band] = enable;
        applyBandFilter();
        renderFreqBandControl();
      });

      return div;
    };
    freqBandControl.addTo(hfdlMap);
  }

  // Only update the HTML — the delegated listeners on the container persist.
  freqBandControl.getContainer().innerHTML = html;
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
const gsMarkers  = {}; // gs_id → L.marker
const gsDataMap  = {}; // gs_id → latest gs data object (kept in sync by loadGSMarkers)

function makeGSIcon(gs) {
  const heard  = gs.last_heard && gs.last_heard > 0;
  const colour = gsColorFor(gs.gs_id);
  // Phase 2d: three opacity levels
  //   1.0 — actually heard by this receiver (received a message from it as source)
  //   0.7 — SPDU-active (network advertises it) but never heard directly
  //   0.25 — neither heard nor SPDU-active
  const opacity = heard ? 1.0 : gs.spdu_active ? 0.7 : 0.25;
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

// Build the GS popup HTML — shared between initial creation and mouseover rebuild.
function buildGSPopup(gs, distKm) {
  const lastHeardStr = gs.last_heard
    ? (() => {
        const d   = new Date(gs.last_heard * 1000);
        const time = d.toUTCString().replace(/.*(\d{2}:\d{2}:\d{2}).*/, '$1') + ' UTC';
        const ago  = timeAgo(gs.last_heard);
        return `${time} (${ago})`;
      })()
    : 'Never';
  const freqLine = gs.heard_freqs_khz && gs.heard_freqs_khz.length
    ? `<br>Heard on: ${gs.heard_freqs_khz.map(f => (f / 1000).toFixed(3) + ' MHz').join(', ')}`
    : '';
  const sigLine = gs.last_sig_level
    ? `<br>Last signal: ${gs.last_sig_level.toFixed(1)} dBFS`
    : '';
  const distLine = distKm !== null && distKm !== undefined
    ? `<br>Distance: ${fmtKm(distKm)}`
    : '';
  // Section 7.4: SPDU-derived fields
  const spduLine = gs.spdu_active
    ? `<br><span style="color:#3fb950">● SPDU active</span>`
    : (gs.spdu_last_seen ? `<br><span style="color:#8b949e">○ SPDU last seen ${new Date(gs.spdu_last_seen*1000).toUTCString().replace('GMT','UTC')}</span>` : '');
  const syncLine = gs.spdu_last_seen
    ? (gs.utc_sync ? `<br>UTC sync: ✓` : `<br>UTC sync: ✗`)
    : '';
  const activeFreqLine = gs.active_freqs_khz && gs.active_freqs_khz.length
    ? `<br>Active slots: ${gs.active_freqs_khz.map(f => (f/1000).toFixed(3)+' MHz').join(', ')}`
    : '';
  // Heard-by count from propHeardByGS (defined in app.js)
  const heardByCount = (typeof propHeardByGS !== 'undefined' && propHeardByGS[gs.gs_id]) || 0;
  const heardByLine = heardByCount > 0 ? `<br>Heard by: ${heardByCount} aircraft` : '';

  return `<div class="gs-popup">` +
    `<strong>${esc(gs.location)}</strong><br>` +
    `GS ID: ${gs.gs_id}` +
    spduLine +
    syncLine +
    `<br>Last heard: ${lastHeardStr}` +
    sigLine +
    freqLine +
    activeFreqLine +
    heardByLine +
    distLine +
    `</div>`;
}

function loadGSMarkers() {
  fetch(BASE_PATH + '/groundstations')
    .then(r => r.json())
    .then(list => {
      if (!Array.isArray(list)) return;
      for (const gs of list) {
        if (!gs.lat || !gs.lon) continue;

        // Always keep the live data map up to date so mouseover uses fresh values
        gsDataMap[gs.gs_id] = gs;

        const icon  = makeGSIcon(gs);
        const popup = buildGSPopup(gs, distanceToReceiverKm(gs.lat, gs.lon));
        if (gsMarkers[gs.gs_id]) {
          gsMarkers[gs.gs_id].setIcon(icon).setPopupContent(popup);
        } else {
          const m = L.marker([gs.lat, gs.lon], { icon, zIndexOffset: -100 })
            .bindPopup(popup, { autoPan: false });
          if (showGSMarkers) m.addTo(hfdlMap);
          gsMarkers[gs.gs_id] = m;

          m.on('mouseover', () => {
            // Use gsDataMap so we always get the latest data (last_heard, etc.)
            // rather than the stale closure-captured gs from the initial fetch.
            const live   = gsDataMap[gs.gs_id] || gs;
            const distKm = distanceToReceiverKm(live.lat, live.lon);
            m.setPopupContent(buildGSPopup(live, distKm));
            m.openPopup();
            showRxLine(live.lat, live.lon);
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

let greylineGroup  = null;   // L.layerGroup holding one polygon per world copy
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

  // UTC hour fraction → longitude of the sub-solar point
  const utcHours = date.getUTCHours() + date.getUTCMinutes() / 60 + date.getUTCSeconds() / 3600;
  const solarLon = (180 - utcHours * 15) % 360;

  // Build the terminator line from lon -180 → +180
  const STEPS = 360;
  const terminator = [];
  for (let i = 0; i <= STEPS; i++) {
    const lon = -180 + (360 * i / STEPS);
    const ha  = (lon - solarLon) * (Math.PI / 180);
    let lat;
    if (Math.abs(Math.sin(decl)) < 1e-10) {
      lat = (Math.cos(ha) >= 0) ? 90 : -90;
    } else {
      lat = Math.atan(-Math.cos(ha) * Math.cos(decl) / Math.sin(decl)) * (180 / Math.PI);
    }
    terminator.push([lat, lon]);
  }

  // The night-side polygon:
  //   terminator line (west→east) + cap over the night pole
  // Using explicit pole corners avoids antimeridian clipping issues.
  const nightPole = decl > 0 ? -90 : 90;

  const ring = [
    ...terminator,
    // Walk the night-pole cap: east edge → pole → west edge
    [nightPole,  180],
    [nightPole,    0],
    [nightPole, -180],
    // Close back to start
    terminator[0],
  ];

  return ring;
}

function updateGreyline() {
  if (!hfdlMap) return;
  const basePts = nightPolygon(new Date());

  // Build three copies of the polygon offset by -360, 0, +360 degrees of
  // longitude so the night-side shading renders correctly in every world copy
  // that Leaflet shows when the user zooms out.
  const offsets = [-360, 0, 360];
  const copies  = offsets.map(offset =>
    basePts.map(([lat, lon]) => [lat, lon + offset])
  );

  if (greylineGroup) {
    // Update each existing polygon in-place
    const layers = greylineGroup.getLayers();
    copies.forEach((pts, i) => layers[i].setLatLngs(pts));
  } else {
    const polyOpts = {
      color:       'transparent',
      fillColor:   '#000033',
      fillOpacity: 0.35,
      interactive: false,
    };
    greylineGroup = L.layerGroup(
      copies.map(pts => L.polygon(pts, polyOpts))
    );
    if (showGreyline) greylineGroup.addTo(hfdlMap);
  }
}

function toggleGreyline(visible) {
  showGreyline = visible;
  if (!greylineGroup) return;
  if (visible) {
    if (!hfdlMap.hasLayer(greylineGroup)) greylineGroup.addTo(hfdlMap);
  } else {
    if (hfdlMap.hasLayer(greylineGroup)) greylineGroup.remove();
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

// ---- Propagation layer (Phase 4c) ------------------------------------------
// Faint great-circle lines from GS markers to aircraft that report hearing them.

let propagationLayerGroup = null;
let showPropagationLayer  = false;

// Map of "aircraftKey|gsId" → L.polyline, so we can update lines in-place
// without clearing the whole layer (which causes a visible flash).
const _propLines = {};

function updatePropagationLayer() {
  if (!hfdlMap) return;
  if (!propagationLayerGroup) {
    propagationLayerGroup = L.layerGroup();
  }
  // Ensure the group is on the map when visible
  if (showPropagationLayer && !hfdlMap.hasLayer(propagationLayerGroup)) {
    propagationLayerGroup.addTo(hfdlMap);
  }
  if (!showPropagationLayer) {
    // Clear everything when hidden
    propagationLayerGroup.clearLayers();
    for (const k of Object.keys(_propLines)) delete _propLines[k];
    return;
  }

  fetch(BASE_PATH + '/propagation')
    .then(r => r.json())
    .then(snap => {
      if (!snap || !snap.paths) return;
      if (!propagationLayerGroup) return;

      // Build the set of path keys that should be visible after this update
      const wantedKeys = new Set();

      for (const p of snap.paths) {
        // Filter: if an aircraft or GS is selected, only show paths relevant to it
        if (selectedKey && p.aircraft_key !== selectedKey) continue;
        if (selectedGS !== null && p.gs_id !== selectedGS) continue;

        // Find GS position from gsMarkers
        const gsMarker = gsMarkers[p.gs_id];
        if (!gsMarker) continue;
        const gsLatLng = gsMarker.getLatLng();

        // Find aircraft position from aircraftMarkers
        const acMarker = aircraftMarkers[p.aircraft_key];
        if (!acMarker) continue;
        const acLatLng = acMarker.getLatLng();

        const pathKey = `${p.aircraft_key}|${p.gs_id}`;
        wantedKeys.add(pathKey);

        // Compute great-circle points
        const pts = greatCirclePoints(
          gsLatLng.lat, gsLatLng.lng,
          acLatLng.lat, acLatLng.lng,
          32
        );

        const label = [p.reg, p.flight, p.icao ? `(${p.icao})` : ''].filter(Boolean).join(' ') || p.aircraft_key;
        const tooltipContent = `${esc(p.gs_location)} → ${esc(label)}<br>${p.freq_khz ? (p.freq_khz/1000).toFixed(3)+' MHz' : ''} ${p.sig_level ? p.sig_level.toFixed(1)+' dBFS' : ''}`;

        if (_propLines[pathKey]) {
          // Update existing line in-place — no flash
          _propLines[pathKey].setLatLngs(pts);
          _propLines[pathKey].setTooltipContent(tooltipContent);
        } else {
          // Create a new line
          const line = L.polyline(pts, {
            color: '#f0a500',  // amber — visible over both sea and land
            weight: 1.5,
            opacity: 0.65,
            dashArray: '8 5',
            interactive: false,
          });
          line.bindTooltip(tooltipContent, { sticky: true, className: 'prop-line-tooltip' });
          propagationLayerGroup.addLayer(line);
          _propLines[pathKey] = line;
        }
      }

      // Remove lines that are no longer in the wanted set
      for (const [k, line] of Object.entries(_propLines)) {
        if (!wantedKeys.has(k)) {
          propagationLayerGroup.removeLayer(line);
          delete _propLines[k];
        }
      }
    })
    .catch(() => {});
}

function togglePropagationLayer(visible) {
  showPropagationLayer = visible;
  if (!hfdlMap) return;
  if (visible) {
    // updatePropagationLayer creates the group if needed and adds it to the map
    updatePropagationLayer();
  } else if (propagationLayerGroup) {
    hfdlMap.removeLayer(propagationLayerGroup);
    propagationLayerGroup.clearLayers();
    // Clear the line cache so re-enabling recreates all lines fresh
    for (const k of Object.keys(_propLines)) delete _propLines[k];
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
      `</label>` +
      `<label class="map-layer-ctrl__row">` +
        `<input type="checkbox" id="lyr-propagation">` +
        `<span>Propagation paths</span>` +
      `</label>` +
      `<label class="map-layer-ctrl__row">` +
        `<input type="checkbox" id="lyr-autofit" checked>` +
        `<span>Auto fit</span>` +
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
    div.querySelector('#lyr-propagation').addEventListener('change', e => {
      togglePropagationLayer(e.target.checked);
    });
    div.querySelector('#lyr-autofit').addEventListener('change', e => {
      autoFit = e.target.checked;
      if (autoFit) fitToVisibleAircraft();
    });

    return div;
  };
  layerControl.addTo(hfdlMap);
}

/** Fit the map view to all currently visible aircraft (respects band filter). */
function fitToVisibleAircraft() {
  if (!hfdlMap) return;
  const latlngs = [];
  for (const [key, marker] of Object.entries(aircraftMarkers)) {
    if (hfdlMap.hasLayer(marker)) latlngs.push(marker.getLatLng());
  }
  if (latlngs.length === 0) return;
  hfdlMap.fitBounds(L.latLngBounds(latlngs), { padding: [40, 40], maxZoom: 8 });
}

// ---- Receiver marker -------------------------------------------------------

// ---- Geodesic (great-circle) line helpers ----------------------------------

const DEG = Math.PI / 180;
const RAD = 180 / Math.PI;

/**
 * Interpolate N+1 points along the great-circle arc between two lat/lon pairs.
 * Returns an array of segments (each segment is an array of [lat, lon] pairs)
 * split at antimeridian crossings, suitable for L.polyline (which accepts
 * nested arrays).  Splitting prevents Leaflet from drawing a line across the
 * entire map when the arc crosses ±180° longitude.
 */
function greatCirclePoints(lat1, lon1, lat2, lon2, steps) {
  const φ1 = lat1 * DEG, λ1 = lon1 * DEG;
  const φ2 = lat2 * DEG, λ2 = lon2 * DEG;

  const dφ = φ2 - φ1;
  const dλ = λ2 - λ1;
  const a  = Math.sin(dφ / 2) ** 2 +
             Math.cos(φ1) * Math.cos(φ2) * Math.sin(dλ / 2) ** 2;
  const d  = 2 * Math.atan2(Math.sqrt(a), Math.sqrt(1 - a)); // central angle

  if (d < 1e-10) return [[[lat1, lon1]]]; // same point — one segment

  const flat = [];
  for (let i = 0; i <= steps; i++) {
    const f  = i / steps;
    const A  = Math.sin((1 - f) * d) / Math.sin(d);
    const B  = Math.sin(f * d)       / Math.sin(d);
    const x  = A * Math.cos(φ1) * Math.cos(λ1) + B * Math.cos(φ2) * Math.cos(λ2);
    const y  = A * Math.cos(φ1) * Math.sin(λ1) + B * Math.cos(φ2) * Math.sin(λ2);
    const z  = A * Math.sin(φ1)                 + B * Math.sin(φ2);
    const φi = Math.atan2(z, Math.sqrt(x * x + y * y));
    const λi = Math.atan2(y, x);
    flat.push([φi * RAD, λi * RAD]);
  }

  // Split into segments at antimeridian crossings (longitude jump > 180°)
  const segments = [];
  let seg = [flat[0]];
  for (let i = 1; i < flat.length; i++) {
    if (Math.abs(flat[i][1] - flat[i - 1][1]) > 180) {
      segments.push(seg);
      seg = [];
    }
    seg.push(flat[i]);
  }
  segments.push(seg);
  return segments;
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

      // Delegated click: select aircraft and pan to it.
      L.DomEvent.on(this._div, 'click', (e) => {
        const el = e.target.closest('.map-dist-stats__ac--link');
        if (!el) return;
        L.DomEvent.stopPropagation(e);
        const key = el.dataset.key;
        if (!key) return;
        selectAircraft(key, true);
        const marker = aircraftMarkers[key];
      });

      // Delegated mouseover: show popup and range line.
      L.DomEvent.on(this._div, 'mouseover', (e) => {
        const el = e.target.closest('.map-dist-stats__ac--link');
        if (!el) return;
        const key = el.dataset.key;
        if (!key) return;
        const marker = aircraftMarkers[key];
        const ac = aircraftData[key];
        if (marker && ac) {
          marker.setPopupContent(buildPopup(ac));
          showRxLine(ac.lat, ac.lon);
          marker.openPopup();
        }
      });

      // Delegated mouseout: close popup and hide range line.
      L.DomEvent.on(this._div, 'mouseout', (e) => {
        const el = e.target.closest('.map-dist-stats__ac--link');
        if (!el) return;
        const key = el.dataset.key;
        if (!key) return;
        const marker = aircraftMarkers[key];
        if (marker) marker.closePopup();
        hideRxLine();
      });

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
  const mid  = Math.floor(distances.length / 2);
  const medD = distances.length % 2 === 1
    ? distances[mid].d
    : (distances[mid - 1].d + distances[mid].d) / 2;

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
      `<span class="map-dist-stats__sep">·</span>` +
      `<span class="map-dist-stats__lbl">Med</span>` +
      `<span class="map-dist-stats__val">${fmtKm(medD)}</span>` +
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

  // Delegated listeners are attached once when the control is first created
  // (see distStatsControl.onAdd below). Nothing to attach here on each render.

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

// ---- Aircraft detail side panel -------------------------------------------

// In-memory caches keyed by ICAO hex — mirrors the Go-side LRU so the browser
// doesn't re-fetch on every selection within the same page session.
const _aircraftCache = new Map(); // icao → enrichment object (or null)
const _photoCache    = new Map(); // icao[:reg] → first photo object (or null)

/**
 * Populate and open the side panel for the given aircraft object.
 * Called from selectAircraft() after the track fetch is initiated.
 */
function openAircraftPanel(ac) {
  const panel = document.getElementById('ac-panel');
  if (!panel) return;

  // ---- Populate static fields immediately from aircraftData ----------------
  const label = [ac.flight, ac.reg, ac.icao].filter(Boolean).join(' / ') || ac.key;
  document.getElementById('ac-panel-callsign').textContent = label.toUpperCase();

  // Subtitle: last seen time
  const subEl = document.getElementById('ac-panel-subtitle');
  if (ac.last_seen) {
    const d = new Date(ac.last_seen * 1000);
    subEl.textContent = 'Last seen ' + d.toUTCString().replace(/.*(\d{2}:\d{2}:\d{2}).*/, '$1') + ' UTC';
  } else {
    subEl.textContent = '';
  }

  // Helper: set a <dd> and hide the dt+dd pair if value is empty
  function setField(id, value) {
    const dd = document.getElementById(id);
    if (!dd) return;
    const dt = dd.previousElementSibling;
    if (value) {
      dd.textContent = value;
      dd.classList.remove('ac-panel--hidden');
      if (dt) dt.classList.remove('ac-panel--hidden');
    } else {
      dd.textContent = '—';
      dd.classList.add('ac-panel--hidden');
      if (dt) dt.classList.add('ac-panel--hidden');
    }
  }

  setField('ac-panel-icao',    ac.icao   || '');
  setField('ac-panel-reg',     ac.reg    || '');
  setField('ac-panel-flight',  ac.flight || '');
  setField('ac-panel-freq',    ac.freq_khz ? ac.freq_khz.toLocaleString() + ' kHz' : '');

  const gsName = ac.gs_id && typeof gsNames !== 'undefined' && gsNames[ac.gs_id]
    ? gsNames[ac.gs_id]
    : (ac.gs_id ? `GS ${ac.gs_id}` : '');
  setField('ac-panel-gs', gsName);

  const sigStr = ac.sig_level != null && ac.sig_level !== 0
    ? ac.sig_level.toFixed(1) + ' dBFS'
    : '';
  setField('ac-panel-signal', sigStr);

  setField('ac-panel-alt',
    ac.alt_valid && ac.alt_ft ? Math.round(ac.alt_ft).toLocaleString() + ' ft' : '');

  let spdStr = '';
  if (ac.gnd_spd_kts) {
    spdStr = Math.round(ac.gnd_spd_kts) + ' kts';
    if (ac.vspd_ftmin) {
      spdStr += ' ' + (ac.vspd_ftmin > 0 ? '↑' : '↓') +
                Math.abs(Math.round(ac.vspd_ftmin)) + ' ft/min';
    }
  }
  setField('ac-panel-speed', spdStr);

  setField('ac-panel-track',
    ac.true_trk_valid && ac.true_trk_deg != null
      ? ac.true_trk_deg.toFixed(1) + '° (' + bearingToCardinal(ac.true_trk_deg) + ')'
      : '');

  const distKm = ac.lat && ac.lon ? distanceToReceiverKm(ac.lat, ac.lon) : null;
  setField('ac-panel-dist', distKm !== null ? fmtKm(distKm) : '');

  const trackedKm = ac.tracked_km || 0;
  setField('ac-panel-tracked',
    trackedKm > 0
      ? (trackedKm >= 1000 ? (trackedKm / 1000).toFixed(1) + 'k km' : fmtKm(trackedKm))
      : '');

  setField('ac-panel-msgs', ac.msg_count ? ac.msg_count.toLocaleString() : '');

  setField('ac-panel-lastseen',
    ac.last_seen
      ? (() => {
          const d = new Date(ac.last_seen * 1000);
          return d.toUTCString().replace(/.*(\d{2}:\d{2}:\d{2}).*/, '$1') + ' UTC';
        })()
      : '');

  // Reset enrichment / photo sections while async fetches run
  const enrichEl = document.getElementById('ac-panel-enrich');
  if (enrichEl) enrichEl.hidden = true;
  const photoWrap = document.getElementById('ac-panel-photo-wrap');
  if (photoWrap) photoWrap.hidden = true;
  const skeleton = document.getElementById('ac-panel-photo-skeleton');
  if (skeleton) skeleton.hidden = false;

  // Slide the panel open
  panel.classList.add('ac-panel--open');

  // ---- Async enrichment -------------------------------------------------------
  // ac.icao is the ICAO hex when decoded from the HFDL frame.
  // ac.key is "ICAO hex if known, else registration/flight" — use it as a
  // fallback when ac.icao is absent but the key looks like a 6-hex ICAO.
  const _hexIcao = ac.icao ||
    (/^[0-9A-Fa-f]{6}$/.test(ac.key || '') ? ac.key : null);

  if (!_hexIcao) {
    // No ICAO hex available — hide skeleton, nothing to fetch
    if (skeleton) skeleton.hidden = true;
    return;
  }

  const icao   = _hexIcao.toUpperCase();
  const reg    = ac.reg || '';

  // Helper: format a route airport object returned by /aircraft/{icao}
  function fmtAirport(ap) {
    if (!ap) return '';
    const code = ap.iata || ap.icao || '';
    const city = ap.city || ap.name || '';
    const country = ap.country || '';
    const place = city && country ? city + ', ' + country : city || country;
    return code && place ? code + ' – ' + place : code || place;
  }

  // Fetch unified aircraft enrichment (adsbdb primary, hexdb fallback — done server-side)
  const acPromise = _aircraftCache.has(icao)
    ? Promise.resolve(_aircraftCache.get(icao))
    : fetch(BASE_PATH + '/aircraft/' + icao + '/enrich')
        .then(r => r.ok ? r.json() : null)
        .then(data => { _aircraftCache.set(icao, data); return data; })
        .catch(() => { _aircraftCache.set(icao, null); return null; });

  // Fetch Planespotters photo — backend resolves reg from the live store automatically
  const photoPromise = _photoCache.has(icao)
    ? Promise.resolve(_photoCache.get(icao))
    : fetch(BASE_PATH + '/aircraft/' + icao + '/photo')
        .then(r => r.ok ? r.json() : null)
        .then(data => {
          const photo = data && Array.isArray(data.photos) && data.photos.length > 0
            ? data.photos[0] : null;
          _photoCache.set(icao, photo);
          return photo;
        })
        .catch(() => { _photoCache.set(icao, null); return null; });

  // Apply enrichment result
  acPromise.then(data => {
    if (!panel.classList.contains('ac-panel--open')) return;
    if (!data) return;
    const operator = data.operator || '';
    const type     = data.type     || '';
    if (operator || type) {
      const opEl   = document.getElementById('ac-panel-operator');
      const typeEl = document.getElementById('ac-panel-type');
      if (opEl)   opEl.textContent   = operator;
      if (typeEl) typeEl.textContent = type;
      if (enrichEl) enrichEl.hidden = false;
      if (operator && subEl) {
        const existing = subEl.textContent;
        subEl.textContent = operator + (existing ? ' · ' + existing : '');
      }
    }
    setField('ac-panel-iata-flight',  data.iata_flight   || '');
    setField('ac-panel-airline-iata', data.airline_iata  || '');
    setField('ac-panel-manufacturer', data.manufacturer  || '');
    setField('ac-panel-icao-type',    data.icao_type     || '');
    setField('ac-panel-country',      data.country       || '');
    setField('ac-panel-origin',       fmtAirport(data.origin));
    setField('ac-panel-dest',         fmtAirport(data.destination));
  });

  // Apply photo result
  photoPromise.then(photo => {
    if (!panel.classList.contains('ac-panel--open')) return;
    if (skeleton) skeleton.hidden = true;
    if (!photo) return;
    const imgEl    = document.getElementById('ac-panel-photo');
    const linkEl   = document.getElementById('ac-panel-photo-link');
    const creditEl = document.getElementById('ac-panel-photo-credit');
    if (!imgEl || !photoWrap) return;
    const src = (photo.thumbnail_large && photo.thumbnail_large.src) ||
                (photo.thumbnail && photo.thumbnail.src) || '';
    if (!src) return;
    imgEl.src = src;
    imgEl.onload = () => { photoWrap.hidden = false; };
    imgEl.onerror = () => { /* leave hidden */ };
    if (linkEl && photo.link) linkEl.href = photo.link;
    if (creditEl) {
      creditEl.textContent = photo.photographer
        ? '📷 ' + photo.photographer + ' · Planespotters.net'
        : 'Planespotters.net';
    }
  });
}

/** Close and reset the aircraft detail side panel. */
function closeAircraftPanel() {
  const panel = document.getElementById('ac-panel');
  if (panel) panel.classList.remove('ac-panel--open');
}

/** Wire up the close button — called once from initMap(). */
function initAircraftPanel() {
  const btn = document.getElementById('ac-panel-close');
  if (btn) btn.addEventListener('click', () => deselectAircraft());
}

function initMap() {
  hfdlMap = L.map('map', {
    center: [30, 0],
    zoom: 3,
    zoomControl: false,
  });
  // Re-add zoom control — positioned via CSS to sit centred at the top of the map
  L.control.zoom({ position: 'topleft' }).addTo(hfdlMap);

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
  // Refresh propagation paths every 30 s when the layer is visible
  setInterval(() => { if (showPropagationLayer) updatePropagationLayer(); }, 30_000);
  initLayerControl();
  initMapSearch();
  initAircraftPanel();

  // Live-activity overlay — topleft, below the recent-positions history panel.
  // Created here so Leaflet inserts it after historyControl (which is added
  // lazily on first position event) — the CSS margin-top handles the gap.
  liveActivityControl = L.control({ position: 'topleft' });
  liveActivityControl.onAdd = function () {
    const div = L.DomUtil.create('div', 'map-live-activity-wrap');
    L.DomEvent.disableClickPropagation(div);
    L.DomEvent.disableScrollPropagation(div);
    return div;
  };
  liveActivityControl.addTo(hfdlMap);
  renderLiveActivity();

  // Start the live-activity ticker
  setInterval(tickLiveActivity, ACTIVITY_TICK_MS);
}

// ---- Load initial aircraft positions ---------------------------------------

function loadAircraft() {
  fetch(BASE_PATH + '/aircraft')
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
  const lastSeen = ac.last_seen
    ? (() => {
        const d    = new Date(ac.last_seen * 1000);
        const time = d.toUTCString().replace(/.*(\d{2}:\d{2}:\d{2}).*/, '$1') + ' UTC';
        const ago  = timeAgo(ac.last_seen);
        return `${time} (${ago})`;
      })()
    : '—';
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
  const trackedKm = ac.tracked_km || 0;
  const trackedHtml = trackedKm > 0
    ? `Tracked: ${trackedKm >= 1000 ? (trackedKm / 1000).toFixed(1) + 'k km' : fmtKm(trackedKm)}<br>`
    : '';
  // Section 2.6.4: ADS-C altitude, speed, track
  const altHtml = ac.alt_valid && ac.alt_ft
    ? `Altitude: ${Math.round(ac.alt_ft).toLocaleString()} ft<br>`
    : '';
  const spdHtml = ac.gnd_spd_kts
    ? `Speed: ${Math.round(ac.gnd_spd_kts)} kts` +
      (ac.vspd_ftmin ? ` / ${ac.vspd_ftmin > 0 ? '↑' : '↓'}${Math.abs(Math.round(ac.vspd_ftmin))} ft/min` : '') +
      `<br>`
    : '';
  const windHtml = ac.wind_spd_kts
    ? `Wind: ${Math.round(ac.wind_dir_deg)}° / ${Math.round(ac.wind_spd_kts)} kts<br>`
    : '';
  const trkHtml = ac.true_trk_valid && ac.true_trk_deg != null
    ? `Track: ${ac.true_trk_deg.toFixed(1)}° (${bearingToCardinal(ac.true_trk_deg)})<br>`
    : '';
  // Section 7.4: Phase 3 fields in aircraft popup
  function linkIcon(code) {
    switch ((code || '').toUpperCase()) {
      case 'HF':     return '📻 HF';
      case 'VHF':    return '📶 VHF';
      case 'SATCOM': return '🛰 SATCOM';
      default:       return esc(code);
    }
  }
  const cur = ac.current_link ? ac.current_link.toUpperCase() : null;
  let dlHtml = '';
  if (cur) {
    if (ac.available_links && ac.available_links.length > 0) {
      const allLinks = [...ac.available_links];
      if (!allLinks.map(l => l.toUpperCase()).includes(cur)) {
        allLinks.unshift(ac.current_link);
      }
      const parts = allLinks.map(l =>
        l.toUpperCase() === cur
          ? `<strong>${esc(l)}</strong>`
          : esc(l)
      ).join(', ');
      dlHtml = `Datalink: ${parts}<br>`;
    } else {
      dlHtml = `Datalink: <strong>${esc(ac.current_link)}</strong><br>`;
    }
  }
  const lqHtml = (ac.error_rate != null && (ac.mpdu_rx || ac.mpdu_tx))
    ? `Link quality: ${ac.error_rate.toFixed(1)}% err (${ac.mpdu_rx || 0} rx / ${ac.mpdu_tx || 0} tx)<br>`
    : '';
  const fccHtml = ac.last_freq_change_cause
    ? `Freq change: ${esc(ac.last_freq_change_cause)}<br>`
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
      ${altHtml}
      ${spdHtml}
      ${trkHtml}
      ${windHtml}
      ${dlHtml}
      ${lqHtml}
      ${fccHtml}
      ${distHtml}
      ${trackedHtml}
      ${ac.msg_count ? `Messages: ${ac.msg_count.toLocaleString()}<br>` : ''}
      Last seen: ${lastSeen}
    </div>`;
}

// ---- Marker management ----------------------------------------------------

function upsertMarker(ac, fromSSE = false) {
  if (!hfdlMap) return;
  if (!ac.lat || !ac.lon) return;

  // Check whether the position actually changed before overwriting stored data
  const prev = aircraftData[ac.key];
  const posChanged = !prev || prev.lat !== ac.lat || prev.lon !== ac.lon;

  // Always keep the latest data for icon rebuilds
  aircraftData[ac.key] = ac;

  const selected = ac.key === selectedKey;
  const icon  = makePlaneIcon(ac, selected);
  const popup = buildPopup(ac);

  const isNew = !aircraftMarkers[ac.key];

  // Show bottom-centre notification for SSE-driven events only.
  // Show "New" on first appearance, "Updated" only when the position moved AND
  // the aircraft has been seen more than once (posCount > 1 in the persistent store).
  if (fromSSE && (isNew || posChanged)) {
    const label = acLabel(ac);
    const isFirstPos = (posCountStore[ac.key] || 0) <= 1;
    showMapNotification(isNew || isFirstPos ? 'new' : 'update', label);
  }

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
      selectAircraft(ac.key, true);
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

  // If this is the selected aircraft, extend the live track polyline and
  // pan the map if the new position has drifted outside the current viewport.
  if (selected) {
    if (trackPolyline) {
      const latlngs = trackPolyline.getLatLngs();
      latlngs.push(L.latLng(ac.lat, ac.lon));
      if (latlngs.length > MAX_TRACK_POINTS) latlngs.splice(0, latlngs.length - MAX_TRACK_POINTS);
      trackPolyline.setLatLngs(latlngs);
      const trackColor = gsColorFor(ac.gs_id);
      trackPolyline.setStyle({ color: trackColor });
      // Add a new dot for this live position
      if (trackDotLayer) {
        trackDotLayer.addLayer(L.circleMarker([ac.lat, ac.lon], {
          radius:      4,
          color:       trackColor,
          weight:      1.5,
          opacity:     0.9,
          fillColor:   trackColor,
          fillOpacity: 0.55,
          interactive: false,
        }));
      }
    }
    // Pan to keep the selected aircraft in view — only if it has moved outside
    // the current map bounds (avoids jarring movement when it's already visible).
    if (hfdlMap && posChanged) {
      const newLatLng = L.latLng(ac.lat, ac.lon);
      if (!hfdlMap.getBounds().contains(newLatLng)) {
        hfdlMap.panTo(newLatLng);
      }
    }
    // Refresh the side panel data fields (signal, alt, speed, etc.) on live updates.
    // Don't re-fetch photos/enrichment — those are already cached.
    if (typeof openAircraftPanel === 'function') openAircraftPanel(ac);
  }

  renderLegend();
  renderFreqBandControl();
  renderDistanceStats();

  // Auto-fit: only trigger on live SSE updates, and only when nothing is selected.
  // When an aircraft is selected the user has intentionally zoomed in — don't override that.
  if (fromSSE && autoFit && !selectedKey && selectedGS === null) fitToVisibleAircraft();
}

// ---- Selection / track -----------------------------------------------------

function selectAircraft(key, fitBounds = false) {
  // Clear any GS selection first (mutually exclusive)
  selectedGS  = null;
  selectedKey = key;

  // Open the detail side panel immediately with data we already have
  const ac = aircraftData[key];
  if (ac) openAircraftPanel(ac);

  // Redraw all markers to apply dim/highlight
  for (const [k, marker] of Object.entries(aircraftMarkers)) {
    const ac = aircraftData[k];
    if (ac) marker.setIcon(makePlaneIcon(ac, k === key));
  }

  // Remove old track and dots
  if (trackPolyline) {
    trackPolyline.remove();
    trackPolyline = null;
  }
  if (trackDotLayer) {
    trackDotLayer.remove();
    trackDotLayer = null;
  }

  // Fetch and draw the track for the newly selected aircraft
  fetch(BASE_PATH + `/aircraft/${encodeURIComponent(key)}/track`)
    .then(r => r.json())
    .then(track => {
      const ac = aircraftData[key];
      const trackColor = ac ? gsColorFor(ac.gs_id) : '#58a6ff';

      // Draw the polyline and position dots if there are at least 2 points
      if (Array.isArray(track) && track.length >= 2) {
        const latlngs = track.map(p => [p.lat, p.lon]);
        trackPolyline = L.polyline(latlngs, {
          color:   trackColor,
          weight:  3,
          opacity: 1,
        }).addTo(hfdlMap);

        // Add a small circle at each recorded position
        trackDotLayer = L.layerGroup(
          track.map(p => L.circleMarker([p.lat, p.lon], {
            radius:      4,
            color:       trackColor,
            weight:      1.5,
            opacity:     0.9,
            fillColor:   trackColor,
            fillOpacity: 0.55,
            interactive: false,
          }))
        ).addTo(hfdlMap);
      }

      // Always fit/pan when requested, regardless of track length
      if (fitBounds && hfdlMap) {
        const marker = aircraftMarkers[key];
        if (Array.isArray(track) && track.length >= 2) {
          // Multiple points — fit bounds to the full track
          const bounds = L.latLngBounds(track.map(p => [p.lat, p.lon]));
          if (marker) bounds.extend(marker.getLatLng());
          hfdlMap.fitBounds(bounds, { padding: [60, 60], maxZoom: 8 });
        } else if (marker) {
          // Single position or no track — zoom in on the marker
          hfdlMap.setView(marker.getLatLng(), Math.max(hfdlMap.getZoom(), 6));
        }
      }
    })
    .catch(err => console.warn('track fetch error:', err));
  // Refresh propagation layer to show only this aircraft's paths
  if (showPropagationLayer) updatePropagationLayer();
}

/**
 * Select an aircraft and switch to the map tab first.
 * Called from the Planes tab row click handler in app.js.
 * Switches the tab, waits for Leaflet to invalidate its size, then selects
 * the aircraft and fits the map to its track.
 *
 * @param {string} key  Aircraft key (ICAO / reg / flight)
 */
function selectAircraftFromPlanes(key) {
  // Switch to the map tab
  const mapBtn = document.querySelector('.tab-btn[data-tab="map"]');
  if (mapBtn) mapBtn.click();

  // Give Leaflet time to invalidate size (tab switch fires invalidateSize after 50 ms)
  // then select the aircraft and fit bounds.
  setTimeout(() => {
    selectAircraft(key, true);
    // Also open the popup so the user gets immediate feedback
    const marker = aircraftMarkers[key];
    if (marker && hfdlMap) {
      const ac = aircraftData[key];
      if (ac) marker.setPopupContent(buildPopup(ac));
      marker.openPopup();
    }
  }, 80);
}

function deselectAircraft() {
  if (!selectedKey && selectedGS === null) return;
  selectedKey = null;
  selectedGS  = null;

  if (trackPolyline) {
    trackPolyline.remove();
    trackPolyline = null;
  }
  if (trackDotLayer) {
    trackDotLayer.remove();
    trackDotLayer = null;
  }

  // Close the detail side panel
  closeAircraftPanel();

  // Redraw all markers without dim
  for (const [k, marker] of Object.entries(aircraftMarkers)) {
    const ac = aircraftData[k];
    if (ac) marker.setIcon(makePlaneIcon(ac, false));
  }
  // Restore all propagation paths
  if (showPropagationLayer) updatePropagationLayer();
  // Fit the map to all visible aircraft so the user can see the full picture
  fitToVisibleAircraft();
}

// ---- GS selection ----------------------------------------------------------

function selectGS(gsId) {
  // Clear any aircraft selection first (mutually exclusive)
  if (selectedKey) {
    selectedKey = null;
    if (trackPolyline) { trackPolyline.remove(); trackPolyline = null; }
    if (trackDotLayer) { trackDotLayer.remove(); trackDotLayer = null; }
  }
  selectedGS = gsId;

  // Redraw all aircraft markers to apply GS-based dimming
  for (const [k, marker] of Object.entries(aircraftMarkers)) {
    const ac = aircraftData[k];
    if (ac) marker.setIcon(makePlaneIcon(ac, false));
  }
  // Show only paths for this GS
  if (showPropagationLayer) updatePropagationLayer();
}

function deselectGS() {
  if (selectedGS === null) return;
  selectedGS = null;

  // Redraw all aircraft markers without dim
  for (const [k, marker] of Object.entries(aircraftMarkers)) {
    const ac = aircraftData[k];
    if (ac) marker.setIcon(makePlaneIcon(ac, false));
  }
  // Restore all propagation paths
  if (showPropagationLayer) updatePropagationLayer();
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
  pushHistory(ac);
  upsertMarker(ac, true);
  updateAircraftCount();
}

function handlePurgeEvent(key) {
  if (aircraftMarkers[key]) {
    // Show removal notification before deleting the data
    const ac = aircraftData[key];
    const label = ac ? acLabel(ac) : key;
    showMapNotification('removed', label);

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

// ---- Bottom-centre aircraft notification -----------------------------------

let _mapNotifEl    = null;
let _mapNotifTimer = null;

/**
 * Show a brief notification at the bottom-centre of the map.
 * Uses a plain div injected directly into the map container so that
 * CSS centering works without fighting Leaflet's corner-based layout.
 * @param {string} type  'new' | 'update' | 'removed'
 * @param {string} label Aircraft label (flight / reg / icao)
 */
function showMapNotification(type, label) {
  if (!hfdlMap) return;

  // Create the element once and inject it into the map container
  if (!_mapNotifEl) {
    _mapNotifEl = document.createElement('div');
    _mapNotifEl.className = 'map-ac-notif';
    hfdlMap.getContainer().appendChild(_mapNotifEl);
  }

  // Clear any pending hide timer
  if (_mapNotifTimer) { clearTimeout(_mapNotifTimer); _mapNotifTimer = null; }

  const iconText = type === 'new'     ? '✈ New'
                 : type === 'removed' ? '✈ Removed'
                 : '✈ Updated';
  const cls      = type === 'new'     ? 'map-ac-notif--new'
                 : type === 'removed' ? 'map-ac-notif--removed'
                 : 'map-ac-notif--update';

  _mapNotifEl.className = `map-ac-notif ${cls} map-ac-notif--visible`;
  _mapNotifEl.innerHTML = `<span class="map-ac-notif__icon">${iconText}</span>` +
                          `<span class="map-ac-notif__label">${esc(label)}</span>`;

  // Auto-hide after 3 s
  _mapNotifTimer = setTimeout(() => {
    _mapNotifEl.classList.remove('map-ac-notif--visible');
    _mapNotifTimer = null;
  }, 3000);
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
