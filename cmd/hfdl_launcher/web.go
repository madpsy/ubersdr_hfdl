package main

import (
	"container/list"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// ---------------------------------------------------------------------------
// Simple LRU cache for external API proxy responses
// ---------------------------------------------------------------------------

type lruEntry struct {
	key     string
	value   []byte
	fetched time.Time
}

type lruCache struct {
	mu    sync.Mutex
	cap   int
	ttl   time.Duration
	items map[string]*list.Element
	evict *list.List
}

func newLRUCache(capacity int, ttl time.Duration) *lruCache {
	return &lruCache{
		cap:   capacity,
		ttl:   ttl,
		items: make(map[string]*list.Element),
		evict: list.New(),
	}
}

// get returns the cached value and true, or nil and false if missing/expired.
func (c *lruCache) get(key string) ([]byte, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.items[key]
	if !ok {
		return nil, false
	}
	entry := el.Value.(*lruEntry)
	if c.ttl > 0 && time.Since(entry.fetched) > c.ttl {
		c.evict.Remove(el)
		delete(c.items, key)
		return nil, false
	}
	c.evict.MoveToFront(el)
	return entry.value, true
}

// set stores a value, evicting the LRU entry if at capacity.
func (c *lruCache) set(key string, value []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.items[key]; ok {
		c.evict.MoveToFront(el)
		el.Value.(*lruEntry).value = value
		el.Value.(*lruEntry).fetched = time.Now()
		return
	}
	if c.evict.Len() >= c.cap {
		oldest := c.evict.Back()
		if oldest != nil {
			c.evict.Remove(oldest)
			delete(c.items, oldest.Value.(*lruEntry).key)
		}
	}
	entry := &lruEntry{key: key, value: value, fetched: time.Now()}
	el := c.evict.PushFront(entry)
	c.items[key] = el
}

// Package-level caches — 1000 entries, 24-hour TTL (aircraft registrations
// and photos don't change often).
var (
	hexdbCache         = newLRUCache(1000, 24*time.Hour)
	planespottersCache = newLRUCache(1000, 24*time.Hour)
)

// instanceInfo is the JSON representation of one running IQ window.
type instanceInfo struct {
	CenterKHz     int    `json:"center_khz"`
	IQMode        string `json:"iq_mode"`
	BandwidthKHz  int    `json:"bandwidth_khz"`
	FreqsKHz      []int  `json:"freqs_khz"`
	Running       bool   `json:"running"`
	Reconnections int    `json:"reconnections"`
	StartedAt     int64  `json:"started_at"`      // unix seconds; 0 = not currently running
	LastHealthyAt int64  `json:"last_healthy_at"` // unix seconds; 0 = never ran
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

func startWebServer(port int, staticDir string, store *statsStore, instances []*instance, groups []freqGroup, disabledFreqs []int, extraArgs []string, freqURL string, configPass string, ubersdrURL string, exitCh chan<- struct{}) {
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
		// Ensure the parent directory exists (handles cases where the path was
		// configured but the directory was never created on the host).
		if dir := filepath.Dir(freqFilePath); dir != "" {
			if err := os.MkdirAll(dir, 0755); err != nil { //nolint:gosec
				log.Printf("web: apply: failed to create directory %s: %v", dir, err)
				msg, _ := json.Marshal(map[string]string{"error": fmt.Sprintf("failed to create config directory: %v", err)}) //nolint:errcheck
				http.Error(w, string(msg), http.StatusInternalServerError)
				return
			}
		}
		if err := os.WriteFile(freqFilePath, data, 0644); err != nil { //nolint:gosec
			log.Printf("web: apply: failed to write %s: %v", freqFilePath, err)
			msg, _ := json.Marshal(map[string]string{"error": fmt.Sprintf("failed to write config file: %v", err)}) //nolint:errcheck
			http.Error(w, string(msg), http.StatusInternalServerError)
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

	// /instances — IQ windows, extra args, apply_enabled flag, and per-instance health
	mux.HandleFunc("/instances", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Access-Control-Allow-Origin", "*")
		windows := make([]instanceInfo, len(groups))
		for i, g := range groups {
			bw := 0
			if info, ok := iqModes[g.iqMode]; ok {
				bw = info.bandwidthKHz
			}
			info := instanceInfo{
				CenterKHz:    g.centerKHz,
				IQMode:       g.iqMode,
				BandwidthKHz: bw,
				FreqsKHz:     g.freqsKHz,
			}
			// Attach live health data from the corresponding instance (if available).
			if i < len(instances) {
				inst := instances[i]
				inst.mu.Lock()
				info.Running = inst.running
				info.Reconnections = inst.reconnections
				if !inst.startedAt.IsZero() {
					info.StartedAt = inst.startedAt.Unix()
				}
				if !inst.lastHealthyAt.IsZero() {
					info.LastHealthyAt = inst.lastHealthyAt.Unix()
				}
				inst.mu.Unlock()
			}
			windows[i] = info
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

	// /receiver/description — proxy /api/description from the UberSDR backend,
	// extracting callsign, antenna, name, lat and lon for the map receiver marker.
	mux.HandleFunc("/receiver/description", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Access-Control-Allow-Origin", "*")

		target := strings.TrimRight(ubersdrURL, "/") + "/api/description"
		resp, err := http.Get(target) //nolint:noctx
		if err != nil {
			http.Error(w, `{"error":"failed to reach UberSDR"}`, http.StatusBadGateway)
			return
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			http.Error(w, fmt.Sprintf(`{"error":"UberSDR returned HTTP %d"}`, resp.StatusCode), http.StatusBadGateway)
			return
		}

		// Decode only the fields we need so we don't expose the full payload.
		var full struct {
			Receiver struct {
				Callsign string `json:"callsign"`
				Antenna  string `json:"antenna"`
				Name     string `json:"name"`
				GPS      struct {
					Lat float64 `json:"lat"`
					Lon float64 `json:"lon"`
				} `json:"gps"`
			} `json:"receiver"`
		}
		body, err := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
		if err != nil {
			http.Error(w, `{"error":"failed to read UberSDR response"}`, http.StatusBadGateway)
			return
		}
		if err := json.Unmarshal(body, &full); err != nil {
			http.Error(w, `{"error":"failed to parse UberSDR response"}`, http.StatusBadGateway)
			return
		}

		out := map[string]interface{}{
			"callsign": full.Receiver.Callsign,
			"antenna":  full.Receiver.Antenna,
			"name":     full.Receiver.Name,
			"lat":      full.Receiver.GPS.Lat,
			"lon":      full.Receiver.GPS.Lon,
		}
		if err := json.NewEncoder(w).Encode(out); err != nil {
			log.Printf("web: /receiver/description encode error: %v", err)
		}
	})

	// /groundstations — full ground station list with frequencies, last-heard times, and SPDU state
	mux.HandleFunc("/groundstations", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Access-Control-Allow-Origin", "*")
		if err := json.NewEncoder(w).Encode(store.groundStationsSnapshot()); err != nil {
			log.Printf("web: /groundstations encode error: %v", err)
		}
	})

	// /network — SPDU-derived network topology (all GS advertised active slots)
	mux.HandleFunc("/network", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Access-Control-Allow-Origin", "*")
		snap := store.networkSnapshot()
		if snap == nil {
			snap = []NetworkGSState{}
		}
		if err := json.NewEncoder(w).Encode(snap); err != nil {
			log.Printf("web: /network encode error: %v", err)
		}
	})

	// /propagation — aircraft→GS propagation paths from freq_data[]
	mux.HandleFunc("/propagation", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Access-Control-Allow-Origin", "*")
		snap := store.propagationSnapshot()
		if err := json.NewEncoder(w).Encode(snap); err != nil {
			log.Printf("web: /propagation encode error: %v", err)
		}
	})

	// /weather — ACARS label H1/WX weather message ring buffer
	mux.HandleFunc("/weather", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Access-Control-Allow-Origin", "*")
		msgs := store.weatherSnapshot()
		if err := json.NewEncoder(w).Encode(msgs); err != nil {
			log.Printf("web: /weather encode error: %v", err)
		}
	})

	// /proxy/hexdb/{icao} — proxy Hexdb.io aircraft lookup with LRU cache
	mux.HandleFunc("/proxy/hexdb/", func(w http.ResponseWriter, r *http.Request) {
		icao := strings.TrimPrefix(r.URL.Path, "/proxy/hexdb/")
		icao = strings.Trim(icao, "/")
		if icao == "" {
			http.Error(w, `{"error":"missing ICAO"}`, http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Access-Control-Allow-Origin", "*")

		if cached, ok := hexdbCache.get(icao); ok {
			w.Write(cached) //nolint:errcheck
			return
		}

		upstream := "https://hexdb.io/api/v1/aircraft/" + icao
		resp, err := http.Get(upstream) //nolint:noctx
		if err != nil {
			http.Error(w, `{"error":"hexdb fetch failed"}`, http.StatusBadGateway)
			return
		}
		defer resp.Body.Close()
		body, err := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
		if err != nil {
			http.Error(w, `{"error":"hexdb read failed"}`, http.StatusBadGateway)
			return
		}
		if resp.StatusCode == http.StatusOK {
			hexdbCache.set(icao, body)
		}
		w.WriteHeader(resp.StatusCode)
		w.Write(body) //nolint:errcheck
	})

	// /proxy/planespotters/{icao} — proxy Planespotters photo lookup with LRU cache
	mux.HandleFunc("/proxy/planespotters/", func(w http.ResponseWriter, r *http.Request) {
		icao := strings.TrimPrefix(r.URL.Path, "/proxy/planespotters/")
		icao = strings.Trim(icao, "/")
		if icao == "" {
			http.Error(w, `{"error":"missing ICAO"}`, http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Access-Control-Allow-Origin", "*")

		// Build cache key including optional reg/type query params
		reg := r.URL.Query().Get("reg")
		cacheKey := icao
		if reg != "" {
			cacheKey = icao + ":" + reg
		}

		if cached, ok := planespottersCache.get(cacheKey); ok {
			w.Write(cached) //nolint:errcheck
			return
		}

		upstream := "https://api.planespotters.net/pub/photos/hex/" + icao
		if reg != "" {
			upstream += "?reg=" + reg
		}
		resp, err := http.Get(upstream) //nolint:noctx
		if err != nil {
			http.Error(w, `{"error":"planespotters fetch failed"}`, http.StatusBadGateway)
			return
		}
		defer resp.Body.Close()
		body, err := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
		if err != nil {
			http.Error(w, `{"error":"planespotters read failed"}`, http.StatusBadGateway)
			return
		}
		if resp.StatusCode == http.StatusOK {
			planespottersCache.set(cacheKey, body)
		}
		w.WriteHeader(resp.StatusCode)
		w.Write(body) //nolint:errcheck
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
