/* -----------------------------------------------------------------------
   UberSDR HFDL — Propagation tab (visual rewrite)

   Renders a dedicated Leaflet map above the signal matrix with three
   view modes driven by real HFNPDU type-213 data:

   1. Heatmap  — per-band IDW-interpolated signal-strength tiles
   2. Contours — iso-signal lines at −45 / −60 / −75 dBFS
   3. Polar    — per-GS polar heatmap (bearing × distance sectors)

   Data source: GET /propagation  (PropPath[] with ac_lat/ac_lon added)
   ----------------------------------------------------------------------- */

'use strict';

// ── Module state ─────────────────────────────────────────────────────────────

let propMap        = null;   // Leaflet map instance (lazy-init on first tab open)
let _propMapInited = false;
let _propLastSnap  = null;   // last /propagation response
let _propRefreshTimer = null;

// Current UI state
let _propMode = 'heatmap'; // 'heatmap' | 'contours' | 'polar'
let _propBand = 'all';     // 'all' | '8' | '11' | …
let _propGSID = null;      // selected GS id (polar mode, number)

// Layer groups
let _heatLayer     = null;
let _contourLayer  = null;
let _polarLayer    = null;
let _pathLayer     = null;
let _gsLayer       = null;
let _greylineLayer = null;

// Layer visibility — paths and GS markers off by default to keep the map clean
let _showPaths    = false;
let _showGS       = false;
let _showGreyline = true;

// Legend toggle state — clicking a legend item hides/shows that band or level
const _hiddenBands  = new Set(); // band MHz numbers (integers) hidden in heatmap
const _hiddenLevels = new Set(); // contour dBFS levels hidden in contours view

// Matrix filter state (reused from old propagation.js)
let propFilterTerm = '';
let _propLastData  = null;

// ── Constants ─────────────────────────────────────────────────────────────────

const PROP_DEG = Math.PI / 180;
const PROP_RAD = 180 / Math.PI;

const GRID_DEG        = 2.0;   // IDW grid cell size in degrees (matches /propagation/grid bins)
const IDW_RADIUS      = 40;    // max search radius in degrees for heatmap IDW (~4000 km)
const CONTOUR_RADIUS  = 60;    // wider search radius for contour IDW — fills sparse areas
const IDW_POWER       = 2;     // IDW power parameter

// Signal levels observed: −16 to −42 dBFS (stronger than typical HFDL literature
// because dumphfdl reports relative to ADC full-scale, not noise floor).
// Contour levels bracket the observed reception range:
//   −20 dBFS = strong (good copy)
//   −30 dBFS = moderate (reliable)
//   −40 dBFS = weak (marginal)
const CONTOUR_LEVELS = [-20, -30, -40]; // dBFS thresholds

const POLAR_SECTORS = 36;                          // 10° per sector
const POLAR_RINGS   = [500, 1500, 3000, 5000, 8000]; // outer edge km

const BAND_COLOURS = {
  3:  '#a5d6ff',
  5:  '#79c0ff',
  6:  '#58a6ff',
  8:  '#3fb950',
  10: '#56d364',
  11: '#e3b341',
  13: '#ffa657',
  15: '#f0883e',
  17: '#ff7b72',
  21: '#f85149',
};

// ── Utility ───────────────────────────────────────────────────────────────────

function escProp(s) {
  if (!s) return '';
  return String(s).replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;');
}

// sigColour maps a dBFS value to an RGBA colour string.
// Observed range: −16 to −42 dBFS (relative to ADC full-scale).
//   ≥ −20  strong (green)
//   ≥ −30  moderate (amber)
//   ≥ −40  weak (orange)
//   <  −40 very weak (red)
function sigColour(dbfs, alpha) {
  if (dbfs >= -20) return `rgba(63,185,80,${alpha})`;
  if (dbfs >= -30) return `rgba(227,179,65,${alpha})`;
  if (dbfs >= -40) return `rgba(240,136,62,${alpha})`;
  return `rgba(248,81,73,${alpha})`;
}

function haversineKm(lat1, lon1, lat2, lon2) {
  const R  = 6371;
  const dφ = (lat2 - lat1) * PROP_DEG;
  const dλ = (lon2 - lon1) * PROP_DEG;
  const a  = Math.sin(dφ / 2) ** 2 +
             Math.cos(lat1 * PROP_DEG) * Math.cos(lat2 * PROP_DEG) * Math.sin(dλ / 2) ** 2;
  return R * 2 * Math.atan2(Math.sqrt(a), Math.sqrt(1 - a));
}

function bearingDeg(lat1, lon1, lat2, lon2) {
  const φ1 = lat1 * PROP_DEG, φ2 = lat2 * PROP_DEG;
  const dλ = (lon2 - lon1) * PROP_DEG;
  const y  = Math.sin(dλ) * Math.cos(φ2);
  const x  = Math.cos(φ1) * Math.sin(φ2) - Math.sin(φ1) * Math.cos(φ2) * Math.cos(dλ);
  return (Math.atan2(y, x) * PROP_RAD + 360) % 360;
}

function destPoint(lat, lon, km, bearing) {
  const R  = 6371;
  const δ  = km / R;
  const θ  = bearing * PROP_DEG;
  const φ1 = lat * PROP_DEG, λ1 = lon * PROP_DEG;
  const φ2 = Math.asin(Math.sin(φ1) * Math.cos(δ) + Math.cos(φ1) * Math.sin(δ) * Math.cos(θ));
  const λ2 = λ1 + Math.atan2(Math.sin(θ) * Math.sin(δ) * Math.cos(φ1),
                               Math.cos(δ) - Math.sin(φ1) * Math.sin(φ2));
  return [φ2 * PROP_RAD, ((λ2 * PROP_RAD) + 540) % 360 - 180];
}

function mhzBand(freqKhz) {
  return Math.floor(freqKhz / 1000);
}

function filterByBand(paths) {
  if (_propBand === 'all') return paths;
  const band = parseInt(_propBand, 10);
  return paths.filter(p => p.freq_khz && mhzBand(p.freq_khz) === band);
}

function pathsWithPos(paths) {
  return paths.filter(p =>
    p.ac_lat && p.ac_lon &&
    Math.abs(p.ac_lat) <= 90 && Math.abs(p.ac_lon) <= 180
  );
}

// ── IDW interpolation ─────────────────────────────────────────────────────────

function idwAt(lat, lon, samples, radius) {
  let wSum = 0, vSum = 0;
  for (const s of samples) {
    const dLat = lat - s.lat;
    const dLon = lon - s.lon;
    const dist2 = dLat * dLat + dLon * dLon;
    if (dist2 < 1e-10) return s.value; // exact hit
    const distDeg = Math.sqrt(dist2);
    if (distDeg > radius) continue;
    const w = 1 / Math.pow(distDeg, IDW_POWER);
    wSum += w;
    vSum += w * s.value;
  }
  return wSum === 0 ? null : vSum / wSum;
}

function buildIDWGrid(samples, radius) {
  const r0     = radius || IDW_RADIUS;
  const step   = GRID_DEG;
  const latMin = -80, latMax = 80;
  const lonMin = -180, lonMax = 180;
  const rows   = Math.ceil((latMax - latMin) / step);
  const cols   = Math.ceil((lonMax - lonMin) / step);
  const grid   = new Float32Array(rows * cols).fill(NaN);

  for (let r = 0; r < rows; r++) {
    const lat = latMin + r * step + step / 2;
    for (let c = 0; c < cols; c++) {
      const lon = lonMin + c * step + step / 2;
      const v   = idwAt(lat, lon, samples, r0);
      if (v !== null) grid[r * cols + c] = v;
    }
  }
  return { grid, latMin, lonMin, rows, cols, step };
}

// ── Heatmap layer ─────────────────────────────────────────────────────────────

// buildHeatmapLayer renders a signal-strength heatmap by binning observations
// into 1°×1° cells (~100 km) and colouring each cell by average signal level.
// No IDW interpolation — only cells with actual observations are shown.
// Colour = sigColour(avg_sig_level), opacity fixed at 0.55.
// Sources (in priority order): GridCell[] → SigSample[] → PropPath positions.
function buildHeatmapLayer(paths, gridCells, rawSamples) {
  if (!_heatLayer) _heatLayer = L.layerGroup();
  _heatLayer.clearLayers();

  // cellDeg: size of each rendered cell in degrees (~100 km at mid-latitudes)
  const cellDeg = 1.0;

  // bins: Map of "latBin,lonBin,band" → {sum, count, freq_khz}
  const bins = new Map();

  function addToBin(lat, lon, band, sig, freq) {
    const latBin = Math.floor(lat / cellDeg);
    const lonBin = Math.floor(lon / cellDeg);
    const key    = `${latBin},${lonBin},${band}`;
    if (!bins.has(key)) bins.set(key, { latBin, lonBin, band, sum: 0, count: 0, freq });
    const b = bins.get(key);
    b.sum   += sig;
    b.count += 1;
  }

  if (gridCells && gridCells.length > 0) {
    // Best: pre-binned server-side cells (already averaged, use as-is)
    for (const c of gridCells) {
      if (_propBand !== 'all' && c.band_mhz !== parseInt(_propBand, 10)) continue;
      if (_hiddenBands.has(c.band_mhz)) continue;
      addToBin(c.lat, c.lon, c.band_mhz, c.sig_level, c.freq_khz);
    }
  } else if (rawSamples && rawSamples.length > 0) {
    // Good: raw ring-buffer samples — bin client-side
    for (const s of rawSamples) {
      if (s.lat == null || s.lon == null || !s.sig_level || !s.freq_khz) continue;
      const band = mhzBand(s.freq_khz);
      if (_propBand !== 'all' && band !== parseInt(_propBand, 10)) continue;
      if (_hiddenBands.has(band)) continue;
      addToBin(s.lat, s.lon, band, s.sig_level, s.freq_khz);
    }
  } else {
    // Fallback: PropPath positions
    for (const p of pathsWithPos(paths)) {
      if (!p.sig_level || !p.freq_khz) continue;
      const band = mhzBand(p.freq_khz);
      if (_propBand !== 'all' && band !== parseInt(_propBand, 10)) continue;
      if (_hiddenBands.has(band)) continue;
      addToBin(p.ac_lat, p.ac_lon, band, p.sig_level, p.freq_khz);
    }
  }

  // Render one rectangle per bin.
  // Colour = band (from BAND_COLOURS), opacity = signal strength.
  // Opacity range: 0.25 (weak) → 0.65 (strong), map always visible underneath.
  // Signal range auto-detected from the data so it works across different receivers.
  const sigVals = [...bins.values()].map(b => b.sum / b.count);
  const sigMin  = sigVals.length ? Math.min(...sigVals) : -42;
  const sigMax  = sigVals.length ? Math.max(...sigVals) : -16;
  const sigSpan = sigMax - sigMin || 1; // avoid div-by-zero if all same

  for (const b of bins.values()) {
    const avg        = b.sum / b.count;
    const bandColour = BAND_COLOURS[b.band] || '#58a6ff';
    // Normalise within the actual data range, then map to [0.25, 0.65]
    const t     = Math.max(0, Math.min(1, (avg - sigMin) / sigSpan));
    const alpha = 0.25 + t * 0.40;
    const lat0  = b.latBin * cellDeg;
    const lon0  = b.lonBin * cellDeg;
    const bounds = [[lat0, lon0], [lat0 + cellDeg, lon0 + cellDeg]];

    const rect = L.rectangle(bounds, {
      color:       'transparent',
      fillColor:   bandColour,
      fillOpacity: alpha,
      interactive: true,
      weight:      0,
    });
    rect.bindTooltip(
      `${b.band} MHz · ${avg.toFixed(1)} dBFS · ${b.count} obs`,
      { sticky: true, className: 'prop-map-tooltip' }
    );
    _heatLayer.addLayer(rect);
  }

  return _heatLayer;
}

// ── Contour layer (marching squares) ─────────────────────────────────────────

function marchingSquares(grid, rows, cols, latMin, lonMin, step, threshold) {
  const segments = [];

  function val(r, c) {
    if (r < 0 || r >= rows || c < 0 || c >= cols) return NaN;
    return grid[r * cols + c];
  }

  function edgePt(r0, c0, r1, c1) {
    const v0 = val(r0, c0), v1 = val(r1, c1);
    if (isNaN(v0) || isNaN(v1)) return null;
    const t   = (threshold - v0) / (v1 - v0);
    const lat = latMin + (r0 + (r1 - r0) * t) * step + step / 2;
    const lon = lonMin + (c0 + (c1 - c0) * t) * step + step / 2;
    return [lat, lon];
  }

  // Marching squares lookup: index → pairs of edge points
  // Edges: 0=top, 1=right, 2=bottom, 3=left
  const TABLE = {
    1:  [[2,3]],
    2:  [[1,2]],
    3:  [[1,3]],
    4:  [[0,1]],
    5:  [[0,3],[1,2]],
    6:  [[0,2]],
    7:  [[0,3]],
    8:  [[0,3]],
    9:  [[0,2]],
    10: [[0,1],[2,3]],
    11: [[0,1]],
    12: [[1,3]],
    13: [[1,2]],
    14: [[2,3]],
  };

  for (let r = 0; r < rows - 1; r++) {
    for (let c = 0; c < cols - 1; c++) {
      const v00 = val(r,   c);
      const v10 = val(r+1, c);
      const v01 = val(r,   c+1);
      const v11 = val(r+1, c+1);
      if (isNaN(v00) || isNaN(v10) || isNaN(v01) || isNaN(v11)) continue;

      const idx = ((v00 >= threshold) ? 8 : 0) |
                  ((v01 >= threshold) ? 4 : 0) |
                  ((v11 >= threshold) ? 2 : 0) |
                  ((v10 >= threshold) ? 1 : 0);

      if (idx === 0 || idx === 15) continue;
      const pairs = TABLE[idx];
      if (!pairs) continue;

      // Edge midpoints
      const edges = [
        edgePt(r,   c,   r,   c+1), // 0 top
        edgePt(r,   c+1, r+1, c+1), // 1 right
        edgePt(r+1, c,   r+1, c+1), // 2 bottom
        edgePt(r,   c,   r+1, c),   // 3 left
      ];

      for (const [ei, ej] of pairs) {
        const a = edges[ei], b = edges[ej];
        if (a && b) segments.push([a, b]);
      }
    }
  }
  return segments;
}

// buildContourLayer uses pre-binned GridCell[] from /propagation/grid (preferred),
// falls back to raw SigSample[] from snap.samples, then to PropPath positions.
// Uses a wider IDW search radius (CONTOUR_RADIUS) so sparse data still produces contours.
function buildContourLayer(paths, gridCells, rawSamples) {
  if (!_contourLayer) _contourLayer = L.layerGroup();
  _contourLayer.clearLayers();

  let idwSamples;
  if (gridCells && gridCells.length > 0) {
    const filtered = _propBand === 'all'
      ? gridCells
      : gridCells.filter(c => c.band_mhz === parseInt(_propBand, 10));
    if (filtered.length < 1) return _contourLayer;
    idwSamples = filtered.map(c => ({ lat: c.lat, lon: c.lon, value: c.sig_level }));
  } else if (rawSamples && rawSamples.length > 0) {
    // Use ring-buffer samples directly — filter by band if needed
    const filtered = _propBand === 'all'
      ? rawSamples.filter(s => s.lat && s.lon && s.sig_level)
      : rawSamples.filter(s => s.lat && s.lon && s.sig_level && mhzBand(s.freq_khz) === parseInt(_propBand, 10));
    if (filtered.length < 1) return _contourLayer;
    idwSamples = filtered.map(s => ({ lat: s.lat, lon: s.lon, value: s.sig_level }));
  } else {
    const filtered = pathsWithPos(filterByBand(paths)).filter(p => p.sig_level);
    if (filtered.length < 1) return _contourLayer;
    idwSamples = filtered.map(p => ({ lat: p.ac_lat, lon: p.ac_lon, value: p.sig_level }));
  }
  // Use wider radius so sparse samples still fill the IDW grid enough for marching squares
  const { grid, latMin, lonMin, rows, cols, step } = buildIDWGrid(idwSamples, CONTOUR_RADIUS);

  // Keys must match CONTOUR_LEVELS exactly
  const levelStyles = {
    [-20]: { color: '#3fb950', weight: 2,   opacity: 0.9, label: '−20 dBFS (strong)' },
    [-30]: { color: '#e3b341', weight: 1.5, opacity: 0.8, label: '−30 dBFS (moderate)' },
    [-40]: { color: '#f85149', weight: 1,   opacity: 0.7, label: '−40 dBFS (weak)' },
  };

  for (const level of CONTOUR_LEVELS) {
    if (_hiddenLevels.has(level)) continue;
    const segs  = marchingSquares(grid, rows, cols, latMin, lonMin, step, level);
    const style = levelStyles[level];
    for (const [a, b] of segs) {
      const line = L.polyline([a, b], {
        color:       style.color,
        weight:      style.weight,
        opacity:     style.opacity,
        interactive: true,
      });
      line.bindTooltip(style.label, { sticky: true, className: 'prop-map-tooltip' });
      _contourLayer.addLayer(line);
    }
  }
  return _contourLayer;
}

// ── Polar heatmap layer ───────────────────────────────────────────────────────

// buildPolarLayer renders a polar heatmap centred on the selected GS.
// It uses gridCells (from /propagation/grid) as the sample source — computing
// bearing and distance from the GS to each cell centre.  This gives far more
// data than the old PropPath-only approach (which required rare type-213 msgs).
// Falls back to raw SigSample[] from snap.samples, then to PropPath positions.
function buildPolarLayer(paths, gsID, gridCells, rawSamples) {
  if (!_polarLayer) _polarLayer = L.layerGroup();
  _polarLayer.clearLayers();

  if (!gsID) return _polarLayer;

  const gsData = _propGSData[gsID];
  if (!gsData || !gsData.lat || !gsData.lon) return _polarLayer;

  const gsLat = gsData.lat;
  const gsLon = gsData.lon;

  const sectorDeg = 360 / POLAR_SECTORS;
  // polarGrid[sector][ring] = {sum, count}
  const polarGrid = Array.from({ length: POLAR_SECTORS }, () =>
    Array.from({ length: POLAR_RINGS.length }, () => ({ sum: 0, count: 0 }))
  );

  if (gridCells && gridCells.length > 0) {
    // Best: pre-binned grid cells weighted by observation count
    for (const c of gridCells) {
      const dist   = haversineKm(gsLat, gsLon, c.lat, c.lon);
      const bear   = bearingDeg(gsLat, gsLon, c.lat, c.lon);
      const sector = Math.floor(bear / sectorDeg) % POLAR_SECTORS;
      let ring = POLAR_RINGS.length - 1;
      for (let i = 0; i < POLAR_RINGS.length; i++) {
        if (dist <= POLAR_RINGS[i]) { ring = i; break; }
      }
      polarGrid[sector][ring].sum   += c.sig_level * c.count;
      polarGrid[sector][ring].count += c.count;
    }
  } else if (rawSamples && rawSamples.length > 0) {
    // Good: raw ring-buffer samples — each counts as 1 observation
    for (const s of rawSamples) {
      if (!s.lat || !s.lon || !s.sig_level) continue;
      const dist   = haversineKm(gsLat, gsLon, s.lat, s.lon);
      const bear   = bearingDeg(gsLat, gsLon, s.lat, s.lon);
      const sector = Math.floor(bear / sectorDeg) % POLAR_SECTORS;
      let ring = POLAR_RINGS.length - 1;
      for (let i = 0; i < POLAR_RINGS.length; i++) {
        if (dist <= POLAR_RINGS[i]) { ring = i; break; }
      }
      polarGrid[sector][ring].sum   += s.sig_level;
      polarGrid[sector][ring].count += 1;
    }
  } else {
    const gsPaths = paths.filter(p => p.gs_id === gsID && p.ac_lat && p.ac_lon && p.sig_level);
    if (gsPaths.length === 0) return _polarLayer;
    for (const p of gsPaths) {
      const dist   = haversineKm(gsLat, gsLon, p.ac_lat, p.ac_lon);
      const bear   = bearingDeg(gsLat, gsLon, p.ac_lat, p.ac_lon);
      const sector = Math.floor(bear / sectorDeg) % POLAR_SECTORS;
      let ring = POLAR_RINGS.length - 1;
      for (let i = 0; i < POLAR_RINGS.length; i++) {
        if (dist <= POLAR_RINGS[i]) { ring = i; break; }
      }
      polarGrid[sector][ring].sum   += p.sig_level;
      polarGrid[sector][ring].count += 1;
    }
  }

  // Check if we have any data at all
  const hasData = polarGrid.some(s => s.some(r => r.count > 0));
  if (!hasData) return _polarLayer;

  const STEPS = 8; // arc interpolation steps per sector edge

  for (let s = 0; s < POLAR_SECTORS; s++) {
    const bearStart = s * sectorDeg;
    const bearEnd   = bearStart + sectorDeg;

    for (let r = 0; r < POLAR_RINGS.length; r++) {
      const cell = polarGrid[s][r];
      if (cell.count === 0) continue;
      const avg     = cell.sum / cell.count;
      const innerKm = r === 0 ? 0 : POLAR_RINGS[r - 1];
      const outerKm = POLAR_RINGS[r];

      const pts = [];
      if (innerKm === 0) {
        pts.push([gsLat, gsLon]);
      } else {
        for (let i = 0; i <= STEPS; i++) {
          const b = bearStart + (bearEnd - bearStart) * i / STEPS;
          pts.push(destPoint(gsLat, gsLon, innerKm, b));
        }
      }
      for (let i = STEPS; i >= 0; i--) {
        const b = bearStart + (bearEnd - bearStart) * i / STEPS;
        pts.push(destPoint(gsLat, gsLon, outerKm, b));
      }

      const poly = L.polygon(pts, {
        color:       'transparent',
        fillColor:   sigColour(avg, 1),
        fillOpacity: 0.55,
        interactive: true,
        weight:      0,
      });
      poly.bindTooltip(
        `${bearStart.toFixed(0)}°–${bearEnd.toFixed(0)}° · ` +
        `${outerKm >= 1000 ? (outerKm/1000).toFixed(0)+'k' : outerKm} km · ` +
        `${avg.toFixed(1)} dBFS (${cell.count} sample${cell.count !== 1 ? 's' : ''})`,
        { sticky: true, className: 'prop-map-tooltip' }
      );
      _polarLayer.addLayer(poly);
    }
  }
  return _polarLayer;
}

// ── GS data cache ─────────────────────────────────────────────────────────────

const _propGSData = {}; // gs_id → groundStationResponse

function loadPropGSData() {
  fetch(BASE_PATH + '/groundstations')
    .then(r => r.json())
    .then(list => {
      if (!Array.isArray(list)) return;
      for (const gs of list) {
        _propGSData[gs.gs_id] = gs;
      }
      // Populate GS selector for polar mode
      const sel = document.getElementById('prop-gs-select');
      if (sel) {
        while (sel.options.length > 1) sel.remove(1);
        const sorted = list.filter(g => g.lat && g.lon).sort((a, b) => a.gs_id - b.gs_id);
        for (const gs of sorted) {
          const opt = document.createElement('option');
          opt.value       = gs.gs_id;
          opt.textContent = `GS ${gs.gs_id} — ${gs.location}`;
          sel.appendChild(opt);
        }
      }
      renderPropGSMarkers(list);
    })
    .catch(() => {});
}

// ── GS markers on prop map ────────────────────────────────────────────────────

function renderPropGSMarkers(list) {
  if (!propMap) return;
  if (!_gsLayer) _gsLayer = L.layerGroup();
  _gsLayer.clearLayers();

  for (const gs of list) {
    if (!gs.lat || !gs.lon) continue;
    const heard   = gs.last_heard && gs.last_heard > 0;
    const opacity = heard ? 1.0 : gs.spdu_active ? 0.6 : 0.25;
    const icon    = L.divIcon({
      className: '',
      html: `<div class="prop-gs-marker" style="opacity:${opacity}" title="${escProp(gs.location)}">` +
            `📡<div class="prop-gs-marker__label">${escProp(gs.location.split(',')[0])}</div></div>`,
      iconSize:   [28, 28],
      iconAnchor: [14, 14],
    });
    const m = L.marker([gs.lat, gs.lon], { icon })
      .bindTooltip(`GS ${gs.gs_id} — ${escProp(gs.location)}`, { className: 'prop-map-tooltip' });
    _gsLayer.addLayer(m);
  }

  if (_showGS && propMap && !propMap.hasLayer(_gsLayer)) {
    _gsLayer.addTo(propMap);
  }
}

// ── Propagation path lines ────────────────────────────────────────────────────

function buildPathLayer(paths) {
  if (!_pathLayer) _pathLayer = L.layerGroup();
  _pathLayer.clearLayers();

  for (const p of pathsWithPos(filterByBand(paths))) {
    const gs = _propGSData[p.gs_id];
    if (!gs || !gs.lat || !gs.lon) continue;

    const colour = sigColour(p.sig_level, 1);
    const label  = [p.reg, p.flight, p.icao ? `(${p.icao})` : ''].filter(Boolean).join(' ') || p.aircraft_key;
    const line   = L.polyline([[gs.lat, gs.lon], [p.ac_lat, p.ac_lon]], {
      color:       colour,
      weight:      1.5,
      opacity:     0.6,
      dashArray:   '6 4',
      interactive: true,
    });
    line.bindTooltip(
      `${escProp(p.gs_location)} → ${escProp(label)}<br>` +
      `${p.freq_khz ? (p.freq_khz / 1000).toFixed(3) + ' MHz' : ''} · ` +
      `${p.sig_level ? p.sig_level.toFixed(1) + ' dBFS' : ''}`,
      { sticky: true, className: 'prop-map-tooltip' }
    );
    _pathLayer.addLayer(line);
  }
  return _pathLayer;
}

// ── Grey line ─────────────────────────────────────────────────────────────────

function buildGreyline() {
  if (!propMap) return;
  if (!_greylineLayer) _greylineLayer = L.layerGroup();
  _greylineLayer.clearLayers();

  const date  = new Date();
  const doy   = (Date.UTC(date.getUTCFullYear(), date.getUTCMonth(), date.getUTCDate())
                 - Date.UTC(date.getUTCFullYear(), 0, 0)) / 86400000;
  const decl  = -23.45 * PROP_DEG * Math.cos(2 * Math.PI * (doy + 10) / 365);
  const utcH  = date.getUTCHours() + date.getUTCMinutes() / 60 + date.getUTCSeconds() / 3600;
  const sLon  = (180 - utcH * 15) % 360;
  const STEPS = 360;
  const term  = [];

  for (let i = 0; i <= STEPS; i++) {
    const lon = -180 + 360 * i / STEPS;
    const ha  = (lon - sLon) * PROP_DEG;
    let lat;
    if (Math.abs(Math.sin(decl)) < 1e-10) {
      lat = Math.cos(ha) >= 0 ? 90 : -90;
    } else {
      lat = Math.atan(-Math.cos(ha) * Math.cos(decl) / Math.sin(decl)) * PROP_RAD;
    }
    term.push([lat, lon]);
  }

  const nightPole = decl > 0 ? -90 : 90;
  const ring = [...term, [nightPole, 180], [nightPole, 0], [nightPole, -180], term[0]];
  const opts = { color: 'transparent', fillColor: '#000033', fillOpacity: 0.35, interactive: false };

  for (const offset of [-360, 0, 360]) {
    _greylineLayer.addLayer(L.polygon(ring.map(([la, lo]) => [la, lo + offset]), opts));
  }

  if (_showGreyline && propMap && !propMap.hasLayer(_greylineLayer)) {
    _greylineLayer.addTo(propMap);
  }
}

// ── Legend control ────────────────────────────────────────────────────────────

let _propLegendControl = null;

function renderPropLegend() {
  if (!propMap) return;

  let html = '<div class="prop-legend">';

  if (_propMode === 'heatmap') {
    html += '<div class="prop-legend__title">Band colour <span class="prop-legend__hint">(click to toggle)</span></div>';
    const bands = _propBand === 'all'
      ? Object.keys(BAND_COLOURS).map(Number).sort((a, b) => a - b)
      : [parseInt(_propBand, 10)];
    for (const b of bands) {
      const c       = BAND_COLOURS[b] || '#aaa';
      const hidden  = _hiddenBands.has(b);
      const strike  = hidden ? 'text-decoration:line-through;opacity:0.4;' : '';
      html += `<div class="prop-legend__row prop-legend__row--click" data-toggle-band="${b}" style="${strike}cursor:pointer">` +
              `<span class="prop-legend__swatch" style="background:${c};opacity:${hidden ? 0.15 : 0.55}"></span>${b} MHz</div>`;
    }
    html += '<div class="prop-legend__divider"></div>';
    html += '<div class="prop-legend__title">Opacity = signal strength</div>';
    html += '<div class="prop-legend__row"><span class="prop-legend__swatch" style="background:#888;opacity:0.65"></span>Strongest received</div>';
    html += '<div class="prop-legend__row"><span class="prop-legend__swatch" style="background:#888;opacity:0.45"></span>Moderate</div>';
    html += '<div class="prop-legend__row"><span class="prop-legend__swatch" style="background:#888;opacity:0.25"></span>Weakest received</div>';
  } else if (_propMode === 'contours') {
    html += '<div class="prop-legend__title">Contour levels <span class="prop-legend__hint">(click to toggle)</span></div>';
    const contourDefs = [
      { level: -20, color: '#3fb950', label: '−20 dBFS (strong)' },
      { level: -30, color: '#e3b341', label: '−30 dBFS (moderate)' },
      { level: -40, color: '#f85149', label: '−40 dBFS (weak)' },
    ];
    for (const { level, color, label } of contourDefs) {
      const hidden = _hiddenLevels.has(level);
      const strike = hidden ? 'text-decoration:line-through;opacity:0.4;' : '';
      html += `<div class="prop-legend__row prop-legend__row--click" data-toggle-level="${level}" style="${strike}cursor:pointer">` +
              `<span class="prop-legend__line" style="background:${color};opacity:${hidden ? 0.2 : 1}"></span>${label}</div>`;
    }
  } else if (_propMode === 'polar') {
    html += '<div class="prop-legend__title">Signal level</div>';
    html += '<div class="prop-legend__row"><span class="prop-legend__swatch" style="background:rgba(63,185,80,0.55)"></span>Strong (&gt;−20 dBFS)</div>';
    html += '<div class="prop-legend__row"><span class="prop-legend__swatch" style="background:rgba(227,179,65,0.55)"></span>OK (−20 to −30)</div>';
    html += '<div class="prop-legend__row"><span class="prop-legend__swatch" style="background:rgba(240,136,62,0.55)"></span>Marginal (−30 to −40)</div>';
    html += '<div class="prop-legend__row"><span class="prop-legend__swatch" style="background:rgba(248,81,73,0.55)"></span>Weak (&lt;−40 dBFS)</div>';
  }

  html += '</div>';

  if (!_propLegendControl) {
    _propLegendControl = L.control({ position: 'bottomright' });
    _propLegendControl.onAdd = function () {
      this._div = L.DomUtil.create('div', '');
      L.DomEvent.disableClickPropagation(this._div);
      return this._div;
    };
    _propLegendControl.addTo(propMap);
  }
  _propLegendControl._div.innerHTML = html;

  // Wire up click handlers for toggleable rows
  _propLegendControl._div.querySelectorAll('[data-toggle-band]').forEach(el => {
    el.addEventListener('click', () => {
      const band = parseInt(el.dataset.toggleBand, 10);
      if (_hiddenBands.has(band)) _hiddenBands.delete(band);
      else _hiddenBands.add(band);
      if (_propLastSnap) renderPropMap(_propLastSnap, _propGridSnap);
    });
  });
  _propLegendControl._div.querySelectorAll('[data-toggle-level]').forEach(el => {
    el.addEventListener('click', () => {
      const level = parseInt(el.dataset.toggleLevel, 10);
      if (_hiddenLevels.has(level)) _hiddenLevels.delete(level);
      else _hiddenLevels.add(level);
      if (_propLastSnap) renderPropMap(_propLastSnap, _propGridSnap);
    });
  });
}

// ── Sample count control ──────────────────────────────────────────────────────

let _propSampleControl = null;

function renderPropSampleCount(paths, gridSnap) {
  if (!propMap) return;
  // Show raw sample count from grid metadata if available, else path count
  const sampleCount = gridSnap && gridSnap.sample_n != null
    ? gridSnap.sample_n
    : pathsWithPos(filterByBand(paths)).length;
  const cellCount = gridSnap && gridSnap.cells ? gridSnap.cells.length : 0;
  const total   = paths.length;
  const countStr = gridSnap
    ? `${sampleCount} samples → ${cellCount} cells · ${total} paths`
    : `${sampleCount} samples · ${total} paths`;
  const html    = `<div class="prop-sample-count">${countStr} · Updated ${new Date().toUTCString().replace('GMT','UTC').replace(/.*(\d{2}:\d{2}:\d{2}).*/, '$1 UTC')}</div>`;
if (!_propSampleControl) {
  _propSampleControl = L.control({ position: 'bottomleft' });
  _propSampleControl.onAdd = function () {
    this._div = L.DomUtil.create('div', '');
    L.DomEvent.disableClickPropagation(this._div);
    return this._div;
  };
  _propSampleControl.addTo(propMap);
}
_propSampleControl._div.innerHTML = html;
}

// ── Updated label in toolbar ──────────────────────────────────────────────────

function setPropMapUpdated() {
const el = document.getElementById('prop-map-updated');
if (el) {
  const t = new Date();
  el.textContent = 'Updated ' + t.toUTCString().replace(/.*(\d{2}:\d{2}:\d{2}).*/, '$1') + ' UTC';
}
}

// ── Main render dispatcher ────────────────────────────────────────────────────

// _propGridSnap caches the last /propagation/grid response
let _propGridSnap = null;

function renderPropMap(snap, gridSnap) {
if (!propMap || !snap || !snap.paths) return;
const paths      = snap.paths;
const gridCells  = gridSnap && gridSnap.cells ? gridSnap.cells : [];
// Raw ring-buffer samples from snap.samples — used when /propagation/grid is unavailable
const rawSamples = snap.samples || [];

// Always rebuild path lines (they respect band filter)
buildPathLayer(paths);
if (_showPaths && !propMap.hasLayer(_pathLayer)) _pathLayer.addTo(propMap);
if (!_showPaths && propMap.hasLayer(_pathLayer))  _pathLayer.remove();

// Remove all overlay layers before rebuilding
if (_heatLayer    && propMap.hasLayer(_heatLayer))    _heatLayer.remove();
if (_contourLayer && propMap.hasLayer(_contourLayer)) _contourLayer.remove();
if (_polarLayer   && propMap.hasLayer(_polarLayer))   _polarLayer.remove();

if (_propMode === 'heatmap') {
  buildHeatmapLayer(paths, gridCells, rawSamples);
  if (_heatLayer) _heatLayer.addTo(propMap);
} else if (_propMode === 'contours') {
  buildContourLayer(paths, gridCells, rawSamples);
  if (_contourLayer) _contourLayer.addTo(propMap);
} else if (_propMode === 'polar') {
  buildPolarLayer(paths, _propGSID, gridCells, rawSamples);
  if (_polarLayer) _polarLayer.addTo(propMap);
}

renderPropLegend();
renderPropSampleCount(paths, gridSnap);
setPropMapUpdated();

// Also refresh the matrix below the map
_propLastData = snap;
renderPropagationTab(snap);
}

// ── Data fetch ────────────────────────────────────────────────────────────────

// fetchAndRenderPropMap fetches /propagation (matrix + paths) and
// /propagation/grid (pre-binned heatmap cells) in parallel, then renders.
// /propagation/grid may return 404 on older images — that's handled gracefully;
// the builders fall back to snap.samples from /propagation in that case.
function fetchAndRenderPropMap() {
Promise.all([
  fetch(BASE_PATH + '/propagation').then(r => r.json()),
  fetch(BASE_PATH + '/propagation/grid')
    .then(r => r.ok ? r.json() : null)
    .catch(() => null),
]).then(([snap, gridSnap]) => {
  _propLastSnap  = snap;
  _propGridSnap  = gridSnap;
  renderPropMap(snap, gridSnap);
}).catch(err => console.warn('prop map fetch error:', err));
}

// ── Map initialisation (lazy — called on first tab activation) ────────────────

function initPropMap() {
if (_propMapInited) return;
_propMapInited = true;

propMap = L.map('prop-map', {
  center:      [30, 0],
  zoom:        2,
  zoomControl: true,
});

L.tileLayer('https://{s}.tile.openstreetmap.org/{z}/{x}/{y}.png', {
  attribution: '© <a href="https://www.openstreetmap.org/copyright">OpenStreetMap</a> contributors',
  maxZoom: 18,
}).addTo(propMap);

// Initialise layer groups — only add to map if the toggle is on by default
_heatLayer    = L.layerGroup();
_contourLayer = L.layerGroup();
_polarLayer   = L.layerGroup();
_pathLayer    = L.layerGroup();
_gsLayer      = L.layerGroup();
if (_showPaths) _pathLayer.addTo(propMap);
if (_showGS)   _gsLayer.addTo(propMap);

buildGreyline();

// Load GS data (populates selector + markers)
loadPropGSData();

// Initial data fetch
fetchAndRenderPropMap();

// Auto-refresh every 60 s
_propRefreshTimer = setInterval(fetchAndRenderPropMap, 60_000);
// Refresh grey line every 5 min
setInterval(buildGreyline, 5 * 60_000);

// Invalidate Leaflet size whenever the propagation tab becomes active.
// Without this, the map renders at 0×0 if it was initialised while hidden,
// and tiles don't load correctly after switching away and back.
document.addEventListener('tabchange', (e) => {
  if (e.detail === 'propagation' && propMap) {
    setTimeout(() => propMap.invalidateSize(), 0);
  }
});
}

// ── Toolbar event wiring ──────────────────────────────────────────────────────

function initPropToolbar() {
// Mode buttons
document.querySelectorAll('.prop-mode-btn').forEach(btn => {
  btn.addEventListener('click', () => {
    document.querySelectorAll('.prop-mode-btn').forEach(b => b.classList.remove('prop-mode-btn--active'));
    btn.classList.add('prop-mode-btn--active');
    _propMode = btn.dataset.mode;

    // Show/hide band vs GS selector
    const bandGroup = document.getElementById('prop-band-group');
    const gsGroup   = document.getElementById('prop-gs-group');
    if (bandGroup) bandGroup.style.display = _propMode === 'polar' ? 'none' : '';
    if (gsGroup)   gsGroup.style.display   = _propMode === 'polar' ? '' : 'none';

    if (_propLastSnap) renderPropMap(_propLastSnap, _propGridSnap);
  });
});

// Band selector
const bandSel = document.getElementById('prop-band-select');
if (bandSel) {
  bandSel.addEventListener('change', () => {
    _propBand = bandSel.value;
    if (_propLastSnap) renderPropMap(_propLastSnap, _propGridSnap);
  });
}

// GS selector (polar mode)
const gsSel = document.getElementById('prop-gs-select');
if (gsSel) {
  gsSel.addEventListener('change', () => {
    _propGSID = gsSel.value ? parseInt(gsSel.value, 10) : null;
    if (_propLastSnap) renderPropMap(_propLastSnap, _propGridSnap);
  });
}

// Layer toggles
const pathsCb    = document.getElementById('prop-lyr-paths');
const gsCb       = document.getElementById('prop-lyr-gs');
const greylineCb = document.getElementById('prop-lyr-greyline');

if (pathsCb) {
  pathsCb.addEventListener('change', () => {
    _showPaths = pathsCb.checked;
    if (!propMap || !_pathLayer) return;
    if (_showPaths) { if (!propMap.hasLayer(_pathLayer)) _pathLayer.addTo(propMap); }
    else            { if (propMap.hasLayer(_pathLayer))  _pathLayer.remove(); }
  });
}
if (gsCb) {
  gsCb.addEventListener('change', () => {
    _showGS = gsCb.checked;
    if (!propMap || !_gsLayer) return;
    if (_showGS) { if (!propMap.hasLayer(_gsLayer)) _gsLayer.addTo(propMap); }
    else         { if (propMap.hasLayer(_gsLayer))  _gsLayer.remove(); }
  });
}
if (greylineCb) {
  greylineCb.addEventListener('change', () => {
    _showGreyline = greylineCb.checked;
    if (!propMap || !_greylineLayer) return;
    if (_showGreyline) { if (!propMap.hasLayer(_greylineLayer)) _greylineLayer.addTo(propMap); }
    else               { if (propMap.hasLayer(_greylineLayer))  _greylineLayer.remove(); }
  });
}

// Manual refresh button
const refreshBtn = document.getElementById('prop-refresh-btn');
if (refreshBtn) {
  refreshBtn.addEventListener('click', () => {
    if (_propMapInited) fetchAndRenderPropMap();
  });
}

// Matrix filter
const filterEl = document.getElementById('prop-filter');
const clearEl  = document.getElementById('prop-filter-clear');
if (filterEl) {
  filterEl.addEventListener('input', () => {
    propFilterTerm = filterEl.value.trim().toLowerCase();
    if (_propLastData) renderPropagationTab(_propLastData);
  });
}
if (clearEl) {
  clearEl.addEventListener('click', () => {
    if (filterEl) filterEl.value = '';
    propFilterTerm = '';
    if (_propLastData) renderPropagationTab(_propLastData);
  });
}
}

// ── Matrix renderer (unchanged logic from original propagation.js) ────────────

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
const byACMap = {};
for (const p of paths) {
  if (!byACMap[p.aircraft_key]) byACMap[p.aircraft_key] = {};
  byACMap[p.aircraft_key][p.gs_id] = p;
}

// Apply filter
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

const countEl = document.getElementById('prop-count-label');
if (countEl) countEl.textContent = propFilterTerm ? `${acKeys.length} / ${totalAC}` : `${totalAC}`;

const visibleGSIds = new Set();
for (const key of acKeys) {
  for (const gsId of Object.keys(byACMap[key])) {
    visibleGSIds.add(parseInt(gsId, 10));
  }
}
const allGSIds = [...visibleGSIds].sort((a, b) => a - b);

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

html += `<div class="prop-matrix-wrap"><table class="prop-matrix">`;
html += `<thead><tr><th class="prop-ac-col">Aircraft</th>`;
for (const gsId of allGSIds) {
  const loc = (typeof gsNames !== 'undefined' && gsNames[gsId]) || `GS ${gsId}`;
  html += `<th class="prop-gs-col" title="${escProp(loc)}">GS ${gsId}<div class="prop-gs-name">${escProp(loc.split(',')[0])}</div></th>`;
}
html += `</tr></thead><tbody>`;

for (const acKey of acKeys) {
  const acPaths = byACMap[acKey];
  const anyPath = Object.values(acPaths)[0];
  const label   = [anyPath.reg, anyPath.flight, anyPath.icao ? `(${anyPath.icao})` : '']
    .filter(Boolean).join(' ') || acKey;

  html += `<tr><td class="prop-ac-cell mono">${escProp(label)}</td>`;
  for (const gsId of allGSIds) {
    const p = acPaths[gsId];
    if (p) {
      const sig = p.sig_level ? p.sig_level.toFixed(1) : '?';
      const freq = p.freq_khz ? (p.freq_khz / 1000).toFixed(3) + ' MHz' : '';
      const cls  = p.sig_level >= -30 ? 'prop-cell--strong'
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

// ── Public entry point called from app.js tab-switch logic ────────────────────

/**
* Called the first time the Propagation tab is activated, and on subsequent
* activations to trigger a data refresh.
*/
function loadPropagationTab() {
if (!_propMapInited) {
  // First activation — initialise the map (Leaflet needs the div to be visible)
  initPropMap();
  initPropToolbar();
} else {
  // Subsequent activations — just refresh data
  fetchAndRenderPropMap();
}
}

/**
* Called from app.js whenever a new propagation SSE event arrives
* (or on the periodic /propagation poll) while the tab is visible.
* Kept for backward compatibility — the map auto-refreshes on its own timer.
*/
function onPropagationUpdate(snap) {
if (!_propMapInited) return; // tab not yet opened
_propLastSnap = snap;
// Use cached grid snap — the map's own 60s timer will refresh both together
renderPropMap(snap, _propGridSnap);
}

