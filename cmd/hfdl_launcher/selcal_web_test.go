package main

import (
	"encoding/binary"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// newTestSelcalServer wires the SELCAL routes onto a test server with one
// channel, without connecting to any receiver.
func newTestSelcalServer(t *testing.T, audioEnabled bool, maxListeners int) (*httptest.Server, *selcalManager, *selcalStore) {
	t.Helper()
	store := newSelcalStore(audioEnabled, func(string) {})
	mgr := newSelcalManager(
		[]selcalChannelCfg{{FreqKHz: 8906, Label: "NAT-A", Decode: true}},
		"http://receiver.invalid:8080", "", store, maxListeners, "", 8)
	mgr.channels[0].hub.setFormat(12000, 1)

	mux := http.NewServeMux()
	registerSelcalRoutes(mux, mgr, store, audioEnabled)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, mgr, store
}

func TestSelcalSnapshotEndpoint(t *testing.T) {
	srv, mgr, store := newTestSelcalServer(t, true, 4)

	store.record(mgr.channels[0].cfg,
		selcalDetection{Code: "AB-CD", MarginDB: 41.3, OffsetHz: 0.4}, 47.5, true, time.Now())
	store.flush("AB-CD") // publish immediately rather than waiting out the dedupe window

	resp, err := http.Get(srv.URL + "/selcal")
	if err != nil {
		t.Fatalf("GET /selcal: %v", err)
	}
	defer resp.Body.Close()

	var snap SelcalSnapshot
	if err := json.NewDecoder(resp.Body).Decode(&snap); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !snap.Enabled || !snap.Audio {
		t.Errorf("enabled=%v audio=%v, want both true", snap.Enabled, snap.Audio)
	}
	if len(snap.Channels) != 1 || snap.Channels[0].FreqKHz != 8906 {
		t.Fatalf("unexpected channels: %+v", snap.Channels)
	}
	if snap.Channels[0].LastCode != "AB-CD" || snap.Channels[0].Count != 1 {
		t.Errorf("channel history not recorded: %+v", snap.Channels[0])
	}
	if len(snap.Spots) != 1 || snap.Spots[0].Code != "AB-CD" {
		t.Fatalf("unexpected spots: %+v", snap.Spots)
	}
	// The quoted signal is the receiver's calibrated level, on the same scale as
	// the channel meters; the detector's margin is carried separately.
	if snap.Spots[0].SNRDB != 47.5 || !snap.Spots[0].HasSNR {
		t.Errorf("spot signal = %g (has=%v), want 47.5", snap.Spots[0].SNRDB, snap.Spots[0].HasSNR)
	}
	if snap.Spots[0].MarginDB != 41.3 {
		t.Errorf("spot margin = %g, want 41.3", snap.Spots[0].MarginDB)
	}
	// 12 kHz mono µ-law is one byte per sample.
	if snap.ListenerKbps != 96 {
		t.Errorf("listener_kbps = %d, want 96", snap.ListenerKbps)
	}
	if snap.ListenersMax != 4 || snap.UploadMaxKbps != 384 {
		t.Errorf("listeners_max=%d upload_max_kbps=%d, want 4/384",
			snap.ListenersMax, snap.UploadMaxKbps)
	}
}

func TestSelcalAudioStreamsULaw(t *testing.T) {
	srv, mgr, _ := newTestSelcalServer(t, true, 4)
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/selcal/audio?ch=8906"

	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	// The first message describes the stream.
	if err := conn.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	mt, msg, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("read hello: %v", err)
	}
	if mt != websocket.TextMessage {
		t.Fatalf("first message type = %d, want text", mt)
	}
	var hello struct {
		Type    string  `json:"type"`
		Codec   string  `json:"codec"`
		Framing string  `json:"framing"`
		Rate    int     `json:"rate"`
		Chans   int     `json:"channels"`
		FreqKHz float64 `json:"freq_khz"`
	}
	if err := json.Unmarshal(msg, &hello); err != nil {
		t.Fatalf("hello: %v", err)
	}
	if hello.Type != "format" || hello.Codec != "ulaw" || hello.Framing != "snr16le+ulaw" ||
		hello.Rate != 12000 || hello.Chans != 1 || hello.FreqKHz != 8906 {
		t.Fatalf("unexpected hello: %+v", hello)
	}

	// Wait for the listener to register before pushing audio through it.
	deadline := time.Now().Add(2 * time.Second)
	for mgr.channels[0].hub.listeners() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}

	samples := []int16{0, 1000, -1000, 32767, -32768}
	mgr.channels[0].handlePacket(&pcmPacket{
		Samples: samples, Rate: 12000, Channels: 1,
		BasebandPowerDB: -100, NoiseDensityDB: -142.5, HasSignal: true, // 42.5 dB
	}, new([]float64))

	if err := conn.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	mt, audio, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("read audio: %v", err)
	}
	if mt != websocket.BinaryMessage {
		t.Fatalf("audio message type = %d, want binary", mt)
	}
	// Two-byte SNR header, then one µ-law byte per sample.
	if len(audio) != audioFrameHeaderLen+len(samples) {
		t.Fatalf("got %d bytes for %d samples, want a 2-byte header plus one byte each",
			len(audio), len(samples))
	}
	// The header carries the receiver's level so a listener-side squelch can
	// react per packet rather than waiting on the 2 s status stream.
	if snr := int16(binary.LittleEndian.Uint16(audio[0:2])); snr != 4250 {
		t.Errorf("SNR header = %d centi-dB, want 4250 (42.5 dB)", snr)
	}
	for i, s := range samples {
		if audio[audioFrameHeaderLen+i] != linearToULaw(s) {
			t.Errorf("byte %d = %#x, want %#x", i, audio[audioFrameHeaderLen+i], linearToULaw(s))
		}
	}
}

func TestAudioFrameSNRUnavailable(t *testing.T) {
	// With no receiver measurement the header must be an unambiguous sentinel,
	// not a plausible-looking 0 dB.
	frame := encodeAudioFrame(&pcmPacket{Samples: []int16{1, 2, 3}, Rate: 12000, Channels: 1})
	if snr := int16(binary.LittleEndian.Uint16(frame[0:2])); snr != audioSNRUnavailable {
		t.Errorf("SNR header = %d, want the unavailable sentinel %d", snr, audioSNRUnavailable)
	}
	if len(frame) != audioFrameHeaderLen+3 {
		t.Errorf("frame length %d, want %d", len(frame), audioFrameHeaderLen+3)
	}
}

func TestSelcalAudioRejections(t *testing.T) {
	// Unknown channel.
	srv, _, _ := newTestSelcalServer(t, true, 1)
	resp, err := http.Get(srv.URL + "/selcal/audio?ch=9999")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("unknown channel: status %d, want 404", resp.StatusCode)
	}

	// Audio relay disabled.
	srv2, _, _ := newTestSelcalServer(t, false, 1)
	resp2, err := http.Get(srv2.URL + "/selcal/audio?ch=8906")
	if err != nil {
		t.Fatal(err)
	}
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusForbidden {
		t.Errorf("relay disabled: status %d, want 403", resp2.StatusCode)
	}

	// Budget exhausted: the one permitted listener holds the only slot.
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/selcal/audio?ch=8906"
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	resp3, err := http.Get(srv.URL + "/selcal/audio?ch=8906")
	if err != nil {
		t.Fatal(err)
	}
	resp3.Body.Close()
	if resp3.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("over budget: status %d, want 503", resp3.StatusCode)
	}
}

func TestSelcalDedupeMergesChannels(t *testing.T) {
	var events []string
	store := newSelcalStore(true, func(msg string) { events = append(events, msg) })

	natA := selcalChannelCfg{FreqKHz: 8906, Label: "NAT-A"}
	natA2 := selcalChannelCfg{FreqKHz: 13306, Label: "NAT-A"}
	now := time.Now()

	// The same call keyed simultaneously on two frequencies of one family.
	store.record(natA, selcalDetection{Code: "AB-CD", MarginDB: 38}, 41.0, true, now)
	store.record(natA2, selcalDetection{Code: "AB-CD", MarginDB: 45}, 52.5, true, now)
	// A duplicate report from a channel already recorded must not add a second entry.
	store.record(natA, selcalDetection{Code: "AB-CD", MarginDB: 39}, 42.0, true, now)
	store.flush("AB-CD")

	snap := store.snapshot(nil)
	if len(snap.Spots) != 1 {
		t.Fatalf("expected the calls to merge into 1 spot, got %d", len(snap.Spots))
	}
	spot := snap.Spots[0]
	if len(spot.Channels) != 2 {
		t.Fatalf("expected 2 channels on the merged spot, got %+v", spot.Channels)
	}
	if spot.SNRDB != 52.5 || !spot.HasSNR {
		t.Errorf("merged signal = %g (has=%v), want the best copy (52.5)", spot.SNRDB, spot.HasSNR)
	}
	if spot.MarginDB != 45 {
		t.Errorf("merged margin = %g, want the best of the two (45)", spot.MarginDB)
	}
	if len(events) != 1 {
		t.Errorf("expected 1 SSE event for the merged spot, got %d", len(events))
	}
}

// TestDisabledWhenNoFreqsConfigured covers every way the config can be empty.
// An unset, blank or whitespace-only SELCAL_FREQS must be a no-op — no error,
// no receiver sessions, and a dashboard tab that stays hidden — while a genuine
// typo must still fail loudly at startup rather than being silently ignored.
func TestDisabledWhenNoFreqsConfigured(t *testing.T) {
	for _, spec := range []string{"", "   ", ",", ",,,", " , , "} {
		got, err := parseSelcalFreqs(spec)
		if err != nil {
			t.Errorf("parseSelcalFreqs(%q) errored: %v — an empty config must be a no-op", spec, err)
		}
		if len(got) != 0 {
			t.Errorf("parseSelcalFreqs(%q) produced %d channel(s), want none", spec, len(got))
		}
	}

	// A malformed value is a different matter and must be rejected.
	for _, spec := range []string{"notanumber", "150:TooLow", "8906,8906", "8906,,bad"} {
		if _, err := parseSelcalFreqs(spec); err == nil {
			t.Errorf("parseSelcalFreqs(%q) was accepted, want an error", spec)
		}
	}

	// With no channels the manager is never constructed, so the routes must
	// tolerate a nil manager rather than panicking.
	store := newSelcalStore(true, func(string) {})
	mux := http.NewServeMux()
	registerSelcalRoutes(mux, nil, store, true)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/selcal")
	if err != nil {
		t.Fatalf("GET /selcal: %v", err)
	}
	defer resp.Body.Close()

	var snap SelcalSnapshot
	if err := json.NewDecoder(resp.Body).Decode(&snap); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if snap.Enabled {
		t.Error("enabled=true with no channels — the dashboard tab would appear empty")
	}
	if len(snap.Channels) != 0 || snap.ListenersMax != 0 {
		t.Errorf("unexpected snapshot when disabled: %+v", snap)
	}

	audio, err := http.Get(srv.URL + "/selcal/audio?ch=8906")
	if err != nil {
		t.Fatalf("GET /selcal/audio: %v", err)
	}
	audio.Body.Close()
	if audio.StatusCode != http.StatusNotFound {
		t.Errorf("/selcal/audio returned %d when disabled, want 404", audio.StatusCode)
	}
}
