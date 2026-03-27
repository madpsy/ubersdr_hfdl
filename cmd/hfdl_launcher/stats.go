package main

import (
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"
	"sync"
	"time"
)

// bearingDeg computes the initial bearing (degrees, 0=N, clockwise) from
// point (lat1,lon1) to point (lat2,lon2) using the forward azimuth formula.
func bearingDeg(lat1, lon1, lat2, lon2 float64) float64 {
	const toRad = math.Pi / 180
	φ1 := lat1 * toRad
	φ2 := lat2 * toRad
	Δλ := (lon2 - lon1) * toRad
	y := math.Sin(Δλ) * math.Cos(φ2)
	x := math.Cos(φ1)*math.Sin(φ2) - math.Sin(φ1)*math.Cos(φ2)*math.Cos(Δλ)
	return math.Mod(math.Atan2(y, x)*180/math.Pi+360, 360)
}

const maxRecentMessages = 200

// ---------------------------------------------------------------------------
// JSON message types (subset of dumphfdl's decoded:json output)
// ---------------------------------------------------------------------------

// hfdlMessage is the top-level JSON structure emitted by dumphfdl.
type hfdlMessage struct {
	HFDL struct {
		T struct {
			Sec  int64 `json:"sec"`
			Usec int64 `json:"usec"`
		} `json:"t"`
		Freq       int64   `json:"freq"`
		BitRate    int     `json:"bit_rate"`
		SigLevel   float64 `json:"sig_level"`
		NoiseLevel float64 `json:"noise_level"`
		FreqSkew   float64 `json:"freq_skew"`
		Slot       string  `json:"slot"`
		LPDU       *struct {
			Src struct {
				Type string `json:"type"`
				ID   int    `json:"id"`
			} `json:"src"`
			Dst struct {
				Type string `json:"type"`
				ID   int    `json:"id"`
			} `json:"dst"`
			Type struct {
				Name string `json:"name"`
			} `json:"type"`
			AcInfo *struct {
				ICAO string `json:"icao"`
			} `json:"ac_info"`
			HFNPDU *struct {
				Type struct {
					ID int `json:"id"`
				} `json:"type"`
				// Performance data (type 209) and frequency data (type 213)
				// both carry pos, flight_id, and utc_time / time.
				FlightID string `json:"flight_id"`
				Pos      *struct {
					Lat float64 `json:"lat"`
					Lon float64 `json:"lon"`
				} `json:"pos"`
				ACARS *struct {
					Reg    string `json:"reg"`
					Flight string `json:"flight"`
					Label  string `json:"label"`
				} `json:"acars"`
			} `json:"hfnpdu"`
		} `json:"lpdu"`
		SPDU *struct {
			Src struct {
				Type string `json:"type"`
				ID   int    `json:"id"`
			} `json:"src"`
		} `json:"spdu"`
	} `json:"hfdl"`
}

// ---------------------------------------------------------------------------
// Stats types
// ---------------------------------------------------------------------------

// SigBucket holds signal-level statistics for one 30-minute window.
type SigBucket struct {
	T     int64   `json:"t"`   // unix seconds of bucket start (floor to 1800s)
	Avg   float64 `json:"avg"` // mean dBFS
	Min   float64 `json:"min"` // minimum dBFS
	Max   float64 `json:"max"` // maximum dBFS
	Count int64   `json:"n"`   // number of samples
	sum   float64              // running sum (not serialised)
}

const sigBucketSecs  = 1800 // 30 minutes
const maxSigBuckets  = 96   // 48 hours of history

// GSFreqStats holds per-ground-station statistics on a specific frequency.
type GSFreqStats struct {
	GSID        int          `json:"gs_id"`
	MsgCount    int64        `json:"msg_count"`
	LastSeen    int64        `json:"last_seen"`    // unix seconds
	AvgSigLevel float64      `json:"avg_sig_level"` // current-bucket average
	Buckets     []*SigBucket `json:"sig_history"`   // oldest-first, capped at maxSigBuckets
}

// addSigSample adds a signal-level sample to the appropriate 30-min bucket.
func (g *GSFreqStats) addSigSample(t int64, sig float64) {
	bucketStart := (t / sigBucketSecs) * sigBucketSecs
	// Reuse the last bucket if it matches the current window
	if n := len(g.Buckets); n > 0 && g.Buckets[n-1].T == bucketStart {
		b := g.Buckets[n-1]
		b.sum += sig
		b.Count++
		b.Avg = b.sum / float64(b.Count)
		if sig < b.Min {
			b.Min = sig
		}
		if sig > b.Max {
			b.Max = sig
		}
		g.AvgSigLevel = b.Avg
		return
	}
	// New bucket
	b := &SigBucket{
		T:     bucketStart,
		Avg:   sig,
		Min:   sig,
		Max:   sig,
		Count: 1,
		sum:   sig,
	}
	g.Buckets = append(g.Buckets, b)
	if len(g.Buckets) > maxSigBuckets {
		g.Buckets = g.Buckets[len(g.Buckets)-maxSigBuckets:]
	}
	g.AvgSigLevel = sig
}

// FreqStats holds per-frequency statistics, broken down by heard ground station.
type FreqStats struct {
	FreqHz  int64                  `json:"freq_hz"`
	FreqKHz int64                  `json:"freq_khz"`
	GSStats map[int]*GSFreqStats   `json:"gs_stats"` // gs_id → stats (only heard GS)
}

// TrackPoint is a single historical position fix for an aircraft.
type TrackPoint struct {
	Lat  float64 `json:"lat"`
	Lon  float64 `json:"lon"`
	Time int64   `json:"time"` // unix seconds
}

const maxTrackPoints = 500

// AircraftState holds the last-known position and identity of an aircraft.
type AircraftState struct {
	Key      string       `json:"key"`      // ICAO hex if known, else registration
	ICAO     string       `json:"icao,omitempty"`
	Reg      string       `json:"reg,omitempty"`
	Flight   string       `json:"flight,omitempty"`
	Lat      float64      `json:"lat"`
	Lon      float64      `json:"lon"`
	FreqKHz  int64        `json:"freq_khz"`
	GSID     int          `json:"gs_id,omitempty"` // last ground station ID that communicated with this aircraft
	MsgCount int64        `json:"msg_count"`       // total messages seen for this aircraft
	SigLevel float64      `json:"sig_level"`       // signal level (dBFS) of the last received message
	LastSeen int64        `json:"last_seen"`       // unix seconds
	Bearing  float64      `json:"bearing"`  // degrees clockwise from north, 0 if unknown
	Track    []TrackPoint `json:"-"`        // position history, served via /aircraft/{key}/track only
}

// RecentMessage is a trimmed record for the live feed.
type RecentMessage struct {
	Time     int64   `json:"time"`
	FreqKHz  int64   `json:"freq_khz"`
	BitRate  int     `json:"bit_rate"`
	SigLevel float64 `json:"sig_level"`
	Slot     string  `json:"slot"`
	SrcType  string  `json:"src_type"`
	SrcID    int     `json:"src_id"`
	DstType  string  `json:"dst_type,omitempty"`
	DstID    int     `json:"dst_id,omitempty"`
	MsgType  string  `json:"msg_type"`
	Reg      string  `json:"reg,omitempty"`
	Flight   string  `json:"flight,omitempty"`
}

// StatsSnapshot is the full stats payload served at /stats.
type StatsSnapshot struct {
	TotalMessages  int64           `json:"total_messages"`
	StartTime      int64           `json:"start_time"`
	UptimeSecs     int64           `json:"uptime_secs"`
	Frequencies    []*FreqStats    `json:"frequencies"`
	Recent         []RecentMessage `json:"recent"`
	GroundStations map[int]string  `json:"ground_stations"` // gs_id → location name
}

// sseEvent wraps a typed SSE payload so the browser can distinguish
// message-feed events from position updates.
type sseEvent struct {
	Type string `json:"type"` // "message" | "position"
	Data any    `json:"data"`
}

// ---------------------------------------------------------------------------
// Stats store
// ---------------------------------------------------------------------------

// groundStationResponse is the JSON shape returned by /groundstations.
type groundStationResponse struct {
	GSID          int           `json:"gs_id"`
	Location      string        `json:"location"`
	Lat           float64       `json:"lat,omitempty"`
	Lon           float64       `json:"lon,omitempty"`
	Freqs         []gsFrequency `json:"frequencies"`
	LastHeard     int64         `json:"last_heard,omitempty"`      // unix seconds, 0 = never heard
	HeardFreqsKHz []int64       `json:"heard_freqs_khz,omitempty"` // frequencies actually heard on
	LastSigLevel  float64       `json:"last_sig_level,omitempty"`  // most recent signal level (dBFS) when GS was source
}

// statsStore holds all aggregated statistics and the SSE subscriber list.
type statsStore struct {
	mu              sync.RWMutex
	startTime       time.Time
	total           int64
	freqs           map[int64]*FreqStats
	recent          []RecentMessage
	aircraft        map[string]*AircraftState  // keyed by ICAO or reg
	heardGS         map[int]int64              // gs_id → last_heard unix seconds
	heardGSFreqs    map[int]map[int64]struct{} // gs_id → set of freq_khz heard on
	heardGSLastSig  map[int]float64            // gs_id → most recent signal level (dBFS) when GS was source
	gsNames         map[int]string             // gs_id → location name (read-only after init)
	freqGSID        map[int][]int              // freq_khz → []gs_id (read-only after init)
	stations        []groundStation            // all ground stations sorted by ID (read-only after init)

	subsMu sync.Mutex
	subs   map[chan string]struct{}
}

func newStatsStore(gsNames map[int]string, freqGSID map[int][]int, stations []groundStation) *statsStore {
	if gsNames == nil {
		gsNames = make(map[int]string)
	}
	if freqGSID == nil {
		freqGSID = make(map[int][]int)
	}
	return &statsStore{
		startTime:      time.Now(),
		freqs:          make(map[int64]*FreqStats),
		aircraft:       make(map[string]*AircraftState),
		heardGS:        make(map[int]int64),
		heardGSFreqs:   make(map[int]map[int64]struct{}),
		heardGSLastSig: make(map[int]float64),
		gsNames:        gsNames,
		freqGSID:       freqGSID,
		stations:       stations,
		subs:           make(map[chan string]struct{}),
	}
}

// isValidPos returns true if the lat/lon is a real position (not the 180/180
// sentinel that dumphfdl uses for "unknown").
func isValidPos(lat, lon float64) bool {
	return !(math.Abs(lat) > 90 || math.Abs(lon) > 180 ||
		(lat == 180 && lon == 180) || (lat == 0 && lon == 0))
}

// ingest parses one JSON line from dumphfdl and updates the stats store.
func (s *statsStore) ingest(line string) {
	var msg hfdlMessage
	if err := json.Unmarshal([]byte(line), &msg); err != nil {
		return
	}
	h := msg.HFDL
	if h.Freq == 0 {
		return
	}

	freqKHz := h.Freq / 1000

	s.mu.Lock()

	s.total++

	fs, ok := s.freqs[h.Freq]
	if !ok {
		fs = &FreqStats{
			FreqHz:  h.Freq,
			FreqKHz: freqKHz,
			GSStats: make(map[int]*GSFreqStats),
		}
		s.freqs[h.Freq] = fs
	}

	// Build recent-message record
	rm := RecentMessage{
		Time:     h.T.Sec,
		FreqKHz:  freqKHz,
		BitRate:  h.BitRate,
		SigLevel: h.SigLevel,
		Slot:     h.Slot,
	}

	now := h.T.Sec

	// helper: update per-GS stats on this frequency when a GS is the source
	recordGSSrc := func(gsID int) {
		gs, ok := fs.GSStats[gsID]
		if !ok {
			gs = &GSFreqStats{GSID: gsID}
			fs.GSStats[gsID] = gs
		}
		gs.MsgCount++
		gs.LastSeen = now
		gs.addSigSample(now, h.SigLevel)
		// update global heardGS timestamp and last signal level
		if t := s.heardGS[gsID]; now > t {
			s.heardGS[gsID] = now
		}
		s.heardGSLastSig[gsID] = h.SigLevel
		// record which frequency this GS was heard on
		if s.heardGSFreqs[gsID] == nil {
			s.heardGSFreqs[gsID] = make(map[int64]struct{})
		}
		s.heardGSFreqs[gsID][freqKHz] = struct{}{}
	}

	// Extract position and identity from LPDU if present
	var posUpdate *AircraftState
	if h.LPDU != nil {
		rm.SrcType = h.LPDU.Src.Type
		rm.SrcID = h.LPDU.Src.ID
		rm.DstType = h.LPDU.Dst.Type
		rm.DstID = h.LPDU.Dst.ID
		rm.MsgType = h.LPDU.Type.Name

		if h.LPDU.Src.Type == "Ground station" {
			recordGSSrc(h.LPDU.Src.ID)
		}

		// Gather identity fields
		icao := ""
		if h.LPDU.AcInfo != nil {
			icao = h.LPDU.AcInfo.ICAO
		}
		reg := ""
		flight := ""
		if h.LPDU.HFNPDU != nil {
			if h.LPDU.HFNPDU.ACARS != nil {
				reg = strings.TrimPrefix(h.LPDU.HFNPDU.ACARS.Reg, ".")
				flight = strings.TrimPrefix(h.LPDU.HFNPDU.ACARS.Flight, ".")
			}
			if h.LPDU.HFNPDU.FlightID != "" && flight == "" {
				flight = h.LPDU.HFNPDU.FlightID
			}
			rm.Reg = reg
			rm.Flight = flight

			// Determine aircraft key for counting and position tracking
			acKey := icao
			if acKey == "" {
				acKey = reg
			}
			if acKey == "" {
				acKey = flight
			}

			// Increment message count for this aircraft
			if acKey != "" {
				existing := s.aircraft[acKey]
				if existing == nil {
					existing = &AircraftState{Key: acKey}
					s.aircraft[acKey] = existing
				}
				existing.MsgCount++
				existing.SigLevel = h.SigLevel
				// Update identity fields in case they're newly available
				if icao != "" {
					existing.ICAO = icao
				}
				if reg != "" {
					existing.Reg = reg
				}
				if flight != "" {
					existing.Flight = flight
				}
			}

			// Extract position if present and valid
			if h.LPDU.HFNPDU.Pos != nil {
				lat := h.LPDU.HFNPDU.Pos.Lat
				lon := h.LPDU.HFNPDU.Pos.Lon
				if isValidPos(lat, lon) && acKey != "" {
					gsID := 0
					if h.LPDU.Src.Type == "Ground station" {
						gsID = h.LPDU.Src.ID
					} else if h.LPDU.Dst.Type == "Ground station" {
						gsID = h.LPDU.Dst.ID
					}
					ac := s.aircraft[acKey]
					ac.Lat = lat
					ac.Lon = lon
					ac.FreqKHz = freqKHz
					ac.GSID = gsID
					ac.LastSeen = h.T.Sec
					// Append to position history, capping at maxTrackPoints
					ac.Track = append(ac.Track, TrackPoint{Lat: lat, Lon: lon, Time: h.T.Sec})
					if len(ac.Track) > maxTrackPoints {
						ac.Track = ac.Track[len(ac.Track)-maxTrackPoints:]
					}
					// Compute bearing from previous position if available
					if n := len(ac.Track); n >= 2 {
						prev := ac.Track[n-2]
						ac.Bearing = bearingDeg(prev.Lat, prev.Lon, lat, lon)
					}
					posUpdate = ac
				}
			}
		}
	} else if h.SPDU != nil {
		rm.SrcType = h.SPDU.Src.Type
		rm.SrcID = h.SPDU.Src.ID
		rm.MsgType = "SPDU"
		if h.SPDU.Src.Type == "Ground station" {
			recordGSSrc(h.SPDU.Src.ID)
		}
	}

	s.recent = append(s.recent, rm)
	if len(s.recent) > maxRecentMessages {
		s.recent = s.recent[len(s.recent)-maxRecentMessages:]
	}

	s.mu.Unlock()

	// Broadcast message event to SSE subscribers (outside the main lock)
	if ev, err := json.Marshal(sseEvent{Type: "message", Data: rm}); err == nil {
		s.broadcast(string(ev))
	}

	// Broadcast position update if we got one
	if posUpdate != nil {
		if ev, err := json.Marshal(sseEvent{Type: "position", Data: posUpdate}); err == nil {
			s.broadcast(string(ev))
		}
	}
}

// SignalSeries is one (GS, frequency) time series returned by /signal.
type SignalSeries struct {
	GSID     int          `json:"gs_id"`
	Location string       `json:"location"`
	FreqKHz  int64        `json:"freq_khz"`
	Buckets  []*SigBucket `json:"buckets"`
}

// signalHistorySnapshot returns all (GS, freq) signal time series.
func (s *statsStore) signalHistorySnapshot() []SignalSeries {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var result []SignalSeries
	for _, fs := range s.freqs {
		for gsID, gs := range fs.GSStats {
			if len(gs.Buckets) == 0 {
				continue
			}
			buckets := make([]*SigBucket, len(gs.Buckets))
			for i, b := range gs.Buckets {
				cp := *b
				buckets[i] = &cp
			}
			loc := s.gsNames[gsID]
			if loc == "" {
				loc = fmt.Sprintf("GS %d", gsID)
			}
			result = append(result, SignalSeries{
				GSID:     gsID,
				Location: loc,
				FreqKHz:  fs.FreqKHz,
				Buckets:  buckets,
			})
		}
	}
	// Sort by GS ID then frequency for stable ordering
	sort.Slice(result, func(i, j int) bool {
		if result[i].GSID != result[j].GSID {
			return result[i].GSID < result[j].GSID
		}
		return result[i].FreqKHz < result[j].FreqKHz
	})
	return result
}

// snapshot returns a point-in-time copy of all statistics.
func (s *statsStore) snapshot() StatsSnapshot {
	s.mu.RLock()
	defer s.mu.RUnlock()

	freqs := make([]*FreqStats, 0, len(s.freqs))
	for _, fs := range s.freqs {
		cp := FreqStats{
			FreqHz:  fs.FreqHz,
			FreqKHz: fs.FreqKHz,
			GSStats: make(map[int]*GSFreqStats, len(fs.GSStats)),
		}
		for id, gs := range fs.GSStats {
			gsCp := GSFreqStats{
				GSID:        gs.GSID,
				MsgCount:    gs.MsgCount,
				LastSeen:    gs.LastSeen,
				AvgSigLevel: gs.AvgSigLevel,
				// Omit Buckets from the /stats snapshot to keep it small
			}
			cp.GSStats[id] = &gsCp
		}
		freqs = append(freqs, &cp)
	}
	sort.Slice(freqs, func(i, j int) bool {
		return freqs[i].FreqHz < freqs[j].FreqHz
	})

	recent := make([]RecentMessage, len(s.recent))
	copy(recent, s.recent)

	return StatsSnapshot{
		TotalMessages:  s.total,
		StartTime:      s.startTime.Unix(),
		UptimeSecs:     int64(time.Since(s.startTime).Seconds()),
		Frequencies:    freqs,
		Recent:         recent,
		GroundStations: s.gsNames,
	}
}

const aircraftMaxAgeSecs = 30 * 60 // 30 minutes

// purgeStaleAircraft removes aircraft not seen in the last 30 minutes.
// It also broadcasts a "purge" SSE event for each removed aircraft so the
// browser can remove the marker without waiting for a page reload.
// Call this in a goroutine; it runs until the process exits.
func (s *statsStore) purgeStaleAircraft() {
	ticker := time.NewTicker(2 * time.Minute)
	defer ticker.Stop()
	for range ticker.C {
		cutoff := time.Now().Unix() - aircraftMaxAgeSecs
		s.mu.Lock()
		for key, ac := range s.aircraft {
			if ac.LastSeen < cutoff {
				delete(s.aircraft, key)
				// Notify browser to remove the marker
				if ev, err := json.Marshal(sseEvent{Type: "purge", Data: key}); err == nil {
					s.mu.Unlock()
					s.broadcast(string(ev))
					s.mu.Lock()
				}
			}
		}
		s.mu.Unlock()
	}
}

// aircraftSnapshot returns a copy of all known aircraft positions.
func (s *statsStore) aircraftSnapshot() []*AircraftState {
	s.mu.RLock()
	defer s.mu.RUnlock()

	result := make([]*AircraftState, 0, len(s.aircraft))
	for _, ac := range s.aircraft {
		cp := *ac
		result = append(result, &cp)
	}
	// Sort by last seen descending
	sort.Slice(result, func(i, j int) bool {
		return result[i].LastSeen > result[j].LastSeen
	})
	return result
}

// trackSnapshot returns a copy of the position history for the given aircraft key.
// Returns nil if the key is unknown.
func (s *statsStore) trackSnapshot(key string) []TrackPoint {
	s.mu.RLock()
	defer s.mu.RUnlock()
	ac, ok := s.aircraft[key]
	if !ok || len(ac.Track) == 0 {
		return nil
	}
	cp := make([]TrackPoint, len(ac.Track))
	copy(cp, ac.Track)
	return cp
}

// groundStationsSnapshot returns the station list merged with last-heard times and heard frequencies.
func (s *statsStore) groundStationsSnapshot() []groundStationResponse {
	s.mu.RLock()
	defer s.mu.RUnlock()

	result := make([]groundStationResponse, len(s.stations))
	for i, gs := range s.stations {
		var heardFreqs []int64
		if fset := s.heardGSFreqs[gs.GSID]; len(fset) > 0 {
			heardFreqs = make([]int64, 0, len(fset))
			for f := range fset {
				heardFreqs = append(heardFreqs, f)
			}
			sort.Slice(heardFreqs, func(a, b int) bool { return heardFreqs[a] < heardFreqs[b] })
		}
		result[i] = groundStationResponse{
			GSID:          gs.GSID,
			Location:      gs.Location,
			Lat:           gs.Lat,
			Lon:           gs.Lon,
			Freqs:         gs.Freqs,
			LastHeard:     s.heardGS[gs.GSID],
			HeardFreqsKHz: heardFreqs,
			LastSigLevel:  s.heardGSLastSig[gs.GSID],
		}
	}
	return result
}

// ---------------------------------------------------------------------------
// SSE pub/sub
// ---------------------------------------------------------------------------

func (s *statsStore) subscribe() chan string {
	ch := make(chan string, 64)
	s.subsMu.Lock()
	s.subs[ch] = struct{}{}
	s.subsMu.Unlock()
	return ch
}

func (s *statsStore) unsubscribe(ch chan string) {
	s.subsMu.Lock()
	delete(s.subs, ch)
	s.subsMu.Unlock()
}

func (s *statsStore) broadcast(msg string) {
	s.subsMu.Lock()
	defer s.subsMu.Unlock()
	for ch := range s.subs {
		select {
		case ch <- msg:
		default:
			// slow subscriber — drop rather than block
		}
	}
}
