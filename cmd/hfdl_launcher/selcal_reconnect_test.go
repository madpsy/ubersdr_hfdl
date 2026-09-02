package main

import (
	"encoding/binary"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// fakeReceiver stands in for UberSDR: it accepts audio WebSocket sessions and
// lets a test decide how each one misbehaves.
type fakeReceiver struct {
	srv      *httptest.Server
	sessions atomic.Int32
	// behaviour is called per session; returning ends the session.
	behaviour func(conn *websocket.Conn, session int)
}

func newFakeReceiver(t *testing.T, behaviour func(conn *websocket.Conn, session int)) *fakeReceiver {
	t.Helper()
	fr := &fakeReceiver{behaviour: behaviour}

	up := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	mux := http.NewServeMux()
	mux.HandleFunc("/connection", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"allowed":true}`))
	})
	mux.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
		conn, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		fr.behaviour(conn, int(fr.sessions.Add(1)))
	})

	fr.srv = httptest.NewServer(mux)
	t.Cleanup(fr.srv.Close)
	return fr
}

// sendAudio writes one valid protocol version 4 packet.
//
// A resynchronisation point every time, which is what a reconnect gets: the
// decoder on the far side is new, its predictor holds no state, and a delta
// packet would rightly be refused.
func sendAudio(conn *websocket.Conn) error {
	const (
		flagEscape   = 1 << 7
		flagMetadata = 1 << 5
		flagCount    = 1 << 3
		profileAudio = 1
	)
	const samples = 32

	buf := make([]byte, 0, 24+samples*2)
	buf = binary.LittleEndian.AppendUint32(buf, 0x344D4350) // "PCM4"
	buf = append(buf, byte(flagEscape|flagMetadata|flagCount)|profileAudio)
	buf = binary.LittleEndian.AppendUint64(buf, 1234)
	var scratch [binary.MaxVarintLen64]byte
	n := binary.PutUvarint(scratch[:], samples)
	buf = append(buf, scratch[:n]...)
	n = binary.PutUvarint(scratch[:], 12000)
	buf = append(buf, scratch[:n]...)
	buf = append(buf, 1) // channels
	buf = append(buf, make([]byte, samples*2)...)
	return conn.WriteMessage(websocket.BinaryMessage, buf)
}

// withShortTimeouts shrinks the reconnect timings so tests run quickly.
func withShortTimeouts(t *testing.T, read, ping, backoff, healthy time.Duration) {
	t.Helper()
	oR, oP, oB, oH := selcalReadTimeout, selcalPingInterval, selcalMaxBackoff, selcalHealthySession
	selcalReadTimeout, selcalPingInterval = read, ping
	selcalMaxBackoff, selcalHealthySession = backoff, healthy
	t.Cleanup(func() {
		selcalReadTimeout, selcalPingInterval = oR, oP
		selcalMaxBackoff, selcalHealthySession = oB, oH
	})
}

// startTestChannel launches a channel and guarantees run() has returned before
// the test finishes, so withShortTimeouts can restore the package timings
// without racing the goroutine that reads them.
func startTestChannel(t *testing.T, url string) (*selcalChannel, <-chan struct{}) {
	t.Helper()
	store := newSelcalStore(true, func(string) {})
	ch := newSelcalChannel(
		selcalChannelCfg{FreqKHz: 8906, Label: "test", Decode: true},
		url, "", store, newListenerBudget(0), "", 8, false)

	exited := make(chan struct{})
	go func() { ch.run(); close(exited) }()

	t.Cleanup(func() {
		ch.stop()
		select {
		case <-exited:
		case <-time.After(10 * time.Second):
			t.Error("run() did not return after stop()")
		}
	})
	return ch, exited
}

// TestReconnectsAfterServerDropsConnection covers the ordinary case: the
// receiver closes the session and the channel must come back on its own.
func TestReconnectsAfterServerDropsConnection(t *testing.T) {
	withShortTimeouts(t, 2*time.Second, 500*time.Millisecond, 300*time.Millisecond, time.Hour)

	var wg sync.WaitGroup
	fr := newFakeReceiver(t, func(conn *websocket.Conn, session int) {
		// Serve a little audio, then hang up.
		for i := 0; i < 3; i++ {
			if err := sendAudio(conn); err != nil {
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
		wg.Done()
	})
	wg.Add(3) // expect at least three sessions

	ch, _ := startTestChannel(t, fr.srv.URL)

	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()

	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatalf("expected repeated reconnects, only saw %d session(s)", fr.sessions.Load())
	}

	st := ch.status()
	if st.Packets == 0 {
		t.Error("no audio was decoded across the reconnects")
	}
	if st.Reconnections == 0 {
		t.Error("reconnections counter did not advance")
	}
}

// TestReconnectsAfterStall is the failure this guards against specifically: the
// socket stays open but the receiver stops sending.  Without a read deadline
// the channel would block in ReadMessage forever and never recover.
func TestReconnectsAfterStall(t *testing.T) {
	withShortTimeouts(t, 1500*time.Millisecond, 400*time.Millisecond, 300*time.Millisecond, time.Hour)

	secondSession := make(chan struct{}, 1)
	fr := newFakeReceiver(t, func(conn *websocket.Conn, session int) {
		if session >= 2 {
			select {
			case secondSession <- struct{}{}:
			default:
			}
			// Healthy session: keep feeding audio.
			for {
				if err := sendAudio(conn); err != nil {
					return
				}
				time.Sleep(50 * time.Millisecond)
			}
		}
		// First session: send one packet, then go silent without closing.
		// Drain reads so the client's pings do not error out the connection.
		if err := sendAudio(conn); err != nil {
			return
		}
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	})

	ch, _ := startTestChannel(t, fr.srv.URL)

	select {
	case <-secondSession:
	case <-time.After(20 * time.Second):
		t.Fatal("channel never recovered from a stalled session — read deadline not working")
	}

	// And it should now stay up and keep decoding.
	time.Sleep(500 * time.Millisecond)
	if st := ch.status(); !st.Connected || st.Packets < 2 {
		t.Errorf("after recovery: connected=%v packets=%d, want connected with audio flowing",
			st.Connected, st.Packets)
	}
}

// TestStopSuppressesReconnect confirms a deliberate shutdown does not trigger
// the retry loop.
func TestStopSuppressesReconnect(t *testing.T) {
	withShortTimeouts(t, 2*time.Second, 500*time.Millisecond, 200*time.Millisecond, time.Hour)

	fr := newFakeReceiver(t, func(conn *websocket.Conn, session int) {
		for {
			if err := sendAudio(conn); err != nil {
				return
			}
			time.Sleep(30 * time.Millisecond)
		}
	})

	ch, exited := startTestChannel(t, fr.srv.URL)

	// Wait until it is up.
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if ch.status().Connected {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !ch.status().Connected {
		t.Fatal("channel never connected")
	}

	ch.stop()
	select {
	case <-exited:
	case <-time.After(10 * time.Second):
		t.Fatal("run() did not return after stop()")
	}

	before := fr.sessions.Load()
	time.Sleep(700 * time.Millisecond)
	if after := fr.sessions.Load(); after != before {
		t.Errorf("opened %d further session(s) after stop()", after-before)
	}
}

// TestSurvivesUnreachableReceiver checks the channel keeps retrying rather than
// giving up when the receiver is down at startup.
func TestSurvivesUnreachableReceiver(t *testing.T) {
	withShortTimeouts(t, time.Second, 300*time.Millisecond, 200*time.Millisecond, time.Hour)

	// A server that is closed immediately, so every dial fails.
	dead := httptest.NewServer(http.NewServeMux())
	url := dead.URL
	dead.Close()

	ch, _ := startTestChannel(t, url)

	time.Sleep(1500 * time.Millisecond)
	if st := ch.status(); st.Reconnections < 2 {
		t.Errorf("expected repeated retries against a dead receiver, got %d", st.Reconnections)
	}
	if st := ch.status(); st.Connected {
		t.Error("channel reports connected despite the receiver being down")
	}
}
