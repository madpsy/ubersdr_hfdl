package main

// Live end-to-end check against a real receiver.  Skipped unless
// SELCAL_LIVE_URL is set, so the normal test run stays offline:
//
//	SELCAL_LIVE_URL=https://receiver.example.org \
//	SELCAL_LIVE_FREQS=8906:NAT-A,5505:Volmet \
//	SELCAL_LIVE_SECONDS=30 \
//	go test ./cmd/hfdl_launcher/ -run TestLiveReceiver -v
//
// It verifies the parts that cannot be exercised offline: the WebSocket
// handshake, the pcm-zstd/version-2 negotiation, continuous delivery of
// signal-quality data, and that audio actually flows.

import (
	"os"
	"strconv"
	"testing"
	"time"
)

func TestLiveReceiver(t *testing.T) {
	baseURL := os.Getenv("SELCAL_LIVE_URL")
	if baseURL == "" {
		t.Skip("set SELCAL_LIVE_URL to run the live receiver check")
	}

	spec := os.Getenv("SELCAL_LIVE_FREQS")
	if spec == "" {
		spec = "8906:NAT-A"
	}
	cfgs, err := parseSelcalFreqs(spec)
	if err != nil {
		t.Fatalf("SELCAL_LIVE_FREQS: %v", err)
	}

	secs := 25
	if v := os.Getenv("SELCAL_LIVE_SECONDS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			secs = n
		}
	}

	store := newSelcalStore(true, func(msg string) { t.Logf("SSE: %s", msg) })
	mgr := newSelcalManager(cfgs, baseURL, os.Getenv("SELCAL_LIVE_PASS"), store, 10, "", 8)
	mgr.start()
	defer mgr.stop()

	deadline := time.Now().Add(time.Duration(secs) * time.Second)
	for time.Now().Before(deadline) {
		time.Sleep(5 * time.Second)
		for _, st := range mgr.statuses() {
			t.Logf("%8.1f kHz %-14s connected=%-5v pkts=%-6d rate=%-6d level=%7.1f noise=%7.1f snr=%5.1f dB",
				st.FreqKHz, st.Label, st.Connected, st.Packets, st.SampleRate,
				st.LevelDB, st.NoiseDB, st.SNRDB)
		}
	}

	// Every configured channel must have connected and be receiving audio with
	// live signal-quality data attached.
	for _, ch := range mgr.channels {
		st := ch.status()
		if !st.Connected {
			t.Errorf("%g kHz never connected", st.FreqKHz)
			continue
		}
		if st.Packets == 0 {
			t.Errorf("%g kHz connected but received no audio packets", st.FreqKHz)
		}
		if st.SampleRate != 12000 {
			t.Errorf("%g kHz sample rate = %d, want 12000 (usb)", st.FreqKHz, st.SampleRate)
		}
		ch.mu.Lock()
		hasSignal := ch.hasSignal
		ch.mu.Unlock()
		if !hasSignal {
			t.Errorf("%g kHz received no signal-quality data — check the version=2 request",
				st.FreqKHz)
		}
		if st.NoiseDB == 0 || st.LevelDB == 0 {
			t.Errorf("%g kHz signal levels look unpopulated (level=%g noise=%g)",
				st.FreqKHz, st.LevelDB, st.NoiseDB)
		}
	}

	for _, s := range store.snapshot(mgr.statuses()).Spots {
		t.Logf("decoded SELCAL %s on %+v (snr %.1f dB, offset %+.1f Hz, selcal32=%v)",
			s.Code, s.Channels, s.SNRDB, s.OffsetHz, s.Selcal32)
	}
}
