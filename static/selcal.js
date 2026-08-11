/* -----------------------------------------------------------------------
   UberSDR HFDL — SELCAL tab

   HF aeronautical voice channels: live audio plus decoded selective calls.

   The launcher holds one receiver session per configured channel and decodes
   SELCAL on all of them continuously.  This tab lets the user listen to one
   of those channels at a time; listening is a pure fan-out from the launcher,
   so it neither starts nor stops any decoding.

   Data sources:
     GET /selcal                 channel status + spot history (polled)
     GET /selcal/audio?ch=ID     WebSocket, mono signed 16-bit LE PCM
     /events SSE "selcal"        newly decoded calls, pushed
   ----------------------------------------------------------------------- */

'use strict';

const SELCAL_MAX_SPOTS = 500;

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
let selcalGain = null;
let selcalRate = 12000;
let selcalPlayhead = 0;
let selcalListeningId = null;

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
  selcalGain = selcalCtx.createGain();
  selcalGain.gain.value = selcalVolume();
  selcalGain.connect(selcalCtx.destination);
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
    selcalPlayULaw(new Uint8Array(ev.data));
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

function selcalPlayULaw(bytes) {
  if (!selcalCtx || !bytes.length) return;

  const floats = new Float32Array(bytes.length);
  for (let i = 0; i < bytes.length; i++) floats[i] = SELCAL_ULAW_TABLE[bytes[i]];

  const buf = selcalCtx.createBuffer(1, floats.length, selcalRate);
  buf.copyToChannel(floats, 0);

  const src = selcalCtx.createBufferSource();
  src.buffer = buf;
  src.connect(selcalGain);

  const now = selcalCtx.currentTime;
  if (selcalPlayhead < now + 0.02 || selcalPlayhead > now + SELCAL_MAX_LEAD) {
    selcalPlayhead = now + SELCAL_BUFFER_LEAD; // prime, or resync after a stall
  }
  src.start(selcalPlayhead);
  selcalPlayhead += buf.duration;
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

function selcalSignalBar(ch) {
  if (!ch.connected) return '<span class="selcal-sig-none">—</span>';
  // snr_db is baseband power minus noise density, as measured by the receiver
  // and delivered on every audio packet.
  const snr = ch.snr_db || 0;
  // 0–40 dB maps to the full bar.
  const pct = Math.max(0, Math.min(100, (snr / 40) * 100));
  let cls = 'weak';
  if (snr >= 25) cls = 'strong';
  else if (snr >= 12) cls = 'ok';
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
    ch.snr_db = st.snr_db;
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

function renderSelcalChannels() {
  const tbody = document.getElementById('selcal-channels-tbody');
  if (!tbody) return;

  if (!selcalChannels.length) {
    tbody.innerHTML = '<tr><td colspan="7" class="empty">No channels configured — set SELCAL_FREQS to enable.</td></tr>';
    return;
  }

  const fmtDT = typeof fmtDateTime === 'function' ? fmtDateTime : () => '—';

  tbody.innerHTML = selcalChannels.map(ch => {
    const listening = selcalListeningId === ch.id;
    const btn = selcalAudioAvailable
      ? `<button class="selcal-listen-btn${listening ? ' selcal-listen-btn--on' : ''}"
                 data-ch="${esc(ch.id)}" title="${listening ? 'Stop listening' : 'Listen to this channel'}"
                 ${ch.connected ? '' : 'disabled'}>${listening ? '⏹' : '▶'}</button>`
      : '<span class="selcal-sig-none">—</span>';

    const status = ch.connected
      ? '<span class="selcal-state selcal-state--up">Connected</span>'
      : '<span class="selcal-state selcal-state--down">Offline</span>';

    const last = ch.last_code
      ? `<span class="selcal-code">${esc(ch.last_code)}</span> <span class="selcal-muted">${fmtDT(ch.last_time)}</span>`
      : '<span class="selcal-muted">—</span>';

    const label = ch.decode
      ? esc(ch.label || '')
      : `${esc(ch.label || '')} <span class="selcal-badge selcal-badge--audio">audio only</span>`;

    return `<tr>
      <td>${btn}</td>
      <td class="selcal-freq">${ch.freq_khz.toLocaleString()} kHz</td>
      <td>${label}</td>
      <td>${status}</td>
      <td>${selcalSignalBar(ch)}</td>
      <td>${last}</td>
      <td>${ch.decode ? (ch.count || 0).toLocaleString() : '—'}</td>
    </tr>`;
  }).join('');

  tbody.querySelectorAll('.selcal-listen-btn').forEach(btn => {
    btn.addEventListener('click', () => selcalStartAudio(btn.dataset.ch));
  });
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
      const t = c.label ? `${c.freq_khz} kHz (${c.label})` : `${c.freq_khz} kHz`;
      return `<span class="selcal-chip" title="${esc(t)} — ${c.snr_db.toFixed(1)} dB">${c.freq_khz}</span>`;
    }).join('');
    const badge = s.selcal32 ? '<span class="selcal-badge selcal-badge--32">SELCAL32</span>' : '';
    return `<tr>
      <td>${fmtDT(s.time)}</td>
      <td><span class="selcal-code">${esc(s.code)}</span> ${badge}</td>
      <td class="selcal-chips">${heard}</td>
      <td>${s.snr_db.toFixed(1)} dB</td>
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
