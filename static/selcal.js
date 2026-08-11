/* -----------------------------------------------------------------------
   UberSDR HFDL — SELCAL tab

   HF aeronautical voice channels: live audio plus decoded selective calls.

   The launcher holds one receiver session per configured channel and decodes
   SELCAL on all of them continuously.  This tab lets the user listen to one
   of those channels at a time; listening is a pure fan-out from the launcher,
   so it neither starts nor stops any decoding.

   Data sources:
     GET /selcal                 channel status + spot history (polled)
     GET /selcal/audio?ch=ID     WebSocket: per packet, an int16 LE signal
                                 level in centi-dB followed by mono u-law audio
     /events SSE "selcal"        newly decoded calls, pushed
   ----------------------------------------------------------------------- */

'use strict';

const SELCAL_MAX_SPOTS = 500;

// Signal meter scaling, in dB of baseband power over noise density as reported
// by the receiver.  An idle HF voice channel sits around 30–35 dB, so the bar
// starts empty there rather than at 0; a strong signal reaches 60.
const SELCAL_SNR_MIN    = 30; // bar empty at or below
const SELCAL_SNR_MAX    = 60; // bar full at or above
const SELCAL_SNR_OK     = 40; // amber above this
const SELCAL_SNR_STRONG = 50; // green above this

// Squelch. The threshold is per channel and shares the meter scale, so a slider
// position can be read directly against the channel's signal bar. The bottom of
// the range is "off" rather than a 30 dB gate, since an idle channel already
// sits at 30–35 dB and a gate there would never close anyway.
const SELCAL_SQUELCH_OFF   = SELCAL_SNR_MIN;
const SELCAL_SQUELCH_DWELL = 0.5;   // seconds continuously below the threshold before muting
const SELCAL_SQUELCH_RAMP  = 0.015; // gain ramp, long enough to avoid clicks
const SELCAL_AUTO_WINDOW   = 3.0;   // seconds of history the Auto button averages
const SELCAL_AUTO_MARGIN   = 3;     // dB Auto sets above that average
const SELCAL_SNR_HISTORY   = 10.0;  // seconds of per-channel history retained
const SELCAL_SQUELCH_STORE = 'selcalSquelch';

// Per-channel squelch thresholds, persisted so they survive a page reload.
let selcalSquelch = {};
try {
  selcalSquelch = JSON.parse(localStorage.getItem(SELCAL_SQUELCH_STORE)) || {};
} catch (e) { selcalSquelch = {}; }

function selcalSquelchFor(id) {
  const v = selcalSquelch[id];
  return typeof v === 'number' ? v : SELCAL_SQUELCH_OFF;
}

function selcalSetSquelch(id, db) {
  selcalSquelch[id] = db;
  try { localStorage.setItem(SELCAL_SQUELCH_STORE, JSON.stringify(selcalSquelch)); }
  catch (e) { /* private browsing — the setting just will not persist */ }
}

// Rolling SNR history per channel: { id: [{t, snr}, …] }.  Fed at ~50 Hz from
// the audio stream for whichever channel is being listened to, and every couple
// of seconds from the status stream for all of them, so Auto works on any
// channel — just at a coarser resolution for the ones that are not playing.
const selcalSnrHistory = {};

function selcalPushSNR(id, snr) {
  if (typeof snr !== 'number' || !isFinite(snr)) return;
  const now = performance.now() / 1000;
  const hist = selcalSnrHistory[id] || (selcalSnrHistory[id] = []);
  hist.push({ t: now, snr });
  const cutoff = now - SELCAL_SNR_HISTORY;
  while (hist.length && hist[0].t < cutoff) hist.shift();
}

// selcalAverageSNR returns the mean over the last `window` seconds, or null.
function selcalAverageSNR(id, window) {
  const hist = selcalSnrHistory[id];
  if (!hist || !hist.length) return null;
  const cutoff = performance.now() / 1000 - window;
  const recent = hist.filter(s => s.t >= cutoff);
  const use = recent.length ? recent : hist.slice(-1);
  return use.reduce((a, s) => a + s.snr, 0) / use.length;
}

// Newest first — mirrors the order /selcal returns.
let selcalSpots = [];
let selcalChannels = [];
let selcalFilterTerm = '';
let selcalAudioAvailable = false;

// ---- Audio playback --------------------------------------------------------

// Audio is relayed as 8-bit G.711 µ-law (one byte per sample) rather than
// 16-bit PCM, halving the upload cost per listener. Decoding is a 256-entry
// table lookup built once at load.
const SELCAL_ULAW_TABLE = (() => {
  const t = new Float32Array(256);
  for (let i = 0; i < 256; i++) {
    const u = ~i & 0xFF;
    const mantissa = u & 0x0F;
    const exponent = (u >> 4) & 0x07;
    let sample = (((mantissa << 3) + 0x84) << exponent) - 0x84;
    if (u & 0x80) sample = -sample;
    t[i] = sample / 32768;
  }
  return t;
})();

// Streamed PCM is played by scheduling each arriving packet as an
// AudioBufferSourceNode at a running playhead.  A small lead keeps playback
// clear of network jitter; if the playhead drifts too far ahead (a stalled
// tab, say) it is reset rather than allowed to grow unbounded.
const SELCAL_BUFFER_LEAD = 0.25; // seconds of scheduling lead
const SELCAL_MAX_LEAD    = 1.5;  // resync threshold

let selcalWS = null;
let selcalCtx = null;
let selcalGain = null;        // user volume
let selcalSquelchGain = null; // squelch gate, scheduled in step with the audio
let selcalRate = 12000;
let selcalPlayhead = 0;
let selcalListeningId = null;

// Squelch gate state for the channel currently playing.
let selcalGateOpen = true;
let selcalGateBelowSince = null; // audio-context time the level first fell below the threshold
let selcalLastMeterPaint = 0;

// The signal level arrives in a two-byte header on each audio frame; the
// sentinel means the receiver supplied no measurement for that packet.
const SELCAL_SNR_UNAVAILABLE = -32768;

function selcalStopAudio() {
  if (selcalWS) {
    const ws = selcalWS;
    selcalWS = null;      // suppress the onclose status update
    try { ws.close(); } catch (e) { /* already closing */ }
  }
  if (selcalCtx) {
    const ctx = selcalCtx;
    selcalCtx = null;
    selcalGain = null;
    selcalSquelchGain = null;
    try { ctx.close(); } catch (e) { /* already closed */ }
  }
  selcalListeningId = null;
  selcalPlayhead = 0;
  updateSelcalAudioUI();
  renderSelcalChannels();
}

function selcalStartAudio(id) {
  if (selcalListeningId === id) { selcalStopAudio(); return; }
  selcalStopAudio();

  const AudioCtx = window.AudioContext || window.webkitAudioContext;
  if (!AudioCtx) {
    if (typeof showToast === 'function') showToast('This browser cannot play audio', 8000);
    return;
  }

  selcalListeningId = id;
  selcalCtx = new AudioCtx();
  // source → squelch gate → volume → output.  Keeping the gate on its own node
  // means squelch scheduling and the volume slider cannot fight each other.
  selcalGain = selcalCtx.createGain();
  selcalGain.gain.value = selcalVolume();
  selcalGain.connect(selcalCtx.destination);
  selcalSquelchGain = selcalCtx.createGain();
  selcalSquelchGain.gain.value = 1;
  selcalSquelchGain.connect(selcalGain);
  selcalGateOpen = true;
  selcalGateBelowSince = null;
  // Browsers start the context suspended until a user gesture; this call is
  // made from the click handler, so resuming here is permitted.
  if (selcalCtx.state === 'suspended') selcalCtx.resume();

  const scheme = location.protocol === 'https:' ? 'wss:' : 'ws:';
  const url = `${scheme}//${location.host}${BASE_PATH}/selcal/audio?ch=${encodeURIComponent(id)}`;
  const ws = new WebSocket(url);
  ws.binaryType = 'arraybuffer';
  selcalWS = ws;

  ws.onmessage = (ev) => {
    if (typeof ev.data === 'string') {
      try {
        const meta = JSON.parse(ev.data);
        if (meta.type === 'format' && meta.rate) selcalRate = meta.rate;
      } catch (e) { /* ignore malformed metadata */ }
      return;
    }
    selcalPlayFrame(new DataView(ev.data));
  };

  ws.onerror = () => { /* onclose reports it */ };

  ws.onclose = () => {
    if (selcalWS !== ws) return; // superseded or stopped deliberately
    selcalStopAudio();
    if (typeof showToast === 'function') showToast('Audio stream closed', 8000);
  };

  updateSelcalAudioUI();
  renderSelcalChannels();
}

// selcalPlayFrame decodes one relay frame — a two-byte signal-level header
// followed by µ-law samples — schedules it for playback, and moves the squelch
// gate in step with it.
function selcalPlayFrame(view) {
  if (!selcalCtx || view.byteLength <= 2) return;

  const raw = view.getInt16(0, true);
  const snr = raw === SELCAL_SNR_UNAVAILABLE ? null : raw / 100;

  const n = view.byteLength - 2;
  const floats = new Float32Array(n);
  for (let i = 0; i < n; i++) floats[i] = SELCAL_ULAW_TABLE[view.getUint8(2 + i)];

  const buf = selcalCtx.createBuffer(1, n, selcalRate);
  buf.copyToChannel(floats, 0);

  const src = selcalCtx.createBufferSource();
  src.buffer = buf;
  src.connect(selcalSquelchGain);

  const now = selcalCtx.currentTime;
  if (selcalPlayhead < now + 0.02 || selcalPlayhead > now + SELCAL_MAX_LEAD) {
    selcalPlayhead = now + SELCAL_BUFFER_LEAD; // prime, or resync after a stall
  }
  const startAt = selcalPlayhead;
  src.start(startAt);
  selcalPlayhead += buf.duration;

  selcalApplySquelch(snr, startAt, buf.duration);

  // The listened channel gets a level on every packet, so its meter can track
  // the signal at a proper refresh rate instead of waiting for the 2 s status
  // stream.  Repaint is throttled; the history is kept at full resolution.
  if (snr !== null && selcalListeningId) {
    selcalPushSNR(selcalListeningId, snr);
    const ch = selcalChannels.find(c => c.id === selcalListeningId);
    if (ch) {
      ch.snr_db = snr;
      const wall = performance.now();
      if (wall - selcalLastMeterPaint > 100) {
        selcalLastMeterPaint = wall;
        selcalPaintChannelRow(selcalListeningId);
      }
    }
  }
}

// selcalApplySquelch decides whether the gate is open for this packet and
// schedules the gain change at the instant the packet will actually be heard,
// rather than when it arrived.
//
// The gate opens the instant the level reaches the threshold, but only closes
// once the level has stayed below it continuously for SELCAL_SQUELCH_DWELL.
// That dwell is what stops it chattering, and it also rides over the gaps
// between syllables rather than clipping them.  It replaces a hysteresis band,
// which would have been the wrong tool here: a signal parked just under the
// threshold would have held the gate open indefinitely instead of closing.
function selcalApplySquelch(snr, startAt, duration) {
  const threshold = selcalSquelchFor(selcalListeningId);
  let open;

  if (threshold <= SELCAL_SQUELCH_OFF || snr === null) {
    // Squelch off, or no measurement to judge by: never mute silently.
    open = true;
    selcalGateBelowSince = null;
  } else if (snr >= threshold) {
    open = true;
    selcalGateBelowSince = null;
  } else {
    if (selcalGateBelowSince === null) selcalGateBelowSince = startAt;
    open = (startAt - selcalGateBelowSince) < SELCAL_SQUELCH_DWELL;
  }

  if (open !== selcalGateOpen) {
    selcalGateOpen = open;
    selcalPaintChannelRow(selcalListeningId);
  }
  selcalSquelchGain.gain.setTargetAtTime(open ? 1 : 0, startAt, SELCAL_SQUELCH_RAMP);
}

function selcalVolume() {
  const el = document.getElementById('selcal-volume');
  return el ? Math.pow(el.value / 100, 2) : 0.64; // perceptual taper
}

// Receiver-wide listener accounting: upload bandwidth is shared across all
// channels, so the figure that matters is the total, not the per-channel count.
let selcalListenerStats = null;

function updateSelcalListenerUI() {
  const el = document.getElementById('selcal-listeners-label');
  if (!el || !selcalListenerStats) return;
  const s = selcalListenerStats;
  const cap = s.listeners_max > 0 ? `/${s.listeners_max}` : '';
  el.textContent = `${s.listeners_total}${cap} listening · ${s.upload_kbps} kbit/s up`;
  el.title = s.listeners_max > 0
    ? `${s.listener_kbps} kbit/s per listener; up to ${s.upload_max_kbps} kbit/s at the ${s.listeners_max}-listener limit (shared across all channels)`
    : `${s.listener_kbps} kbit/s per listener; no listener limit set`;
  el.classList.toggle('selcal-listeners--full',
    s.listeners_max > 0 && s.listeners_total >= s.listeners_max);
}

function updateSelcalAudioUI() {
  const label = document.getElementById('selcal-listening-label');
  const stop = document.getElementById('selcal-stop');
  if (!label || !stop) return;

  if (selcalListeningId) {
    const ch = selcalChannels.find(c => c.id === selcalListeningId);
    const name = ch ? (ch.label ? `${ch.freq_khz} kHz — ${ch.label}` : `${ch.freq_khz} kHz`) : selcalListeningId;
    label.textContent = '🔊 Listening: ' + name;
    stop.hidden = false;
  } else {
    label.textContent = 'Not listening';
    stop.hidden = true;
  }
}

// ---- Rendering -------------------------------------------------------------

// selcalSnrClass grades a calibrated signal level on the same thresholds as the
// channel meters, so a figure quoted for a call and one shown on a meter mean
// the same thing.
function selcalSnrClass(snr) {
  if (snr >= SELCAL_SNR_STRONG) return 'selcal-snr selcal-snr--strong';
  if (snr >= SELCAL_SNR_OK) return 'selcal-snr selcal-snr--ok';
  return 'selcal-snr selcal-snr--weak';
}

function selcalSignalBar(ch) {
  if (!ch.connected) return '<span class="selcal-sig-none">—</span>';
  // snr_db is baseband power minus noise density, as measured by the receiver
  // and delivered on every audio packet.
  const snr = ch.snr_db || 0;
  const span = SELCAL_SNR_MAX - SELCAL_SNR_MIN;
  const pct = Math.max(0, Math.min(100, ((snr - SELCAL_SNR_MIN) / span) * 100));
  let cls = 'weak';
  if (snr >= SELCAL_SNR_STRONG) cls = 'strong';
  else if (snr >= SELCAL_SNR_OK) cls = 'ok';
  const title = `Signal ${(ch.level_db || 0).toFixed(1)} dBFS, noise ${(ch.noise_db || 0).toFixed(1)} dBFS`;
  return `<span class="selcal-sig" title="${esc(title)}">
    <span class="selcal-sig-bar"><span class="selcal-sig-fill selcal-sig-fill--${cls}" style="width:${pct.toFixed(0)}%"></span></span>
    <span class="selcal-sig-val">${snr.toFixed(0)} dB</span>
  </span>`;
}

// Called from app.js when a "selcal_signal" SSE event arrives — live levels for
// every configured channel, pushed a few times a minute regardless of whether
// anyone is listening. Decode history already held locally is preserved.
function updateSelcalSignals(statuses) {
  if (!Array.isArray(statuses)) return;

  statuses.forEach(st => {
    const ch = selcalChannels.find(c => c.id === st.id);
    if (!ch) return;
    ch.connected = st.connected;
    ch.level_db = st.level_db;
    ch.noise_db = st.noise_db;
    // The channel being listened to already has a far higher-resolution level
    // from the audio stream, so do not overwrite it with this coarser one.
    if (st.id !== selcalListeningId) ch.snr_db = st.snr_db;
    if (st.connected) selcalPushSNR(st.id, st.snr_db);
    ch.listeners = st.listeners;
    ch.packets = st.packets;
    ch.reconnections = st.reconnections;
    ch.sample_rate = st.sample_rate;
  });

  // Keep the receiver-wide listener/upload figures current between polls.
  if (selcalListenerStats) {
    selcalListenerStats.listeners_total = statuses.reduce((n, s) => n + (s.listeners || 0), 0);
    selcalListenerStats.upload_kbps =
      selcalListenerStats.listeners_total * selcalListenerStats.listener_kbps;
    updateSelcalListenerUI();
  }

  // If the set of channels changed under us (a restart with new config), pick
  // the new list up on the next poll rather than rendering a mismatch.
  if (statuses.length !== selcalChannels.length) return;

  if (document.getElementById('tab-selcal')?.classList.contains('active')) {
    renderSelcalChannels();
  }
}

// The channel table is rebuilt only when the set of channels changes.  Signal
// levels arrive many times a second for the channel being listened to, and
// replacing the table's markup that often would destroy a slider mid-drag and
// throw away focus, so everything volatile is updated cell by cell instead.
let selcalRowsKey = null;

function selcalChannelRowsKey() {
  return selcalChannels.map(c => c.id).join('|') + (selcalAudioAvailable ? '|a' : '');
}

function renderSelcalChannels() {
  const tbody = document.getElementById('selcal-channels-tbody');
  if (!tbody) return;

  if (!selcalChannels.length) {
    tbody.innerHTML = '<tr><td colspan="8" class="empty">No channels configured — set SELCAL_FREQS to enable.</td></tr>';
    selcalRowsKey = null;
    return;
  }

  const key = selcalChannelRowsKey();
  if (key !== selcalRowsKey) {
    selcalRowsKey = key;
    buildSelcalChannelRows(tbody);
  }
  selcalChannels.forEach(ch => selcalPaintChannelRow(ch.id));
}

function buildSelcalChannelRows(tbody) {
  tbody.innerHTML = selcalChannels.map(ch => {
    const id = esc(ch.id);
    const label = ch.decode
      ? esc(ch.label || '')
      : `${esc(ch.label || '')} <span class="selcal-badge selcal-badge--audio">audio only</span>`;

    const listen = selcalAudioAvailable
      ? `<button class="selcal-listen-btn" data-ch="${id}"></button>`
      : '<span class="selcal-sig-none">—</span>';

    // The squelch threshold shares the meter scale, so the slider position can
    // be read straight against the signal bar in the next column.
    const squelch = selcalAudioAvailable
      ? `<span class="selcal-sq">
           <input type="range" class="selcal-sq-slider" data-ch="${id}"
                  min="${SELCAL_SNR_MIN}" max="${SELCAL_SNR_MAX}" step="1"
                  value="${selcalSquelchFor(ch.id)}"
                  title="Mute this channel below this signal level" />
           <span class="selcal-sq-val" data-role="sq-val"></span>
           <button class="selcal-sq-auto" data-ch="${id}"
                   title="Set the threshold to ${SELCAL_AUTO_MARGIN} dB above this channel's average level over the last ${SELCAL_AUTO_WINDOW} s">Auto</button>
           <span class="selcal-sq-state" data-role="sq-state"></span>
         </span>`
      : '<span class="selcal-sig-none">—</span>';

    return `<tr data-ch="${id}">
      <td>${listen}</td>
      <td class="selcal-freq">${ch.freq_khz.toLocaleString()} kHz</td>
      <td>${label}</td>
      <td data-role="status"></td>
      <td data-role="signal"></td>
      <td>${squelch}</td>
      <td data-role="last"></td>
      <td data-role="count"></td>
    </tr>`;
  }).join('');

  tbody.querySelectorAll('.selcal-listen-btn').forEach(btn => {
    btn.addEventListener('click', () => selcalStartAudio(btn.dataset.ch));
  });
  tbody.querySelectorAll('.selcal-sq-slider').forEach(el => {
    el.addEventListener('input', () => {
      selcalSetSquelch(el.dataset.ch, Number(el.value));
      selcalPaintChannelRow(el.dataset.ch);
    });
  });
  tbody.querySelectorAll('.selcal-sq-auto').forEach(btn => {
    btn.addEventListener('click', () => selcalAutoSquelch(btn.dataset.ch));
  });
}

// selcalAutoSquelch sets the threshold a little above the channel's recent
// average — the average being the ambient noise floor, so the margin puts the
// gate just clear of it and anything louder than ambient opens it.
function selcalAutoSquelch(id) {
  let avg = selcalAverageSNR(id, SELCAL_AUTO_WINDOW);
  if (avg === null) {
    const ch = selcalChannels.find(c => c.id === id);
    avg = ch && typeof ch.snr_db === 'number' ? ch.snr_db : null;
  }
  if (avg === null) {
    if (typeof showToast === 'function') showToast('No signal history yet for that channel', 6000);
    return;
  }
  const v = Math.max(SELCAL_SNR_MIN,
                     Math.min(SELCAL_SNR_MAX, Math.round(avg + SELCAL_AUTO_MARGIN)));
  selcalSetSquelch(id, v);

  const slider = document.querySelector(`.selcal-sq-slider[data-ch="${CSS.escape(id)}"]`);
  if (slider) slider.value = v;
  selcalPaintChannelRow(id);
}

// selcalPaintChannelRow refreshes only the volatile cells of one row.
function selcalPaintChannelRow(id) {
  if (!id) return;
  const tbody = document.getElementById('selcal-channels-tbody');
  if (!tbody) return;
  const tr = tbody.querySelector(`tr[data-ch="${CSS.escape(id)}"]`);
  const ch = selcalChannels.find(c => c.id === id);
  if (!tr || !ch) return;

  const fmtDT = typeof fmtDateTime === 'function' ? fmtDateTime : () => '—';
  const listening = selcalListeningId === ch.id;

  const btn = tr.querySelector('.selcal-listen-btn');
  if (btn) {
    btn.textContent = listening ? '⏹' : '▶';
    btn.classList.toggle('selcal-listen-btn--on', listening);
    btn.disabled = !ch.connected;
    btn.title = listening ? 'Stop listening' : 'Listen to this channel';
  }

  const set = (role, html) => {
    const el = tr.querySelector(`[data-role="${role}"]`);
    if (el) el.innerHTML = html;
  };

  set('status', ch.connected
    ? '<span class="selcal-state selcal-state--up">Connected</span>'
    : '<span class="selcal-state selcal-state--down">Offline</span>');
  set('signal', selcalSignalBar(ch));
  set('last', ch.last_code
    ? `<span class="selcal-code">${esc(ch.last_code)}</span> <span class="selcal-muted">${fmtDT(ch.last_time)}</span>`
    : '<span class="selcal-muted">—</span>');
  set('count', ch.decode ? (ch.count || 0).toLocaleString() : '—');

  const threshold = selcalSquelchFor(id);
  const valEl = tr.querySelector('[data-role="sq-val"]');
  if (valEl) {
    valEl.textContent = threshold <= SELCAL_SQUELCH_OFF ? 'Off' : `${threshold} dB`;
    valEl.classList.toggle('selcal-muted', threshold <= SELCAL_SQUELCH_OFF);
  }

  // The open/closed lamp only means anything for the channel being played;
  // the others have no audio to gate.
  const stateEl = tr.querySelector('[data-role="sq-state"]');
  if (stateEl) {
    if (!listening || threshold <= SELCAL_SQUELCH_OFF) {
      stateEl.textContent = '';
      stateEl.title = '';
    } else if (selcalGateOpen) {
      stateEl.textContent = '●';
      stateEl.className = 'selcal-sq-state selcal-sq-state--open';
      stateEl.title = 'Squelch open — audio passing';
    } else {
      stateEl.textContent = '○';
      stateEl.className = 'selcal-sq-state selcal-sq-state--closed';
      stateEl.title = 'Squelch closed — audio muted';
    }
  }
}

function renderSelcalSpots() {
  const tbody = document.getElementById('selcal-spots-tbody');
  if (!tbody) return;

  const term = selcalFilterTerm.trim().toUpperCase();
  const rows = term
    ? selcalSpots.filter(s => s.code.toUpperCase().includes(term))
    : selcalSpots;

  const countEl = document.getElementById('selcal-count-label');
  if (countEl) {
    countEl.textContent = term
      ? `${rows.length} of ${selcalSpots.length} calls`
      : `${selcalSpots.length} call${selcalSpots.length === 1 ? '' : 's'}`;
  }

  if (!rows.length) {
    tbody.innerHTML = `<tr><td colspan="5" class="empty">${term ? 'No calls match that filter…' : 'No SELCAL decoded yet…'}</td></tr>`;
    return;
  }

  const fmtDT = typeof fmtDateTime === 'function' ? fmtDateTime : () => '—';

  tbody.innerHTML = rows.map(s => {
    const heard = (s.channels || []).map(c => {
      const where = c.label ? `${c.freq_khz} kHz (${c.label})` : `${c.freq_khz} kHz`;
      const lvl = c.has_snr ? `${c.snr_db.toFixed(1)} dB signal` : 'signal not measured';
      return `<span class="selcal-chip" title="${esc(where)} — ${lvl}, ${c.margin_db.toFixed(1)} dB decode margin">${c.freq_khz}</span>`;
    }).join('');
    const badge = s.selcal32 ? '<span class="selcal-badge selcal-badge--32">SELCAL32</span>' : '';
    // Signal is the receiver's own measurement over the burst — the same scale
    // as the channel meters.  Margin is the detector's separate confidence
    // figure and is shown as a tooltip rather than a second column.
    const snr = s.has_snr
      ? `<span class="${selcalSnrClass(s.snr_db)}">${s.snr_db.toFixed(1)} dB</span>`
      : '<span class="selcal-muted">—</span>';
    return `<tr title="Decode margin ${s.margin_db.toFixed(1)} dB above the in-band noise floor">
      <td>${fmtDT(s.time)}</td>
      <td><span class="selcal-code">${esc(s.code)}</span> ${badge}</td>
      <td class="selcal-chips">${heard}</td>
      <td>${snr}</td>
      <td>${s.offset_hz > 0 ? '+' : ''}${s.offset_hz.toFixed(1)} Hz</td>
    </tr>`;
  }).join('');
}

// ---- Data ------------------------------------------------------------------

// Called from app.js when a "selcal" SSE event arrives.
function addSelcalSpot(spot) {
  if (!spot || !spot.code) return;
  selcalSpots.unshift(spot);
  if (selcalSpots.length > SELCAL_MAX_SPOTS) selcalSpots.length = SELCAL_MAX_SPOTS;

  // Reflect the call on each channel it landed on straight away, rather than
  // waiting for the next poll.
  (spot.channels || []).forEach(c => {
    const ch = selcalChannels.find(x => x.freq_khz === c.freq_khz);
    if (ch) {
      ch.last_code = spot.code;
      ch.last_time = spot.time;
      ch.count = (ch.count || 0) + 1;
    }
  });

  if (document.getElementById('tab-selcal')?.classList.contains('active')) {
    renderSelcalSpots();
    renderSelcalChannels();
  }
  if (typeof showToast === 'function') {
    const freqs = (spot.channels || []).map(c => c.freq_khz).join(', ');
    showToast(`SELCAL ${spot.code} on ${freqs} kHz`, 8000);
  }
}

function loadSelcalTab() {
  return fetch(BASE_PATH + '/selcal')
    .then(r => r.json())
    .then(data => {
      selcalChannels = data.channels || [];
      selcalSpots = data.spots || [];
      selcalAudioAvailable = !!data.audio;
      selcalListenerStats = {
        listeners_total: data.listeners_total || 0,
        listeners_max: data.listeners_max || 0,
        listener_kbps: data.listener_kbps || 0,
        upload_kbps: data.upload_kbps || 0,
        upload_max_kbps: data.upload_max_kbps || 0,
      };
      updateSelcalListenerUI();

      // The tab only appears when channels are configured, so an install that
      // has not enabled SELCAL sees no change to the dashboard.
      const btn = document.getElementById('tab-btn-selcal');
      if (btn) btn.hidden = !(data.enabled && selcalChannels.length);

      renderSelcalChannels();
      renderSelcalSpots();
      updateSelcalAudioUI();
    })
    .catch(() => { /* endpoint absent on older builds — leave the tab hidden */ });
}

// ---- Init ------------------------------------------------------------------

document.addEventListener('DOMContentLoaded', () => {
  const filter = document.getElementById('selcal-filter');
  if (filter) {
    filter.addEventListener('input', () => {
      selcalFilterTerm = filter.value;
      renderSelcalSpots();
    });
  }
  const clear = document.getElementById('selcal-filter-clear');
  if (clear) {
    clear.addEventListener('click', () => {
      selcalFilterTerm = '';
      if (filter) filter.value = '';
      renderSelcalSpots();
    });
  }
  const stop = document.getElementById('selcal-stop');
  if (stop) stop.addEventListener('click', selcalStopAudio);

  const vol = document.getElementById('selcal-volume');
  if (vol) {
    vol.addEventListener('input', () => {
      if (selcalGain) selcalGain.gain.value = selcalVolume();
    });
  }

  loadSelcalTab();
  // Signal levels, connection state and listener counts arrive over SSE every
  // couple of seconds, and decoded calls are pushed as they happen; this poll
  // is only a backstop that reconciles the channel list and spot history.
  setInterval(loadSelcalTab, 30_000);

  document.addEventListener('tabchange', (e) => {
    if (e.detail === 'selcal') loadSelcalTab();
  });
});

// Stop audio when the page goes away so the launcher drops the listener slot.
window.addEventListener('beforeunload', () => {
  if (selcalWS) try { selcalWS.close(); } catch (e) { /* ignore */ }
});
