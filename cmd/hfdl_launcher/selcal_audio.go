package main

// selcal_audio.go — UberSDR USB audio client, listener fan-out and burst recording.
//
// One WebSocket session per configured voice channel is held open for the life
// of the process.  Each session feeds two consumers:
//
//	UberSDR ──usb/pcm-zstd──▶ selcalChannel ──┬──▶ SELCAL detector ──▶ selcalStore
//	     (1 session per freq)                  └──▶ audioHub ──▶ N browser listeners
//
// Decoding therefore runs continuously on every enabled channel whether or not
// anybody is listening, and browser listeners cost no extra receiver sessions —
// they attach to the launcher, not to UberSDR.

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"log"
	"math"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"github.com/klauspost/compress/zstd"
)

// USB passband requested from the receiver.  The SELCAL tones span
// 312.6–1557.8 Hz, so the standard voice passband already contains the whole
// tone set; it is requested explicitly rather than relying on server defaults
// so the audio a listener hears is exactly the audio that was decoded.
const (
	selcalBandwidthLow  = 50
	selcalBandwidthHigh = 2700
)

// Reconnection behaviour, mirroring the supervision the IQ pipelines get.
//
// A dropped connection is not the only failure mode worth handling: a session
// can also go silent while the TCP connection stays open, in which case a bare
// ReadMessage would block forever and the channel would sit dead without ever
// retrying.  Audio arrives continuously on a healthy session — the receiver
// sends silence packets even when an audio gate is closed — so no traffic at
// all for selcalReadTimeout means the stream is gone, whatever the socket says.
var (
	selcalReadTimeout    = 60 * time.Second // no traffic for this long ⇒ reconnect
	selcalPingInterval   = 20 * time.Second // must be comfortably shorter than the timeout
	selcalMaxBackoff     = 60 * time.Second
	selcalHealthySession = 30 * time.Second // a session lasting this long resets the backoff
)

// ---------------------------------------------------------------------------
// PCM binary packet decoder
// ---------------------------------------------------------------------------
// Mirrors the hybrid binary format produced by the UberSDR server (see
// pcm_binary.go there, and the same decoder in ubersdr_iq's main.go).  Full
// headers carry the stream metadata; minimal headers follow while it is
// unchanged.  Samples are big-endian int16.

const (
	selcalMagicFull    = 0x5043 // "PC"
	selcalMagicMinimal = 0x504D // "PM"

	// selcalSignalUnavailable is the sentinel the server sends when it has no
	// signal-quality measurement for a packet.
	selcalSignalUnavailable = -998.0
)

// pcmPacket is one decoded audio packet.
//
// For non-IQ modes the server forces a full version-2 header on every packet
// (including the silence it substitutes when an audio gate is closed), so
// BasebandPowerDB and NoiseDensityDB arrive continuously rather than only on
// format changes.  That gives a receiver-measured signal level for every
// channel at all times, with no dependence on anyone listening.
type pcmPacket struct {
	Samples         []int16
	Rate            int
	Channels        int
	BasebandPowerDB float64
	NoiseDensityDB  float64
	HasSignal       bool
}

type pcmDecoder struct {
	zd           *zstd.Decoder
	lastRate     int
	lastChannels int
}

func newPCMDecoder() (*pcmDecoder, error) {
	zd, err := zstd.NewReader(nil)
	if err != nil {
		return nil, fmt.Errorf("zstd init: %w", err)
	}
	return &pcmDecoder{zd: zd}, nil
}

func (d *pcmDecoder) close() { d.zd.Close() }

// decode decompresses and parses one packet, returning native-order samples
// along with any signal-quality measurement the header carried.
func (d *pcmDecoder) decode(data []byte) (*pcmPacket, error) {
	var err error
	data, err = d.zd.DecodeAll(data, nil)
	if err != nil {
		return nil, fmt.Errorf("zstd decompress: %w", err)
	}
	if len(data) < 4 {
		return nil, fmt.Errorf("packet too short (%d bytes)", len(data))
	}

	pkt := &pcmPacket{}
	var raw []byte

	switch binary.LittleEndian.Uint16(data[0:2]) {
	case selcalMagicFull:
		version := data[2]
		headerLen := 29
		if version >= 2 {
			headerLen = 37
		}
		if len(data) < headerLen {
			return nil, fmt.Errorf("full-header packet too short (%d < %d)", len(data), headerLen)
		}
		pkt.Rate = int(binary.LittleEndian.Uint32(data[20:24]))
		pkt.Channels = int(data[24])
		if version >= 2 {
			power := float64(math.Float32frombits(binary.LittleEndian.Uint32(data[25:29])))
			noise := float64(math.Float32frombits(binary.LittleEndian.Uint32(data[29:33])))
			if power > selcalSignalUnavailable && noise > selcalSignalUnavailable {
				pkt.BasebandPowerDB, pkt.NoiseDensityDB, pkt.HasSignal = power, noise, true
			}
		}
		raw = data[headerLen:]
		d.lastRate, d.lastChannels = pkt.Rate, pkt.Channels

	case selcalMagicMinimal:
		if len(data) < 13 {
			return nil, fmt.Errorf("minimal-header packet too short (%d bytes)", len(data))
		}
		raw = data[13:]
		pkt.Rate, pkt.Channels = d.lastRate, d.lastChannels
		if pkt.Rate == 0 || pkt.Channels == 0 {
			return nil, fmt.Errorf("minimal header received before full header")
		}

	default:
		return nil, fmt.Errorf("unknown magic 0x%04X", binary.LittleEndian.Uint16(data[0:2]))
	}

	pkt.Samples = make([]int16, len(raw)/2)
	for i := range pkt.Samples {
		pkt.Samples[i] = int16(binary.BigEndian.Uint16(raw[i*2:]))
	}
	return pkt, nil
}

// ---------------------------------------------------------------------------
// Listener fan-out
// ---------------------------------------------------------------------------

// listenerBudget caps concurrent listeners across *all* channels at once.
//
// The constraint being protected is upload bandwidth, which is shared by the
// whole receiver rather than divided per channel: ten listeners on one
// frequency and one listener on each of ten frequencies cost exactly the same
// to serve.  A per-channel cap would bound neither.
type listenerBudget struct {
	mu    sync.Mutex
	max   int // 0 = unlimited
	inUse int
}

func newListenerBudget(max int) *listenerBudget {
	return &listenerBudget{max: max}
}

func (b *listenerBudget) acquire() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.max > 0 && b.inUse >= b.max {
		return false
	}
	b.inUse++
	return true
}

func (b *listenerBudget) release() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.inUse > 0 {
		b.inUse--
	}
}

func (b *listenerBudget) current() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.inUse
}

// audioHub broadcasts audio to the browsers currently listening to one channel.
// Slow listeners are dropped rather than allowed to stall decoding.
type audioHub struct {
	mu       sync.Mutex
	subs     map[chan []byte]struct{}
	budget   *listenerBudget
	rate     int
	channels int
}

func newAudioHub(budget *listenerBudget) *audioHub {
	return &audioHub{subs: make(map[chan []byte]struct{}), budget: budget}
}

// subscribe registers a listener, or returns ok=false when the receiver-wide
// listener budget is exhausted.
func (h *audioHub) subscribe() (chan []byte, bool) {
	if !h.budget.acquire() {
		return nil, false
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	ch := make(chan []byte, 32)
	h.subs[ch] = struct{}{}
	return ch, true
}

func (h *audioHub) unsubscribe(ch chan []byte) {
	h.mu.Lock()
	_, existed := h.subs[ch]
	delete(h.subs, ch)
	h.mu.Unlock()
	if existed {
		h.budget.release()
	}
}

func (h *audioHub) listeners() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.subs)
}

// format reports the stream parameters last seen from the receiver.
func (h *audioHub) format() (rate, channels int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.rate, h.channels
}

func (h *audioHub) setFormat(rate, channels int) {
	h.mu.Lock()
	h.rate, h.channels = rate, channels
	h.mu.Unlock()
}

// ---------------------------------------------------------------------------
// Relay codec
// ---------------------------------------------------------------------------

// Audio is relayed as 8-bit G.711 µ-law rather than the 16-bit PCM it arrives
// as, halving upload bandwidth per listener (12 kHz mono: 192 kbit/s → 96
// kbit/s).  µ-law's logarithmic companding gives roughly 12-bit perceived
// dynamic range, which is ample for AGC'd HF SSB voice, and the browser
// decodes it with a 256-entry lookup table.
//
// Only the relay path is companded; the SELCAL detector always runs on the
// original 16-bit samples.
func linearToULaw(sample int16) byte {
	const (
		bias = 0x84 // added so the segment search never sees zero
		clip = 8159 // full scale in the 14-bit domain µ-law is defined over
	)
	// Upper bound of each of the eight µ-law segments, in the 14-bit domain.
	segEnd := [8]int32{0x3F, 0x7F, 0xFF, 0x1FF, 0x3FF, 0x7FF, 0xFFF, 0x1FFF}

	// µ-law is defined on 14-bit samples; drop the low two bits of the 16-bit
	// input, which the decoder's shift restores.
	v := int32(sample) >> 2

	mask := byte(0xFF) // positive
	if v < 0 {
		v = -v
		mask = 0x7F // negative
	}
	if v > clip {
		v = clip
	}
	v += bias >> 2

	seg := 8
	for i, limit := range segEnd {
		if v <= limit {
			seg = i
			break
		}
	}
	if seg >= 8 {
		return 0x7F ^ mask // clipped
	}
	return (byte(seg<<4) | byte((v>>uint(seg+1))&0x0F)) ^ mask
}

// Relay frame layout: a two-byte little-endian signed header carrying the
// receiver's signal-to-noise measurement for this packet in hundredths of a dB,
// followed by one µ-law byte per sample.
//
// The level travels with the audio rather than only on the two-second status
// stream so that a listener-side squelch can react within a packet (~20 ms).
// Gating on a status feed that slow would clip the opening of every
// transmission.  Two bytes per packet is under 1% overhead.
const (
	audioFrameHeaderLen = 2
	audioSNRUnavailable = math.MinInt16 // receiver supplied no measurement
	audioSNRScale       = 100.0         // centi-dB per dB
)

// encodeAudioFrame builds one relay frame from a decoded packet.
func encodeAudioFrame(pkt *pcmPacket) []byte {
	snr := int64(audioSNRUnavailable)
	if pkt.HasSignal {
		v := math.Round((pkt.BasebandPowerDB - pkt.NoiseDensityDB) * audioSNRScale)
		// Clamp rather than let an absurd reading wrap into a plausible one.
		if v > math.MaxInt16 {
			v = math.MaxInt16
		} else if v <= audioSNRUnavailable {
			v = audioSNRUnavailable + 1
		}
		snr = int64(v)
	}

	frame := make([]byte, audioFrameHeaderLen+len(pkt.Samples))
	binary.LittleEndian.PutUint16(frame[0:2], uint16(int16(snr)))
	for i, s := range pkt.Samples {
		frame[audioFrameHeaderLen+i] = linearToULaw(s)
	}
	return frame
}

// broadcast delivers one encoded frame to every listener, skipping any whose
// buffer has backed up.
func (h *audioHub) broadcast(pcm []byte) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for ch := range h.subs {
		select {
		case ch <- pcm:
		default:
		}
	}
}

// ---------------------------------------------------------------------------
// Burst recorder
// ---------------------------------------------------------------------------

// burstRecorder keeps a rolling window of recent audio so that a WAV covering
// a detection — including the run-up to it — can be written after the fact.
// Enabled only when -selcal-record-dir is set; intended for verifying and
// tuning the detector against real signals rather than for routine use.
type burstRecorder struct {
	dir     string
	rate    int
	ring    []int16
	size    int
	head    int
	filled  bool
	written int
}

func newBurstRecorder(dir string, rate int, seconds int) *burstRecorder {
	size := rate * seconds
	return &burstRecorder{dir: dir, rate: rate, ring: make([]int16, size), size: size}
}

func (r *burstRecorder) push(samples []int16) {
	for _, s := range samples {
		r.ring[r.head] = s
		r.head++
		if r.head == r.size {
			r.head = 0
			r.filled = true
		}
	}
}

// snapshot returns the buffered audio in chronological order.
func (r *burstRecorder) snapshot() []int16 {
	if !r.filled {
		out := make([]int16, r.head)
		copy(out, r.ring[:r.head])
		return out
	}
	out := make([]int16, 0, r.size)
	out = append(out, r.ring[r.head:]...)
	out = append(out, r.ring[:r.head]...)
	return out
}

// save writes the current window as a mono 16-bit WAV named after the channel,
// code and time.
func (r *burstRecorder) save(chanID, code string, when time.Time) {
	samples := r.snapshot()
	if len(samples) == 0 {
		return
	}
	safe := strings.NewReplacer("/", "-", " ", "_", ":", "").Replace(code)
	name := fmt.Sprintf("selcal_%skHz_%s_%s.wav", chanID, safe, when.UTC().Format("20060102T150405Z"))
	path := filepath.Join(r.dir, name)

	f, err := os.Create(path) //nolint:gosec
	if err != nil {
		log.Printf("selcal: recorder: create %s: %v", path, err)
		return
	}
	defer f.Close()

	data := make([]byte, len(samples)*2)
	for i, s := range samples {
		binary.LittleEndian.PutUint16(data[i*2:], uint16(s))
	}
	if err := writeMonoWAVHeader(f, r.rate, uint32(len(data))); err != nil {
		log.Printf("selcal: recorder: header %s: %v", path, err)
		return
	}
	if _, err := f.Write(data); err != nil {
		log.Printf("selcal: recorder: write %s: %v", path, err)
		return
	}
	r.written++
	log.Printf("selcal: recorded %s (%.1f s)", path, float64(len(samples))/float64(r.rate))
}

// writeMonoWAVHeader writes a 44-byte canonical WAV header for mono 16-bit PCM.
// (iqrecorder.go's writeWAVHeader is hardcoded to two channels for CS16 IQ.)
func writeMonoWAVHeader(w *os.File, sampleRateHz int, dataBytes uint32) error {
	const (
		numChannels   = 1
		bitsPerSample = 16
		audioFormat   = 1
	)
	buf := make([]byte, 44)
	copy(buf[0:4], "RIFF")
	binary.LittleEndian.PutUint32(buf[4:8], 36+dataBytes)
	copy(buf[8:12], "WAVE")
	copy(buf[12:16], "fmt ")
	binary.LittleEndian.PutUint32(buf[16:20], 16)
	binary.LittleEndian.PutUint16(buf[20:22], audioFormat)
	binary.LittleEndian.PutUint16(buf[22:24], numChannels)
	binary.LittleEndian.PutUint32(buf[24:28], uint32(sampleRateHz))
	binary.LittleEndian.PutUint32(buf[28:32], uint32(sampleRateHz*numChannels*bitsPerSample/8))
	binary.LittleEndian.PutUint16(buf[32:34], numChannels*bitsPerSample/8)
	binary.LittleEndian.PutUint16(buf[34:36], bitsPerSample)
	copy(buf[36:40], "data")
	binary.LittleEndian.PutUint32(buf[40:44], dataBytes)
	_, err := w.Write(buf)
	return err
}

// ---------------------------------------------------------------------------
// Channel
// ---------------------------------------------------------------------------

// selcalChannel owns one receiver session: it reconnects as needed, decodes
// SELCAL, and fans audio out to listeners.
type selcalChannel struct {
	cfg        selcalChannelCfg
	ubersdrURL string
	password   string
	store      *selcalStore
	hub        *audioHub
	recordDir  string
	recordSecs int

	mu            sync.Mutex
	connected     bool
	lastPacket    time.Time
	levelDB       float64
	noiseDB       float64
	hasSignal     bool // levels came from the receiver rather than the detector
	reconnections int
	packets       int64

	det      *selcalDetector
	rec      *burstRecorder
	stopOnce sync.Once
	stopCh   chan struct{}

	// Rolling history of the receiver's per-packet signal measurements, on the
	// same sample clock the detector reports bursts against.  A decoded call is
	// then quoted with the receiver's own calibrated level for the moment it was
	// transmitted, rather than with an FFT-derived figure on a different scale.
	samplesFed int64
	snrHist    []snrSample
	snrHead    int
}

// snrSample is one packet's receiver-measured signal level, tagged with the
// stretch of the audio stream it covers.
type snrSample struct {
	startSec float64
	endSec   float64
	snrDB    float64
	valid    bool
}

// selcalSNRHistory is how many packets of signal history to retain.  Audio
// arrives in ~20 ms packets, so this covers roughly ten seconds — comfortably
// longer than the ~2.2 s burst plus the detector's reporting lag.
const selcalSNRHistory = 512

// pushSNR records one packet's measurement.
func (c *selcalChannel) pushSNR(s snrSample) {
	if c.snrHist == nil {
		c.snrHist = make([]snrSample, selcalSNRHistory)
	}
	c.snrHist[c.snrHead] = s
	c.snrHead = (c.snrHead + 1) % selcalSNRHistory
}

// peakSNR returns the strongest receiver-measured level overlapping the given
// stretch of stream time.  The peak rather than the mean, because a burst is
// two tone pulses either side of a silent gap, and it is the tones whose level
// is being reported.
func (c *selcalChannel) peakSNR(startSec, endSec float64) (float64, bool) {
	best, found := 0.0, false
	for _, s := range c.snrHist {
		if !s.valid || s.endSec < startSec || s.startSec > endSec {
			continue
		}
		if !found || s.snrDB > best {
			best, found = s.snrDB, true
		}
	}
	return best, found
}

func newSelcalChannel(cfg selcalChannelCfg, ubersdrURL, password string, store *selcalStore,
	budget *listenerBudget, recordDir string, recordSecs int) *selcalChannel {
	return &selcalChannel{
		cfg:        cfg,
		ubersdrURL: ubersdrURL,
		password:   password,
		store:      store,
		hub:        newAudioHub(budget),
		recordDir:  recordDir,
		recordSecs: recordSecs,
		stopCh:     make(chan struct{}),
	}
}

// status is the per-channel JSON shape served at /selcal.
type selcalChannelStatus struct {
	ID            string  `json:"id"`
	FreqKHz       float64 `json:"freq_khz"`
	Label         string  `json:"label"`
	Decode        bool    `json:"decode"`
	Connected     bool    `json:"connected"`
	Listeners     int     `json:"listeners"`
	LevelDB       float64 `json:"level_db"`
	NoiseDB       float64 `json:"noise_db"`
	SNRDB         float64 `json:"snr_db"`
	LastPacket    int64   `json:"last_packet,omitempty"`
	Reconnections int     `json:"reconnections"`
	Packets       int64   `json:"packets"`
	SampleRate    int     `json:"sample_rate,omitempty"`
}

func (c *selcalChannel) status() selcalChannelStatus {
	c.mu.Lock()
	defer c.mu.Unlock()
	rate, _ := c.hub.format()
	st := selcalChannelStatus{
		ID:            c.cfg.ID(),
		FreqKHz:       c.cfg.FreqKHz,
		Label:         c.cfg.Label,
		Decode:        c.cfg.Decode,
		Connected:     c.connected,
		Listeners:     c.hub.listeners(),
		LevelDB:       c.levelDB,
		NoiseDB:       c.noiseDB,
		SNRDB:         c.levelDB - c.noiseDB,
		Reconnections: c.reconnections,
		Packets:       c.packets,
		SampleRate:    rate,
	}
	if !c.lastPacket.IsZero() {
		st.LastPacket = c.lastPacket.Unix()
	}
	return st
}

func (c *selcalChannel) stop() {
	c.stopOnce.Do(func() { close(c.stopCh) })
}

// run reconnects with exponential backoff until stopped.
func (c *selcalChannel) run() {
	retries := 0
	for {
		select {
		case <-c.stopCh:
			return
		default:
		}

		start := time.Now()
		if err := c.runOnce(); err != nil {
			log.Printf("selcal[%s]: %v", c.cfg.Name(), err)
		}

		c.mu.Lock()
		c.connected = false
		if time.Since(start) > selcalHealthySession {
			retries = 0 // the session was healthy for a while; reset backoff
		}
		c.reconnections++
		c.mu.Unlock()

		select {
		case <-c.stopCh:
			return
		default:
		}

		retries++
		shift := retries
		if shift > 6 {
			shift = 6
		}
		backoff := time.Duration(1<<uint(shift)) * time.Second
		if backoff > selcalMaxBackoff {
			backoff = selcalMaxBackoff
		}
		log.Printf("selcal[%s]: reconnecting in %.0fs", c.cfg.Name(), backoff.Seconds())
		select {
		case <-time.After(backoff):
		case <-c.stopCh:
			return
		}
	}
}

// wsURL builds the receiver WebSocket URL for this channel.
func (c *selcalChannel) wsURL(sessionID string) string {
	u, _ := url.Parse(c.ubersdrURL)
	scheme := "ws"
	if u.Scheme == "https" || u.Scheme == "wss" {
		scheme = "wss"
	}
	path := strings.TrimRight(u.Path, "/")
	if path == "" {
		path = "/ws"
	}
	q := url.Values{}
	q.Set("frequency", fmt.Sprintf("%d", c.cfg.FreqHz()))
	q.Set("mode", "usb")
	q.Set("format", "pcm-zstd")
	// Protocol version 2 adds the per-packet signal-quality fields; the server
	// defaults to version 1, which omits them.
	q.Set("version", "2")
	q.Set("bandwidthLow", fmt.Sprintf("%d", selcalBandwidthLow))
	q.Set("bandwidthHigh", fmt.Sprintf("%d", selcalBandwidthHigh))
	q.Set("user_session_id", sessionID)
	if c.password != "" {
		q.Set("password", c.password)
	}
	return fmt.Sprintf("%s://%s%s?%s", scheme, u.Host, path, q.Encode())
}

// httpBase derives the HTTP base URL for the pre-flight connection check.
func (c *selcalChannel) httpBase() string {
	u, _ := url.Parse(c.ubersdrURL)
	scheme := u.Scheme
	switch scheme {
	case "ws":
		scheme = "http"
	case "wss":
		scheme = "https"
	}
	return fmt.Sprintf("%s://%s", scheme, u.Host)
}

// checkConnection asks the receiver whether this client may connect.  A
// transport failure is treated as permission granted, matching ubersdr_iq: the
// WebSocket dial that follows will surface any real problem.
func (c *selcalChannel) checkConnection(sessionID string) error {
	body, _ := json.Marshal(map[string]string{
		"user_session_id": sessionID,
		"password":        c.password,
	})
	req, err := http.NewRequest("POST", c.httpBase()+"/connection", bytes.NewReader(body))
	if err != nil {
		return nil //nolint:nilerr // fall through to the dial
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "ubersdr_hfdl/1.0")

	resp, err := apiClient.Do(req)
	if err != nil {
		return nil //nolint:nilerr // receiver unreachable; let the dial report it
	}
	defer resp.Body.Close()

	var cr struct {
		Allowed bool   `json:"allowed"`
		Reason  string `json:"reason"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&cr); err != nil {
		return nil //nolint:nilerr
	}
	if !cr.Allowed {
		return fmt.Errorf("receiver rejected connection: %s", cr.Reason)
	}
	return nil
}

// runOnce holds one session open until it fails or the channel is stopped.
func (c *selcalChannel) runOnce() error {
	sessionID := uuid.New().String()
	if err := c.checkConnection(sessionID); err != nil {
		return err
	}

	hdr := http.Header{}
	hdr.Set("User-Agent", "ubersdr_hfdl/1.0")
	dialer := &websocket.Dialer{HandshakeTimeout: 10 * time.Second}
	conn, _, err := dialer.Dial(c.wsURL(sessionID), hdr)
	if err != nil {
		return fmt.Errorf("websocket dial: %w", err)
	}
	defer conn.Close()

	dec, err := newPCMDecoder()
	if err != nil {
		return err
	}
	defer dec.close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Any traffic at all — audio, the server's pong, or a protocol-level pong —
	// proves the session is alive and pushes the deadline back.
	resetDeadline := func() {
		_ = conn.SetReadDeadline(time.Now().Add(selcalReadTimeout))
	}
	resetDeadline()
	conn.SetPongHandler(func(string) error {
		resetDeadline()
		return nil
	})

	// Keepalive.
	go func() {
		t := time.NewTicker(selcalPingInterval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if err := conn.WriteJSON(map[string]string{"type": "ping"}); err != nil {
					return
				}
			}
		}
	}()

	// Close the socket promptly when the channel is stopped.
	go func() {
		select {
		case <-c.stopCh:
			conn.Close()
		case <-ctx.Done():
		}
	}()

	log.Printf("selcal[%s]: connected on %g kHz USB", c.cfg.Name(), c.cfg.FreqKHz)
	c.mu.Lock()
	c.connected = true
	c.mu.Unlock()

	floats := make([]float64, 0, 8192)

	for {
		select {
		case <-c.stopCh:
			return nil
		default:
		}

		msgType, msg, err := conn.ReadMessage()
		if err != nil {
			// Distinguish a stalled stream from an ordinary disconnect: both
			// reconnect, but they mean different things when reading the log.
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				return fmt.Errorf("no audio for %s — treating the session as stalled",
					selcalReadTimeout)
			}
			return fmt.Errorf("read: %w", err)
		}
		resetDeadline()

		switch msgType {
		case websocket.BinaryMessage:
			pkt, err := dec.decode(msg)
			if err != nil {
				log.Printf("selcal[%s]: decode: %v", c.cfg.Name(), err)
				continue
			}
			if len(pkt.Samples) == 0 || pkt.Rate == 0 {
				continue
			}
			c.handlePacket(pkt, &floats)

		case websocket.TextMessage:
			var m struct {
				Type  string `json:"type"`
				Error string `json:"error"`
			}
			if json.Unmarshal(msg, &m) == nil && m.Type == "error" {
				return fmt.Errorf("receiver error: %s", m.Error)
			}
		}
	}
}

// handlePacket relays one packet to listeners, records its signal level, then
// decodes it.
func (c *selcalChannel) handlePacket(pkt *pcmPacket, floats *[]float64) {
	// Relay first so listener latency does not depend on decode work.
	c.hub.setFormat(pkt.Rate, pkt.Channels)
	if c.hub.listeners() > 0 {
		c.hub.broadcast(encodeAudioFrame(pkt))
	}

	c.mu.Lock()
	c.lastPacket = time.Now()
	c.packets++
	// The receiver's own measurement, in calibrated dBFS.  It arrives on every
	// packet for USB, so channel levels stay live whether or not the channel is
	// being decoded or listened to.
	if pkt.HasSignal {
		c.levelDB = pkt.BasebandPowerDB
		c.noiseDB = pkt.NoiseDensityDB
		c.hasSignal = true
	}
	c.mu.Unlock()

	if !c.cfg.Decode {
		return
	}

	if c.det == nil || c.det.rate != pkt.Rate {
		c.det = newSelcalDetector(pkt.Rate)
		log.Printf("selcal[%s]: detector ready (%d Hz, %d-point FFT, %.2f Hz bins)",
			c.cfg.Name(), pkt.Rate, c.det.n, c.det.binHz)
		if c.recordDir != "" {
			c.rec = newBurstRecorder(c.recordDir, pkt.Rate, c.recordSecs)
		}
	}
	if c.rec != nil {
		c.rec.push(pkt.Samples)
	}

	// Log this packet's signal level against the stream position it covers,
	// before feeding the samples on, so a burst completing in this packet can
	// still look back over its own duration.
	startSec := float64(c.samplesFed) / float64(pkt.Rate)
	c.samplesFed += int64(len(pkt.Samples))
	c.pushSNR(snrSample{
		startSec: startSec,
		endSec:   float64(c.samplesFed) / float64(pkt.Rate),
		snrDB:    pkt.BasebandPowerDB - pkt.NoiseDensityDB,
		valid:    pkt.HasSignal,
	})

	*floats = (*floats)[:0]
	for _, s := range pkt.Samples {
		*floats = append(*floats, float64(s)/32768.0)
	}

	dets := c.det.feed(*floats)

	// Fall back to the detector's own in-band measurement only if the receiver
	// did not supply one (protocol version 1, or an older server).
	if !c.hasSignal {
		c.mu.Lock()
		c.levelDB = c.det.lastLevelDB
		c.noiseDB = c.det.lastNoiseDB
		c.mu.Unlock()
	}

	for _, d := range dets {
		now := time.Now()
		// Quote the receiver's own signal level for the moment of the burst.
		snrDB, hasSNR := c.peakSNR(d.StartSec, d.EndSec)
		snrText := "n/a"
		if hasSNR {
			snrText = fmt.Sprintf("%.1f dB", snrDB)
		}
		log.Printf("selcal[%s]: %s  signal=%s  margin=%.1f dB  offset=%+.1f Hz  span=%.2f s%s",
			c.cfg.Name(), d.Code, snrText, d.MarginDB, d.OffsetHz, d.Duration,
			map[bool]string{true: "  [SELCAL32]"}[d.Selcal32])
		c.store.record(c.cfg, d, snrDB, hasSNR, now)
		if c.rec != nil {
			c.rec.save(c.cfg.ID(), d.Code, now)
		}
	}
}

// ---------------------------------------------------------------------------
// Manager
// ---------------------------------------------------------------------------

// selcalManager owns every configured channel.
type selcalManager struct {
	channels []*selcalChannel
	store    *selcalStore
	budget   *listenerBudget
	stopCh   chan struct{}
	stopOnce sync.Once
}

func newSelcalManager(cfgs []selcalChannelCfg, ubersdrURL, password string, store *selcalStore,
	maxListeners int, recordDir string, recordSecs int) *selcalManager {
	// One budget shared by every channel — see listenerBudget.
	budget := newListenerBudget(maxListeners)
	m := &selcalManager{store: store, budget: budget, stopCh: make(chan struct{})}
	for _, cfg := range cfgs {
		m.channels = append(m.channels,
			newSelcalChannel(cfg, ubersdrURL, password, store, budget, recordDir, recordSecs))
	}
	return m
}

// selcalSignalInterval is how often live channel levels are pushed to the UI.
const selcalSignalInterval = 2 * time.Second

// start brings every channel up, staggered so the receiver is not hit with a
// burst of simultaneous session creations, and begins publishing signal levels.
func (m *selcalManager) start() {
	for i, ch := range m.channels {
		go func(ch *selcalChannel, delay time.Duration) {
			time.Sleep(delay)
			ch.run()
		}(ch, time.Duration(i)*300*time.Millisecond)
	}
	go m.publishSignals()
}

// publishSignals pushes per-channel signal levels on the same SSE stream that
// carries decoded calls.  Because every channel is streamed continuously, this
// works for all of them at once and does not depend on anyone listening.
func (m *selcalManager) publishSignals() {
	t := time.NewTicker(selcalSignalInterval)
	defer t.Stop()
	for {
		select {
		case <-m.stopCh:
			return
		case <-t.C:
		}
		if m.store.broadcast == nil {
			continue
		}
		payload, err := json.Marshal(sseEvent{Type: "selcal_signal", Data: m.statuses()})
		if err != nil {
			log.Printf("selcal: marshal signal update: %v", err)
			continue
		}
		m.store.broadcast(string(payload))
	}
}

func (m *selcalManager) stop() {
	m.stopOnce.Do(func() { close(m.stopCh) })
	for _, ch := range m.channels {
		ch.stop()
	}
}

// find returns the channel with the given ID.
func (m *selcalManager) find(id string) *selcalChannel {
	for _, ch := range m.channels {
		if ch.cfg.ID() == id {
			return ch
		}
	}
	return nil
}

func (m *selcalManager) statuses() []selcalChannelStatus {
	out := make([]selcalChannelStatus, 0, len(m.channels))
	for _, ch := range m.channels {
		out = append(out, ch.status())
	}
	return out
}
