package main

// selcal_web.go — HTTP surface for the SELCAL feature.
//
//	GET /selcal              JSON snapshot: channel status + decoded spot history
//	GET /selcal/audio?ch=ID  WebSocket: live mono PCM for one channel
//
// Decoded spots are additionally pushed on the existing /events SSE stream as
// {"type":"selcal","data":{...}} so the UI updates without polling.

import (
	"encoding/json"
	"log"
	"net/http"
	"time"

	"github.com/gorilla/websocket"
)

// selcalUpgrader accepts any origin: the dashboard is normally reached through
// the UberSDR reverse proxy, so the Origin header does not identify the site.
var selcalUpgrader = websocket.Upgrader{
	ReadBufferSize:  1024,
	WriteBufferSize: 32 * 1024,
	CheckOrigin:     func(r *http.Request) bool { return true },
}

func registerSelcalRoutes(mux *http.ServeMux, mgr *selcalManager, store *selcalStore, audioEnabled bool) {
	// /selcal — channel status and decoded spot history.
	mux.HandleFunc("/selcal", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Access-Control-Allow-Origin", "*")

		var statuses []selcalChannelStatus
		if mgr != nil {
			statuses = mgr.statuses()
		}
		snap := store.snapshot(statuses)

		if mgr != nil {
			// Per-listener bitrate: one µ-law byte per sample, mono.
			rate := 12000
			for _, st := range statuses {
				if st.SampleRate > 0 {
					rate = st.SampleRate
					break
				}
			}
			snap.ListenerKbps = rate * 8 / 1000
			snap.ListenersTotal = mgr.budget.current()
			snap.ListenersMax = mgr.budget.max
			snap.UploadKbps = snap.ListenersTotal * snap.ListenerKbps
			snap.UploadMaxKbps = snap.ListenersMax * snap.ListenerKbps
		}

		if err := json.NewEncoder(w).Encode(snap); err != nil {
			log.Printf("web: /selcal encode error: %v", err)
		}
	})

	// /selcal/audio — live audio for one channel.
	mux.HandleFunc("/selcal/audio", func(w http.ResponseWriter, r *http.Request) {
		if mgr == nil {
			http.Error(w, "SELCAL is not enabled", http.StatusNotFound)
			return
		}
		if !audioEnabled {
			http.Error(w, "audio relay is disabled (-selcal-audio=false)", http.StatusForbidden)
			return
		}
		ch := mgr.find(r.URL.Query().Get("ch"))
		if ch == nil {
			http.Error(w, "unknown channel", http.StatusNotFound)
			return
		}

		// The listener budget is receiver-wide, not per channel.
		sub, ok := ch.hub.subscribe()
		if !ok {
			http.Error(w, "listener limit reached — too many people are listening right now",
				http.StatusServiceUnavailable)
			return
		}

		conn, err := selcalUpgrader.Upgrade(w, r, nil)
		if err != nil {
			ch.hub.unsubscribe(sub)
			return // Upgrade has already written a response
		}
		defer conn.Close()
		defer ch.hub.unsubscribe(sub)

		// Tell the browser what it is about to receive: mono 8-bit G.711 µ-law
		// at the receiver's audio sample rate, one byte per sample.
		rate, chans := ch.hub.format()
		if rate == 0 {
			rate = 12000 // usb; the first full-header packet will confirm
		}
		if chans == 0 {
			chans = 1
		}
		hello, _ := json.Marshal(map[string]any{
			"type":     "format",
			"codec":    "ulaw",
			"framing":  "snr16le+ulaw", // int16 centi-dB SNR header, then µ-law samples
			"rate":     rate,
			"channels": chans,
			"freq_khz": ch.cfg.FreqKHz,
			"label":    ch.cfg.Label,
		})
		if err := conn.WriteMessage(websocket.TextMessage, hello); err != nil {
			return
		}

		// Reading serves only to notice the client going away (and to consume
		// any keepalive it sends); the audio flows the other way.
		closed := make(chan struct{})
		go func() {
			defer close(closed)
			for {
				if _, _, err := conn.ReadMessage(); err != nil {
					return
				}
			}
		}()

		ping := time.NewTicker(30 * time.Second)
		defer ping.Stop()

		for {
			select {
			case <-closed:
				return
			case <-r.Context().Done():
				return
			case <-ping.C:
				if err := conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(5*time.Second)); err != nil {
					return
				}
			case pcm := <-sub:
				if err := conn.SetWriteDeadline(time.Now().Add(10 * time.Second)); err != nil {
					return
				}
				if err := conn.WriteMessage(websocket.BinaryMessage, pcm); err != nil {
					return
				}
			}
		}
	})
}
