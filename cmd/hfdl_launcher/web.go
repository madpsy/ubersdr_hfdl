package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
)

// instanceInfo is the JSON representation of one running IQ window.
type instanceInfo struct {
	CenterKHz    int    `json:"center_khz"`
	IQMode       string `json:"iq_mode"`
	BandwidthKHz int    `json:"bandwidth_khz"`
	FreqsKHz     []int  `json:"freqs_khz"`
}

// startWebServer starts the HTTP statistics server on the given port.
// Static files are served from staticDir.
// Endpoints:
//
//	GET /               — HTML dashboard (served from staticDir/index.html)
//	GET /stats          — JSON snapshot of current statistics
//	GET /aircraft       — JSON array of all known aircraft positions
//	GET /groundstations — JSON array of all ground stations with their frequencies
//	GET /instances      — JSON object with extra_args and windows array
//	GET /events         — Server-Sent Events stream of typed events:
//	                        {"type":"message","data":{...}}
//	                        {"type":"position","data":{...}}
const defaultFreqURL = "https://ubersdr.org/hfdl/hfdl_frequencies.jsonl"

func startWebServer(port int, staticDir string, store *statsStore, groups []freqGroup, disabledFreqs []int, extraArgs []string, freqURL string) {
	mux := http.NewServeMux()

	// /stats — full JSON snapshot
	mux.HandleFunc("/stats", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Access-Control-Allow-Origin", "*")
		snap := store.snapshot()
		if err := json.NewEncoder(w).Encode(snap); err != nil {
			log.Printf("web: /stats encode error: %v", err)
		}
	})

	// /aircraft — current aircraft positions
	mux.HandleFunc("/aircraft", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Access-Control-Allow-Origin", "*")
		aircraft := store.aircraftSnapshot()
		if err := json.NewEncoder(w).Encode(aircraft); err != nil {
			log.Printf("web: /aircraft encode error: %v", err)
		}
	})

	// /aircraft/{key}/track — position history for a specific aircraft
	mux.HandleFunc("/aircraft/", func(w http.ResponseWriter, r *http.Request) {
		// Expect path: /aircraft/{key}/track
		path := strings.TrimPrefix(r.URL.Path, "/aircraft/")
		key, suffix, _ := strings.Cut(path, "/")
		if key == "" || suffix != "track" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Access-Control-Allow-Origin", "*")
		track := store.trackSnapshot(key)
		if track == nil {
			track = []TrackPoint{} // return empty array, not null
		}
		if err := json.NewEncoder(w).Encode(track); err != nil {
			log.Printf("web: /aircraft/track encode error: %v", err)
		}
	})

	// /instances — IQ windows and extra args
	mux.HandleFunc("/instances", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Access-Control-Allow-Origin", "*")
		windows := make([]instanceInfo, len(groups))
		for i, g := range groups {
			bw := 0
			if info, ok := iqModes[g.iqMode]; ok {
				bw = info.bandwidthKHz
			}
			windows[i] = instanceInfo{
				CenterKHz:    g.centerKHz,
				IQMode:       g.iqMode,
				BandwidthKHz: bw,
				FreqsKHz:     g.freqsKHz,
			}
		}
		resp := map[string]interface{}{
			"extra_args":     extraArgs,
			"windows":        windows,
			"disabled_freqs": disabledFreqs,
			"freq_url":       freqURL,
		}
		if err := json.NewEncoder(w).Encode(resp); err != nil {
			log.Printf("web: /instances encode error: %v", err)
		}
	})

	// /signal — per-(GS, frequency) signal-level time series (30-min buckets)
	mux.HandleFunc("/signal", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Access-Control-Allow-Origin", "*")
		series := store.signalHistorySnapshot()
		if series == nil {
			series = []SignalSeries{}
		}
		if err := json.NewEncoder(w).Encode(series); err != nil {
			log.Printf("web: /signal encode error: %v", err)
		}
	})

	// /export/frequencies — JSONL download with enabled field set per observed activity
	mux.HandleFunc("/export/frequencies", func(w http.ResponseWriter, r *http.Request) {
		data := store.exportActiveFrequencies()
		w.Header().Set("Content-Type", "application/x-ndjson")
		w.Header().Set("Content-Disposition", `attachment; filename="hfdl_frequencies.jsonl"`)
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Write(data) //nolint:errcheck
	})

	// /export/frequencies/all — JSONL download with every frequency set to enabled
	mux.HandleFunc("/export/frequencies/all", func(w http.ResponseWriter, r *http.Request) {
		data := store.exportAllFrequencies()
		w.Header().Set("Content-Type", "application/x-ndjson")
		w.Header().Set("Content-Disposition", `attachment; filename="hfdl_frequencies.jsonl"`)
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Write(data) //nolint:errcheck
	})

	// /export/frequencies/latest — proxy the upstream JSONL directly to the client
	mux.HandleFunc("/export/frequencies/latest", func(w http.ResponseWriter, r *http.Request) {
		upstream := freqURL
		if upstream == "" {
			upstream = defaultFreqURL
		}
		resp, err := http.Get(upstream) //nolint:noctx
		if err != nil {
			http.Error(w, "failed to fetch upstream frequency list: "+err.Error(), http.StatusBadGateway)
			return
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			http.Error(w, fmt.Sprintf("upstream returned HTTP %d", resp.StatusCode), http.StatusBadGateway)
			return
		}
		w.Header().Set("Content-Type", "application/x-ndjson")
		w.Header().Set("Content-Disposition", `attachment; filename="hfdl_frequencies.jsonl"`)
		w.Header().Set("Access-Control-Allow-Origin", "*")
		io.Copy(w, resp.Body) //nolint:errcheck
	})

	// /groundstations — full ground station list with frequencies and last-heard times
	mux.HandleFunc("/groundstations", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Access-Control-Allow-Origin", "*")
		if err := json.NewEncoder(w).Encode(store.groundStationsSnapshot()); err != nil {
			log.Printf("web: /groundstations encode error: %v", err)
		}
	})

	// /events — Server-Sent Events stream
	// Each event is a JSON object: {"type":"message"|"position","data":{...}}
	mux.HandleFunc("/events", func(w http.ResponseWriter, r *http.Request) {
		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "streaming not supported", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		w.Header().Set("Access-Control-Allow-Origin", "*")

		ch := store.subscribe()
		defer store.unsubscribe(ch)

		ctx := r.Context()
		for {
			select {
			case <-ctx.Done():
				return
			case msg, ok := <-ch:
				if !ok {
					return
				}
				fmt.Fprintf(w, "data: %s\n\n", msg)
				flusher.Flush()
			}
		}
	})

	// / — static files
	if staticDir != "" {
		mux.Handle("/", http.FileServer(http.Dir(staticDir)))
	} else {
		mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "no static directory configured (-web-static flag)", http.StatusNotFound)
		})
	}

	addr := fmt.Sprintf(":%d", port)
	log.Printf("web stats server listening on http://0.0.0.0%s  (static: %q)", addr, staticDir)
	srv := &http.Server{
		Addr:    addr,
		Handler: mux,
	}
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Printf("web server error: %v", err)
	}
}
