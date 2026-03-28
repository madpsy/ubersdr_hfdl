package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
)

// instanceInfo is the JSON representation of one running IQ window.
type instanceInfo struct {
	CenterKHz    int    `json:"center_khz"`
	IQMode       string `json:"iq_mode"`
	BandwidthKHz int    `json:"bandwidth_khz"`
	FreqsKHz     []int  `json:"freqs_khz"`
}

// applyRequest is the JSON body expected by POST /apply/* endpoints.
type applyRequest struct {
	Pass string `json:"pass"`
}

// startWebServer starts the HTTP statistics server on the given port.
// Static files are served from staticDir.
//
// configPass is the password required to use the Apply endpoints.
// If empty, Apply endpoints are disabled (return 403).
//
// exitCh is signalled after a successful Apply write so the launcher
// exits cleanly and Docker restarts it with the updated frequency file.
//
// Endpoints:
//
//	GET /               — HTML dashboard (served from staticDir/index.html)
//	GET /stats          — JSON snapshot of current statistics
//	GET /aircraft       — JSON array of all known aircraft positions
//	GET /groundstations — JSON array of all ground stations with their frequencies
//	GET /instances      — JSON object with extra_args, windows array, apply_enabled
//	GET /events         — Server-Sent Events stream of typed events
//	GET /export/frequencies         — JSONL download (active freqs only)
//	GET /export/frequencies/all     — JSONL download (all freqs enabled)
//	GET /export/frequencies/latest  — JSONL download (proxied from ubersdr.org)
//	POST /apply/frequencies         — Write active freqs to file, then exit
//	POST /apply/frequencies/all     — Write all freqs to file, then exit
//	POST /apply/frequencies/latest  — Fetch latest from ubersdr.org, write to file, then exit
const defaultFreqURL = "https://ubersdr.org/hfdl/hfdl_frequencies.jsonl"

func startWebServer(port int, staticDir string, store *statsStore, groups []freqGroup, disabledFreqs []int, extraArgs []string, freqURL string, configPass string, exitCh chan<- struct{}) {
	mux := http.NewServeMux()

	// Derive the writable file path from a file:// freqURL, if applicable.
	// Apply endpoints are only active when both configPass is set AND freqURL
	// is a local file path (so we have somewhere to write).
	freqFilePath := ""
	if strings.HasPrefix(freqURL, "file://") {
		freqFilePath = strings.TrimPrefix(freqURL, "file://")
	}
	applyEnabled := configPass != "" && freqFilePath != ""

	// checkApplyAuth validates the POST body password and writes an error
	// response if the request should be rejected.  Returns true if the caller
	// should proceed.
	checkApplyAuth := func(w http.ResponseWriter, r *http.Request) bool {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return false
		}
		if !applyEnabled {
			http.Error(w, `{"error":"apply is disabled — set CONFIG_PASS and use a file:// FREQ_URL"}`, http.StatusForbidden)
			return false
		}
		var req applyRequest
		body, err := io.ReadAll(io.LimitReader(r.Body, 1024))
		if err != nil || json.Unmarshal(body, &req) != nil {
			http.Error(w, `{"error":"invalid request body"}`, http.StatusBadRequest)
			return false
		}
		if req.Pass != configPass {
			http.Error(w, `{"error":"incorrect password"}`, http.StatusUnauthorized)
			return false
		}
		return true
	}

	// writeAndExit writes data to the frequency file and signals the launcher
	// to exit so Docker restarts it with the updated file.
	writeAndExit := func(w http.ResponseWriter, data []byte) {
		if err := os.WriteFile(freqFilePath, data, 0644); err != nil { //nolint:gosec
			log.Printf("web: apply: failed to write %s: %v", freqFilePath, err)
			http.Error(w, `{"error":"failed to write frequency file"}`, http.StatusInternalServerError)
			return
		}
		log.Printf("web: apply: wrote %d bytes to %s — signalling exit for restart", len(data), freqFilePath)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `{"status":"ok","message":"frequency file updated — restarting"}`) //nolint:errcheck
		// Signal exit in a goroutine so the HTTP response is flushed first.
		go func() { exitCh <- struct{}{} }()
	}

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

	// /instances — IQ windows, extra args, and apply_enabled flag
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
			"apply_enabled":  applyEnabled,
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

	// /export/frequencies/latest — proxy the upstream JSONL directly to the client.
	// Always fetches from the canonical ubersdr.org URL regardless of the configured
	// freq-url (which may be a local file:// path).
	mux.HandleFunc("/export/frequencies/latest", func(w http.ResponseWriter, r *http.Request) {
		upstream := defaultFreqURL
		w.Header().Set("Content-Type", "application/x-ndjson")
		w.Header().Set("Content-Disposition", `attachment; filename="hfdl_frequencies.jsonl"`)
		w.Header().Set("Access-Control-Allow-Origin", "*")

		if strings.HasPrefix(upstream, "file://") {
			path := strings.TrimPrefix(upstream, "file://")
			f, err := os.Open(path)
			if err != nil {
				http.Error(w, "failed to open local frequency file: "+err.Error(), http.StatusBadGateway)
				return
			}
			defer f.Close()
			io.Copy(w, f) //nolint:errcheck
			return
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
		io.Copy(w, resp.Body) //nolint:errcheck
	})

	// /apply/frequencies — write active-only JSONL to the frequency file, then exit
	mux.HandleFunc("/apply/frequencies", func(w http.ResponseWriter, r *http.Request) {
		if !checkApplyAuth(w, r) {
			return
		}
		writeAndExit(w, store.exportActiveFrequencies())
	})

	// /apply/frequencies/all — write all-enabled JSONL to the frequency file, then exit
	mux.HandleFunc("/apply/frequencies/all", func(w http.ResponseWriter, r *http.Request) {
		if !checkApplyAuth(w, r) {
			return
		}
		writeAndExit(w, store.exportAllFrequencies())
	})

	// /apply/frequencies/latest — fetch latest from ubersdr.org, write to file, then exit
	mux.HandleFunc("/apply/frequencies/latest", func(w http.ResponseWriter, r *http.Request) {
		if !checkApplyAuth(w, r) {
			return
		}

		resp, err := http.Get(defaultFreqURL) //nolint:noctx
		if err != nil {
			http.Error(w, `{"error":"failed to fetch upstream frequency list"}`, http.StatusBadGateway)
			return
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			http.Error(w, fmt.Sprintf(`{"error":"upstream returned HTTP %d"}`, resp.StatusCode), http.StatusBadGateway)
			return
		}
		data, err := io.ReadAll(resp.Body)
		if err != nil {
			http.Error(w, `{"error":"failed to read upstream response"}`, http.StatusBadGateway)
			return
		}

		writeAndExit(w, data)
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
				fmt.Fprintf(w, "data: %s\n\n", msg) //nolint:errcheck
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
