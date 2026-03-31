package main

import (
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// parseLabel16Obs attempts to parse an ACARS label 16 weather/position observation.
//
// Format: HHMMSS,AAAAA,SSSS,HHH,N DD.DDD E DDD.DDD
// Example: 170130,33994,1915, 103,N 51.007 E 51.808
//
// Returns lat, lon, altFt, headingDeg, ok.
func parseLabel16Obs(msgText string) (lat, lon, altFt, heading float64, ok bool) {
	// Split on commas — expect at least 5 fields
	parts := strings.SplitN(strings.TrimSpace(msgText), ",", 6)
	if len(parts) < 5 {
		return
	}
	// parts[1] = altitude in feet
	alt, err1 := strconv.ParseFloat(strings.TrimSpace(parts[1]), 64)
	if err1 == nil && alt > 0 {
		altFt = alt
	}
	// parts[3] = heading in degrees
	hdg, err2 := strconv.ParseFloat(strings.TrimSpace(parts[3]), 64)
	if err2 == nil {
		heading = hdg
	}
	// parts[4] = "N DD.DDD E DDD.DDD" or "S DD.DDD W DDD.DDD"
	posStr := strings.TrimSpace(parts[4])
	// Parse: <NS> <lat> <EW> <lon>
	var ns, ew string
	var latVal, lonVal float64
	n, err3 := fmt.Sscanf(posStr, "%s %f %s %f", &ns, &latVal, &ew, &lonVal)
	if err3 != nil || n < 4 {
		return
	}
	lat = latVal
	if ns == "S" {
		lat = -lat
	}
	lon = lonVal
	if ew == "W" {
		lon = -lon
	}
	if !isValidPos(lat, lon) {
		return
	}
	ok = true
	return
}

// parseArinc620Pos attempts to parse an ARINC 620 free-text position report
// from an H1/M1 (or similar) ACARS msg_text.
//
// Format: POS<lat><lon>,<wpt>,<HHMMSS>,<FL>,<next_wpt>,<eta>,<wpt2>,<temp>,<wind_dir><wind_spd>,<fuel><checksum>
// Example: POSN24331E039424,PMA,170413,340,PEGUR,171213,SIGRO,M41,291099,1901F4B
//
// Returns lat, lon, altFt, windDir, windSpd, ok.
// altFt is flight level × 100 (feet).
func parseArinc620Pos(msgText string) (lat, lon, altFt, windDir, windSpd float64, ok bool) {
	// Must start with POS
	if !strings.HasPrefix(msgText, "POS") {
		return
	}
	rest := msgText[3:]

	// Parse lat: N/S + 5 digits (DDMMM where MMM = tenths of minutes)
	// e.g. N24331 = 24°33.1'N
	if len(rest) < 6 {
		return
	}
	latSign := 1.0
	if rest[0] == 'S' {
		latSign = -1.0
	} else if rest[0] != 'N' {
		return
	}
	latStr := rest[1:6]
	latDeg, err1 := strconv.ParseFloat(latStr[:2], 64)
	latMin, err2 := strconv.ParseFloat(latStr[2:], 64)
	if err1 != nil || err2 != nil {
		return
	}
	lat = latSign * (latDeg + latMin/10.0/60.0)
	rest = rest[6:]

	// Parse lon: E/W + 6 digits (DDDMMM)
	if len(rest) < 7 {
		return
	}
	lonSign := 1.0
	if rest[0] == 'W' {
		lonSign = -1.0
	} else if rest[0] != 'E' {
		return
	}
	lonStr := rest[1:7]
	lonDeg, err3 := strconv.ParseFloat(lonStr[:3], 64)
	lonMin, err4 := strconv.ParseFloat(lonStr[3:], 64)
	if err3 != nil || err4 != nil {
		return
	}
	lon = lonSign * (lonDeg + lonMin/10.0/60.0)
	rest = rest[7:]

	// Remaining fields are comma-separated
	// ,<wpt>,<HHMMSS>,<FL>,<next_wpt>,<eta>,<wpt2>,<temp>,<wind_dir><wind_spd>,<fuel+checksum>
	if !strings.HasPrefix(rest, ",") {
		return
	}
	parts := strings.SplitN(rest[1:], ",", 9)
	if len(parts) < 4 {
		return
	}
	// parts[0] = waypoint, parts[1] = time, parts[2] = FL, parts[3] = next_wpt ...
	flStr := parts[2]
	fl, err5 := strconv.ParseFloat(flStr, 64)
	if err5 == nil && fl > 0 {
		altFt = fl * 100 // FL340 → 34000 ft
	}

	// Wind is at parts[7] if available: e.g. "291099" = 291° at 099 kts
	if len(parts) >= 8 {
		windField := parts[7]
		// Remove leading sign for temperature (parts[6] = temp like "M41")
		// Wind field: 6 chars = 3 dir + 3 spd
		if len(windField) == 6 {
			wd, e1 := strconv.ParseFloat(windField[:3], 64)
			ws, e2 := strconv.ParseFloat(windField[3:], 64)
			if e1 == nil && e2 == nil {
				windDir = wd
				windSpd = ws
			}
		}
	}

	if !isValidPos(lat, lon) {
		return
	}
	ok = true
	return
}

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
const maxRecentEvents = 200

// ---------------------------------------------------------------------------
// JSON message types (subset of dumphfdl's decoded:json output)
// ---------------------------------------------------------------------------

// mpduBitrateStats holds per-bitrate MPDU counters from pdu_stats.
// JSON: { "300bps": N, "600bps": N, "1200bps": N, "1800bps": N }
type mpduBitrateStats struct {
	Cnt300  int `json:"300bps"`
	Cnt600  int `json:"600bps"`
	Cnt1200 int `json:"1200bps"`
	Cnt1800 int `json:"1800bps"`
}

// total returns the sum across all bitrates.
func (m *mpduBitrateStats) total() int {
	if m == nil {
		return 0
	}
	return m.Cnt300 + m.Cnt600 + m.Cnt1200 + m.Cnt1800
}

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
				// Phase 1a: ICAO is embedded inside src when aircraft is in AC cache
				AcInfo *struct {
					ICAO string `json:"icao"`
				} `json:"ac_info"`
			} `json:"src"`
			Dst struct {
				Type string `json:"type"`
				ID   int    `json:"id"`
			} `json:"dst"`
			Type struct {
				Name string `json:"name"`
			} `json:"type"`
			// Top-level ac_info (present on logon frames before AC cache is populated)
			AcInfo *struct {
				ICAO string `json:"icao"`
			} `json:"ac_info"`
			// Section 2.3: logon/logoff detail fields
			AssignedAcID int `json:"assigned_ac_id"`
			Reason       *struct {
				Code  int    `json:"code"`
				Descr string `json:"descr"`
			} `json:"reason"`
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
				// Phase 1b: full ACARS payload including msg_text
				ACARS *struct {
					Reg      string `json:"reg"`
					Flight   string `json:"flight"`
					Label    string `json:"label"`
					Sublabel string `json:"sublabel"`
					MsgText  string `json:"msg_text"`
					// Phase 3c: media-adv current_link
					MediaAdv *struct {
						CurrentLink *struct {
							Code string `json:"code"`
							Name string `json:"name"`
						} `json:"current_link"`
					} `json:"media-adv"`
					// Section 2.6.4: ADS-C position contracts via libacars
					ARINC622 *struct {
						ADSC *struct {
							Tags []struct {
								BasicReport *struct {
									Lat float64 `json:"lat"`
									Lon float64 `json:"lon"`
									Alt float64 `json:"alt"` // feet
								} `json:"basic_report"`
								FlightID *struct {
									ID string `json:"id"`
								} `json:"flight_id"`
								// earth_ref tag: ground track and speed
								EarthRef *struct {
									TrueTrkDeg   float64 `json:"true_trk_deg"`
									TrueTrkValid bool    `json:"true_trk_valid"`
									GndSpdKts    float64 `json:"gnd_spd_kts"`
									VspdFtmin    float64 `json:"vspd_ftmin"`
								} `json:"earth_ref"`
								// air_ref tag: true heading and Mach
								AirRef *struct {
									TrueHdgDeg   float64 `json:"true_hdg_deg"`
									TrueHdgValid bool    `json:"true_hdg_valid"`
									SpdMach      float64 `json:"spd_mach"`
									VspdFtmin    float64 `json:"vspd_ftmin"`
								} `json:"air_ref"`
							} `json:"tags"`
						} `json:"adsc"`
					} `json:"arinc622"`
				} `json:"acars"`
				// Phase 3a: last frequency change cause
				// JSON: { "code": N, "descr": "..." }
				LastFreqChangeCause *struct {
					Code  int    `json:"code"`
					Descr string `json:"descr"`
				} `json:"last_freq_change_cause"`
				// Phase 3b: PDU error statistics
				// Each counter is an object with per-bitrate counts.
				// We sum across all bitrates to get a total.
				PDUStats *struct {
					MpdusRxOk  *mpduBitrateStats `json:"mpdus_rx_ok_cnt"`
					MpdusRxErr *mpduBitrateStats `json:"mpdus_rx_err_cnt"`
					MpdusTx    *mpduBitrateStats `json:"mpdus_tx_cnt"`
				} `json:"pdu_stats"`
				// Phase 3d: aircraft-reported UTC time.
				// Performance Data (type 209) uses key "time",
				// Frequency Data (type 213) uses key "utc_time".
				// Both have { "hour": N, "min": N, "sec": N }.
				Time *struct {
					Hour int `json:"hour"`
					Min  int `json:"min"`
					Sec  int `json:"sec"`
				} `json:"time"`
				UTCTime *struct {
					Hour int `json:"hour"`
					Min  int `json:"min"`
					Sec  int `json:"sec"`
				} `json:"utc_time"`
				// Phase 4a: propagation data from Frequency Data messages (type 213)
				FreqData []freqDataEntry `json:"freq_data"`
			} `json:"hfnpdu"`
		} `json:"lpdu"`
		SPDU *struct {
			Src struct {
				Type string `json:"type"`
				ID   int    `json:"id"`
			} `json:"src"`
			// Phase 1c: change_note signals a GS state change
			ChangeNote string `json:"change_note"`
			// Phase 2a: full network topology from each beacon
			GSStatus []spduGSStatus `json:"gs_status"`
			// Section 2.7: system table version advertised by this GS
			SystableVersion int `json:"systable_version"`
		} `json:"spdu"`
		// Phase 1d: dumphfdl application metadata
		App *struct {
			Ver string `json:"ver"`
		} `json:"app"`
		Station string `json:"station"`
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
	sum   float64 // running sum (not serialised)
}

const sigBucketSecs = 1800 // 30 minutes
const maxSigBuckets = 96   // 48 hours of history

// GSFreqStats holds per-ground-station statistics on a specific frequency.
type GSFreqStats struct {
	GSID        int          `json:"gs_id"`
	MsgCount    int64        `json:"msg_count"`
	LastSeen    int64        `json:"last_seen"`     // unix seconds
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
	FreqHz  int64                `json:"freq_hz"`
	FreqKHz int64                `json:"freq_khz"`
	GSStats map[int]*GSFreqStats `json:"gs_stats"` // gs_id → stats (only heard GS)
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
	Key      string       `json:"key"` // ICAO hex if known, else registration
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
	Bearing  float64      `json:"bearing"`         // degrees clockwise from north, 0 if unknown
	Track    []TrackPoint `json:"-"`               // position history, served via /aircraft/{key}/track only
	// Phase 3a: last frequency change cause
	LastFreqChangeCause string `json:"last_freq_change_cause,omitempty"`
	// Phase 3b: link quality from pdu_stats
	MPDURx    int     `json:"mpdu_rx,omitempty"`
	MPDUTx    int     `json:"mpdu_tx,omitempty"`
	MPDUErr   int     `json:"mpdu_err,omitempty"`
	ErrorRate float64 `json:"error_rate,omitempty"` // 0–100 %
	// Phase 3c: current datalink type from media-adv
	CurrentLink string `json:"current_link,omitempty"` // e.g. "HF", "VHF", "SATCOM"
	// Phase 3d: aircraft-reported UTC time as "HH:MM:SS UTC"
	AircraftTime string `json:"aircraft_time,omitempty"`
	// Section 2.6.4: ADS-C altitude from basic_report
	AltFt    float64 `json:"alt_ft,omitempty"`    // feet, 0 = unknown
	AltValid bool    `json:"alt_valid,omitempty"` // true if alt_ft was set from ADS-C
	// ADS-C earth_ref tag: ground track and speed
	GndSpdKts    float64 `json:"gnd_spd_kts,omitempty"`  // knots
	VspdFtmin    float64 `json:"vspd_ftmin,omitempty"`   // ft/min (positive = climbing)
	TrueTrkDeg   float64 `json:"true_trk_deg,omitempty"` // degrees true
	TrueTrkValid bool    `json:"true_trk_valid,omitempty"`
	// ARINC 620 position report: wind
	WindDirDeg float64 `json:"wind_dir_deg,omitempty"` // degrees true
	WindSpdKts float64 `json:"wind_spd_kts,omitempty"` // knots
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
	// Phase 1b: ACARS message content
	Label    string `json:"label,omitempty"`
	Sublabel string `json:"sublabel,omitempty"`
	MsgText  string `json:"msg_text,omitempty"`
	// Section 7.1: current datalink for Live Feed column
	CurrentLink string `json:"current_link,omitempty"`
	// Resolved ICAO for src/dst (from slot map or ac_info)
	SrcICAO string `json:"src_icao,omitempty"`
	// Section 2.3: logon/logoff detail
	AssignedAcID int    `json:"assigned_ac_id,omitempty"`
	ReasonCode   int    `json:"reason_code,omitempty"`
	ReasonDescr  string `json:"reason_descr,omitempty"`
}

// gsEvent is the payload for a "gs_event" SSE notification (Phase 1c).
type gsEvent struct {
	Time       int64  `json:"time"`
	GSID       int    `json:"gs_id"`
	Location   string `json:"location"`
	ChangeNote string `json:"change_note"`
	FreqKHz    int64  `json:"freq_khz"`
}

// StatsSnapshot is the full stats payload served at /stats.
type StatsSnapshot struct {
	TotalMessages   int64           `json:"total_messages"`
	StartTime       int64           `json:"start_time"`
	UptimeSecs      int64           `json:"uptime_secs"`
	Frequencies     []*FreqStats    `json:"frequencies"`
	Recent          []RecentMessage `json:"recent"`
	RecentEvents    []gsEvent       `json:"recent_events"`              // ring buffer of gs_event entries
	GroundStations  map[int]string  `json:"ground_stations"`            // gs_id → location name
	DumphfdlVer     string          `json:"dumphfdl_ver,omitempty"`     // Phase 1d
	SystableVersion int             `json:"systable_version,omitempty"` // Section 2.7
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
	HeardFreqsKHz []int64       `json:"heard_freqs_khz,omitempty"` // frequencies actually heard on (as source)
	DstFreqsKHz   []int64       `json:"dst_freqs_khz,omitempty"`   // frequencies seen as destination only
	LastSigLevel  float64       `json:"last_sig_level,omitempty"`  // most recent signal level (dBFS) when GS was source
	// Phase 2b: SPDU-derived network state
	SPDUActive     bool    `json:"spdu_active"`
	SPDULastSeen   int64   `json:"spdu_last_seen,omitempty"`
	UTCSync        bool    `json:"utc_sync"`
	ActiveSlotIDs  []int   `json:"active_slot_ids,omitempty"`  // always available when SPDU seen
	ActiveFreqsKHz []int64 `json:"active_freqs_khz,omitempty"` // only when system table loaded
}

// statsStore holds all aggregated statistics and the SSE subscriber list.
type statsStore struct {
	mu             sync.RWMutex
	startTime      time.Time
	total          int64
	freqs          map[int64]*FreqStats
	recent         []RecentMessage
	aircraft       map[string]*AircraftState  // keyed by ICAO or reg
	heardGS        map[int]int64              // gs_id → last_heard unix seconds
	heardGSFreqs   map[int]map[int64]struct{} // gs_id → set of freq_khz heard on (as source)
	dstGSFreqs     map[int]map[int64]struct{} // gs_id → set of freq_khz seen as destination
	heardGSLastSig map[int]float64            // gs_id → most recent signal level (dBFS) when GS was source
	gsNames        map[int]string             // gs_id → location name (read-only after init)
	freqGSID       map[int][]int              // freq_khz → []gs_id (read-only after init)
	stations       []groundStation            // all ground stations sorted by ID (read-only after init)
	// Slot→ICAO mapping: key "gsID:slotID" → ICAO hex string
	// Populated on Logon confirm; used to resolve aircraft identity on logoff/data frames.
	slotMap map[string]string
	// Phase 2a: SPDU-derived network topology
	networkGS map[int]*NetworkGSState // gs_id → latest SPDU-advertised state
	// Phase 4a: propagation paths from freq_data[]
	// key: "acKey:gsID" → PropPath
	propagation map[string]*PropPath
	// recentEvents: ring buffer of gs_event entries for page-load seeding
	recentEvents []gsEvent
	// Weather: ring buffer of label H1 / sublabel WX messages
	weatherMessages []WeatherMessage
	// Phase 1d: dumphfdl version string (set on first message received)
	dumphfdlVer string
	// Section 2.7: highest system table version seen in any SPDU
	systableVersion int

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
		dstGSFreqs:     make(map[int]map[int64]struct{}),
		heardGSLastSig: make(map[int]float64),
		gsNames:        gsNames,
		freqGSID:       freqGSID,
		stations:       stations,
		slotMap:        make(map[string]string),
		networkGS:      make(map[int]*NetworkGSState),
		propagation:    make(map[string]*PropPath),
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
		// Section 2.3: logon/logoff detail fields
		if h.LPDU.AssignedAcID != 0 {
			rm.AssignedAcID = h.LPDU.AssignedAcID
		}
		if h.LPDU.Reason != nil {
			rm.ReasonCode = h.LPDU.Reason.Code
			rm.ReasonDescr = h.LPDU.Reason.Descr
		}

		if h.LPDU.Src.Type == "Ground station" {
			recordGSSrc(h.LPDU.Src.ID)
		}
		if h.LPDU.Dst.Type == "Ground station" {
			dstID := h.LPDU.Dst.ID
			if s.dstGSFreqs[dstID] == nil {
				s.dstGSFreqs[dstID] = make(map[int64]struct{})
			}
			s.dstGSFreqs[dstID][freqKHz] = struct{}{}
		}

		// Gather identity fields.
		// Phase 1a: ICAO may be at top-level ac_info (logon frames) OR embedded
		// inside lpdu.src.ac_info (when aircraft is already in the AC cache).
		icao := ""
		if h.LPDU.AcInfo != nil && h.LPDU.AcInfo.ICAO != "" {
			icao = h.LPDU.AcInfo.ICAO
		} else if h.LPDU.Src.AcInfo != nil && h.LPDU.Src.AcInfo.ICAO != "" {
			icao = h.LPDU.Src.AcInfo.ICAO
		}

		// Slot→ICAO mapping:
		// On Logon confirm: store ICAO → slot so we can resolve later frames.
		// On aircraft frames with no ICAO: look up slot to get ICAO.
		gsForSlot := 0
		if h.LPDU.Dst.Type == "Ground station" {
			gsForSlot = h.LPDU.Dst.ID
		} else if h.LPDU.Src.Type == "Ground station" {
			gsForSlot = h.LPDU.Src.ID
		}
		if h.LPDU.Type.Name == "Logon confirm" && icao != "" && h.LPDU.AssignedAcID != 0 && gsForSlot != 0 {
			slotKey := fmt.Sprintf("%d:%d", gsForSlot, h.LPDU.AssignedAcID)
			s.slotMap[slotKey] = icao
		}
		// If src is an aircraft with a slot ID but no ICAO, look up the slot map
		if icao == "" && h.LPDU.Src.Type == "Aircraft" && h.LPDU.Src.ID != 0 && gsForSlot != 0 {
			slotKey := fmt.Sprintf("%d:%d", gsForSlot, h.LPDU.Src.ID)
			if mapped := s.slotMap[slotKey]; mapped != "" {
				icao = mapped
			}
		}
		// Populate resolved ICAO in RecentMessage so Live Feed can show it
		if icao != "" {
			rm.SrcICAO = icao
		}
		reg := ""
		flight := ""
		if h.LPDU.HFNPDU != nil {
			if h.LPDU.HFNPDU.ACARS != nil {
				reg = strings.TrimPrefix(h.LPDU.HFNPDU.ACARS.Reg, ".")
				flight = strings.TrimPrefix(h.LPDU.HFNPDU.ACARS.Flight, ".")
				// Phase 1b: populate ACARS content fields in the recent message
				rm.Label = h.LPDU.HFNPDU.ACARS.Label
				rm.Sublabel = h.LPDU.HFNPDU.ACARS.Sublabel
				rm.MsgText = h.LPDU.HFNPDU.ACARS.MsgText
				// Label 16 weather/position observation parser
				if h.LPDU.HFNPDU.ACARS.Label == "16" && h.LPDU.HFNPDU.ACARS.MsgText != "" {
					if l16Lat, l16Lon, l16Alt, l16Hdg, l16Ok :=
						parseLabel16Obs(h.LPDU.HFNPDU.ACARS.MsgText); l16Ok {
						l16AcKey := icao
						if l16AcKey == "" {
							l16AcKey = strings.TrimPrefix(h.LPDU.HFNPDU.ACARS.Reg, ".")
						}
						if l16AcKey == "" {
							l16AcKey = strings.TrimPrefix(h.LPDU.HFNPDU.ACARS.Flight, ".")
						}
						if l16AcKey != "" {
							ac := s.aircraft[l16AcKey]
							if ac == nil {
								ac = &AircraftState{Key: l16AcKey}
								s.aircraft[l16AcKey] = ac
							}
							if !isValidPos(ac.Lat, ac.Lon) || ac.LastSeen < now-300 {
								ac.Lat = l16Lat
								ac.Lon = l16Lon
								ac.LastSeen = now
								ac.FreqKHz = freqKHz
								if h.LPDU.Dst.Type == "Ground station" {
									ac.GSID = h.LPDU.Dst.ID
								} else if h.LPDU.Src.Type == "Ground station" {
									ac.GSID = h.LPDU.Src.ID
								}
								ac.Track = append(ac.Track, TrackPoint{Lat: l16Lat, Lon: l16Lon, Time: now})
								if len(ac.Track) > maxTrackPoints {
									ac.Track = ac.Track[len(ac.Track)-maxTrackPoints:]
								}
								if n := len(ac.Track); n >= 2 {
									prev := ac.Track[n-2]
									ac.Bearing = bearingDeg(prev.Lat, prev.Lon, l16Lat, l16Lon)
								}
								if posUpdate == nil {
									posUpdate = ac
								}
							}
							if l16Alt > 0 {
								ac.AltFt = l16Alt
								ac.AltValid = true
							}
							if l16Hdg > 0 {
								ac.TrueTrkDeg = l16Hdg
								ac.TrueTrkValid = true
							}
						}
					}
				}
				// ARINC 620 position report parser — H1 messages starting with POS
				// Updates AircraftState with lat/lon/alt/wind from free-text position reports.
				if h.LPDU.HFNPDU.ACARS.Label == "H1" &&
					strings.HasPrefix(h.LPDU.HFNPDU.ACARS.MsgText, "POS") {
					if parsedLat, parsedLon, parsedAlt, parsedWDir, parsedWSpd, posOk :=
						parseArinc620Pos(h.LPDU.HFNPDU.ACARS.MsgText); posOk {
						// Determine acKey for this aircraft
						posAcKey := icao
						if posAcKey == "" {
							posAcKey = strings.TrimPrefix(h.LPDU.HFNPDU.ACARS.Reg, ".")
						}
						if posAcKey == "" {
							posAcKey = strings.TrimPrefix(h.LPDU.HFNPDU.ACARS.Flight, ".")
						}
						if posAcKey != "" {
							ac := s.aircraft[posAcKey]
							if ac == nil {
								ac = &AircraftState{Key: posAcKey}
								s.aircraft[posAcKey] = ac
							}
							// Only update position if HFNPDU pos is absent
							if !isValidPos(ac.Lat, ac.Lon) || ac.LastSeen < now-300 {
								ac.Lat = parsedLat
								ac.Lon = parsedLon
								ac.LastSeen = now
								ac.FreqKHz = freqKHz
								if h.LPDU.Dst.Type == "Ground station" {
									ac.GSID = h.LPDU.Dst.ID
								} else if h.LPDU.Src.Type == "Ground station" {
									ac.GSID = h.LPDU.Src.ID
								}
								ac.Track = append(ac.Track, TrackPoint{Lat: parsedLat, Lon: parsedLon, Time: now})
								if len(ac.Track) > maxTrackPoints {
									ac.Track = ac.Track[len(ac.Track)-maxTrackPoints:]
								}
								if n := len(ac.Track); n >= 2 {
									prev := ac.Track[n-2]
									ac.Bearing = bearingDeg(prev.Lat, prev.Lon, parsedLat, parsedLon)
								}
								if posUpdate == nil {
									posUpdate = ac
								}
							}
							if parsedAlt > 0 {
								ac.AltFt = parsedAlt
								ac.AltValid = true
							}
							if parsedWSpd > 0 {
								ac.WindDirDeg = parsedWDir
								ac.WindSpdKts = parsedWSpd
							}
						}
					}
				}
				// Weather: store H1/WX messages in the weather ring buffer
				if h.LPDU.HFNPDU.ACARS.Label == "H1" &&
					h.LPDU.HFNPDU.ACARS.Sublabel == "WX" &&
					h.LPDU.HFNPDU.ACARS.MsgText != "" {
					gsID := 0
					if h.LPDU.Dst.Type == "Ground station" {
						gsID = h.LPDU.Dst.ID
					} else if h.LPDU.Src.Type == "Ground station" {
						gsID = h.LPDU.Src.ID
					}
					wm := WeatherMessage{
						Time:     now,
						FreqKHz:  freqKHz,
						Reg:      strings.TrimPrefix(h.LPDU.HFNPDU.ACARS.Reg, "."),
						Flight:   strings.TrimPrefix(h.LPDU.HFNPDU.ACARS.Flight, "."),
						GSID:     gsID,
						Label:    h.LPDU.HFNPDU.ACARS.Label,
						Sublabel: h.LPDU.HFNPDU.ACARS.Sublabel,
						MsgText:  h.LPDU.HFNPDU.ACARS.MsgText,
					}
					s.weatherMessages = append([]WeatherMessage{wm}, s.weatherMessages...)
					if len(s.weatherMessages) > maxWeatherMessages {
						s.weatherMessages = s.weatherMessages[:maxWeatherMessages]
					}
				}
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
				// Phase 3a: last frequency change cause
				// JSON: { "code": N, "descr": "..." }
				if h.LPDU.HFNPDU.LastFreqChangeCause != nil &&
					h.LPDU.HFNPDU.LastFreqChangeCause.Descr != "" {
					existing.LastFreqChangeCause = h.LPDU.HFNPDU.LastFreqChangeCause.Descr
				}
				// Phase 3b: PDU error statistics
				// Sum across all bitrates to get totals.
				if h.LPDU.HFNPDU.PDUStats != nil {
					ps := h.LPDU.HFNPDU.PDUStats
					rx := ps.MpdusRxOk.total()
					err := ps.MpdusRxErr.total()
					tx := ps.MpdusTx.total()
					existing.MPDURx = rx
					existing.MPDUTx = tx
					existing.MPDUErr = err
					total := rx + err // rx_ok + rx_err = total received
					if total > 0 {
						existing.ErrorRate = float64(err) / float64(total) * 100
					}
				}
				// Phase 3c: current datalink from media-adv
				if h.LPDU.HFNPDU.ACARS != nil &&
					h.LPDU.HFNPDU.ACARS.MediaAdv != nil &&
					h.LPDU.HFNPDU.ACARS.MediaAdv.CurrentLink != nil {
					existing.CurrentLink = h.LPDU.HFNPDU.ACARS.MediaAdv.CurrentLink.Code
					// Also populate RecentMessage for the Live Feed datalink column
					rm.CurrentLink = h.LPDU.HFNPDU.ACARS.MediaAdv.CurrentLink.Code
				}
				// Phase 3d: aircraft-reported UTC time — format as "HH:MM:SS UTC"
				if h.LPDU.HFNPDU.Time != nil {
					t := h.LPDU.HFNPDU.Time
					existing.AircraftTime = fmt.Sprintf("%02d:%02d:%02d UTC", t.Hour, t.Min, t.Sec)
				} else if h.LPDU.HFNPDU.UTCTime != nil {
					t := h.LPDU.HFNPDU.UTCTime
					existing.AircraftTime = fmt.Sprintf("%02d:%02d:%02d UTC", t.Hour, t.Min, t.Sec)
				}
				// Section 2.6.4: ADS-C altitude from arinc622.adsc.tags[].basic_report
				if h.LPDU.HFNPDU.ACARS != nil &&
					h.LPDU.HFNPDU.ACARS.ARINC622 != nil &&
					h.LPDU.HFNPDU.ACARS.ARINC622.ADSC != nil {
					for _, tag := range h.LPDU.HFNPDU.ACARS.ARINC622.ADSC.Tags {
						if tag.BasicReport != nil && tag.BasicReport.Alt != 0 {
							existing.AltFt = tag.BasicReport.Alt
							existing.AltValid = true
							// ADS-C basic_report also carries lat/lon — use if more precise
							// than the HFNPDU pos (which may be absent in Enveloped Data)
							if !isValidPos(existing.Lat, existing.Lon) &&
								isValidPos(tag.BasicReport.Lat, tag.BasicReport.Lon) {
								existing.Lat = tag.BasicReport.Lat
								existing.Lon = tag.BasicReport.Lon
							}
						}
						// earth_ref tag: ground track and speed
						if tag.EarthRef != nil {
							existing.GndSpdKts = tag.EarthRef.GndSpdKts
							existing.VspdFtmin = tag.EarthRef.VspdFtmin
							if tag.EarthRef.TrueTrkValid {
								existing.TrueTrkDeg = tag.EarthRef.TrueTrkDeg
								existing.TrueTrkValid = true
							}
						}
					}
				}
				// Phase 4a: propagation paths from freq_data[]
				for _, fde := range h.LPDU.HFNPDU.FreqData {
					gsID := fde.GS.ID
					propKey := fmt.Sprintf("%s:%d", acKey, gsID)
					// Pick the best (highest) signal level from the freq list
					bestSig := -999.0
					bestFreqKHz := int64(0)
					for _, f := range fde.Freqs {
						if f.SigLevel > bestSig {
							bestSig = f.SigLevel
							bestFreqKHz = f.Freq / 1000
						}
					}
					loc := s.gsNames[gsID]
					if loc == "" {
						loc = fmt.Sprintf("GS %d", gsID)
					}
					pp := s.propagation[propKey]
					if pp == nil {
						pp = &PropPath{AircraftKey: acKey}
						s.propagation[propKey] = pp
					}
					pp.GSID = gsID
					pp.GSLocation = loc
					pp.LastSeen = now
					if bestFreqKHz > 0 {
						pp.FreqKHz = bestFreqKHz
						pp.SigLevel = bestSig
					}
					// Keep identity fields up to date
					if icao != "" {
						pp.ICAO = icao
					}
					if reg != "" {
						pp.Reg = reg
					}
					if flight != "" {
						pp.Flight = flight
					}
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
		// Section 2.7: track highest system table version seen
		if h.SPDU.SystableVersion > s.systableVersion {
			s.systableVersion = h.SPDU.SystableVersion
		}
		// Phase 2a: update networkGS from gs_status array
		for _, gss := range h.SPDU.GSStatus {
			gsID := gss.GS.ID
			state, ok := s.networkGS[gsID]
			if !ok {
				loc := s.gsNames[gsID]
				if loc == "" {
					loc = fmt.Sprintf("GS %d", gsID)
				}
				state = &NetworkGSState{GSID: gsID, Location: loc}
				s.networkGS[gsID] = state
			}
			state.UTCSync = gss.UTCSync
			state.SPDULastSeen = now
			state.SPDUActive = true
			// Rebuild active slot/freq lists from this beacon.
			// f.ID is always present (slot index, 0-based bitmask position).
			// f.Freq is in kHz (float64), only present when system table is loaded.
			state.ActiveSlotIDs = make([]int, 0, len(gss.Freqs))
			state.ActiveFreqsKHz = make([]int64, 0, len(gss.Freqs))
			for _, f := range gss.Freqs {
				state.ActiveSlotIDs = append(state.ActiveSlotIDs, f.ID)
				if f.Freq > 0 {
					state.ActiveFreqsKHz = append(state.ActiveFreqsKHz, int64(f.Freq))
				}
			}
		}
	}

	// Phase 1d: capture dumphfdl version from first message that carries it
	if s.dumphfdlVer == "" && h.App != nil && h.App.Ver != "" {
		s.dumphfdlVer = h.App.Ver
	}

	// Phase 1c: capture change_note for gs_event emission after unlock
	var gsEvt *gsEvent
	if h.SPDU != nil && h.SPDU.ChangeNote != "" && h.SPDU.ChangeNote != "None" {
		gsID := h.SPDU.Src.ID
		loc := s.gsNames[gsID]
		if loc == "" {
			loc = fmt.Sprintf("GS %d", gsID)
		}
		gsEvt = &gsEvent{
			Time:       now,
			GSID:       gsID,
			Location:   loc,
			ChangeNote: h.SPDU.ChangeNote,
			FreqKHz:    freqKHz,
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

	// Phase 1c: broadcast gs_event if a ground station state change was detected
	if gsEvt != nil {
		// Also store in the recentEvents ring buffer so page-load can seed the Events tab
		s.mu.Lock()
		s.recentEvents = append(s.recentEvents, *gsEvt)
		if len(s.recentEvents) > maxRecentEvents {
			s.recentEvents = s.recentEvents[len(s.recentEvents)-maxRecentEvents:]
		}
		s.mu.Unlock()

		if ev, err := json.Marshal(sseEvent{Type: "gs_event", Data: gsEvt}); err == nil {
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

	recentEvts := make([]gsEvent, len(s.recentEvents))
	copy(recentEvts, s.recentEvents)

	return StatsSnapshot{
		TotalMessages:   s.total,
		StartTime:       s.startTime.Unix(),
		UptimeSecs:      int64(time.Since(s.startTime).Seconds()),
		Frequencies:     freqs,
		Recent:          recent,
		RecentEvents:    recentEvts,
		GroundStations:  s.gsNames,
		DumphfdlVer:     s.dumphfdlVer,
		SystableVersion: s.systableVersion,
	}
}

// networkSnapshot returns a copy of all known NetworkGSState entries,
// sorted by GS ID.  Used by GET /network.
func (s *statsStore) networkSnapshot() []NetworkGSState {
	s.mu.RLock()
	defer s.mu.RUnlock()

	const spduActiveSecs = int64(10 * 60) // 10 minutes
	now := time.Now().Unix()

	result := make([]NetworkGSState, 0, len(s.networkGS))
	for _, state := range s.networkGS {
		cp := *state
		cp.SPDUActive = (now - state.SPDULastSeen) < spduActiveSecs
		result = append(result, cp)
	}
	sort.Slice(result, func(i, j int) bool {
		return result[i].GSID < result[j].GSID
	})
	return result
}

// propagationSnapshot returns a PropSnapshot built from all known propagation
// paths.  Used by GET /propagation.
func (s *statsStore) propagationSnapshot() PropSnapshot {
	s.mu.RLock()
	defer s.mu.RUnlock()

	paths := make([]PropPath, 0, len(s.propagation))
	byGS := make(map[int][]string)
	byAC := make(map[string][]int)

	for _, pp := range s.propagation {
		cp := *pp
		paths = append(paths, cp)
		byGS[pp.GSID] = append(byGS[pp.GSID], pp.AircraftKey)
		byAC[pp.AircraftKey] = append(byAC[pp.AircraftKey], pp.GSID)
	}
	sort.Slice(paths, func(i, j int) bool {
		if paths[i].AircraftKey != paths[j].AircraftKey {
			return paths[i].AircraftKey < paths[j].AircraftKey
		}
		return paths[i].GSID < paths[j].GSID
	})
	return PropSnapshot{Paths: paths, ByGS: byGS, ByAircraft: byAC}
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

// groundStationsSnapshot returns the station list merged with last-heard times,
// heard frequencies, and Phase 2b SPDU-derived network state.
func (s *statsStore) groundStationsSnapshot() []groundStationResponse {
	s.mu.RLock()
	defer s.mu.RUnlock()

	const spduActiveSecs = int64(10 * 60) // 10 minutes
	now := time.Now().Unix()

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
		var dstFreqs []int64
		if fset := s.dstGSFreqs[gs.GSID]; len(fset) > 0 {
			dstFreqs = make([]int64, 0, len(fset))
			for f := range fset {
				dstFreqs = append(dstFreqs, f)
			}
			sort.Slice(dstFreqs, func(a, b int) bool { return dstFreqs[a] < dstFreqs[b] })
		}
		r := groundStationResponse{
			GSID:          gs.GSID,
			Location:      gs.Location,
			Lat:           gs.Lat,
			Lon:           gs.Lon,
			Freqs:         gs.Freqs,
			LastHeard:     s.heardGS[gs.GSID],
			HeardFreqsKHz: heardFreqs,
			DstFreqsKHz:   dstFreqs,
			LastSigLevel:  s.heardGSLastSig[gs.GSID],
		}
		// Phase 2b: merge SPDU network state if available
		if net, ok := s.networkGS[gs.GSID]; ok {
			r.SPDUActive = (now - net.SPDULastSeen) < spduActiveSecs
			r.SPDULastSeen = net.SPDULastSeen
			r.UTCSync = net.UTCSync
			r.ActiveSlotIDs = net.ActiveSlotIDs
			r.ActiveFreqsKHz = net.ActiveFreqsKHz
		}
		result[i] = r
	}
	return result
}

// exportAllFrequencies returns a JSONL byte slice identical in structure to
// the original hfdl_frequencies.jsonl, with every frequency's enabled field
// unconditionally set to true.
func (s *statsStore) exportAllFrequencies() []byte {
	s.mu.RLock()
	defer s.mu.RUnlock()

	type exportFreq struct {
		FreqKHz  int  `json:"freq_khz"`
		Timeslot int  `json:"timeslot"`
		Enabled  bool `json:"enabled"`
	}
	type exportStation struct {
		GSID     int          `json:"gs_id"`
		Location string       `json:"location"`
		Lat      float64      `json:"lat"`
		Lon      float64      `json:"lon"`
		Freqs    []exportFreq `json:"frequencies"`
	}

	var buf []byte
	for _, gs := range s.stations {
		freqs := make([]exportFreq, len(gs.Freqs))
		for i, f := range gs.Freqs {
			freqs[i] = exportFreq{
				FreqKHz:  f.FreqKHz,
				Timeslot: f.Timeslot,
				Enabled:  true,
			}
		}
		row := exportStation{
			GSID:     gs.GSID,
			Location: gs.Location,
			Lat:      gs.Lat,
			Lon:      gs.Lon,
			Freqs:    freqs,
		}
		b, err := json.Marshal(row)
		if err != nil {
			continue
		}
		buf = append(buf, b...)
		buf = append(buf, '\n')
	}
	return buf
}

// exportActiveFrequencies returns a JSONL byte slice identical in structure to
// the original hfdl_frequencies.jsonl, with each frequency's enabled field set
// to true only if at least one message has been received on that frequency.
func (s *statsStore) exportActiveFrequencies() []byte {
	s.mu.RLock()
	defer s.mu.RUnlock()

	// Build set of active freq_khz values from the freqs map.
	activeKHz := make(map[int64]bool, len(s.freqs))
	for khz := range s.freqs {
		activeKHz[khz/1000] = true
	}

	type exportFreq struct {
		FreqKHz  int  `json:"freq_khz"`
		Timeslot int  `json:"timeslot"`
		Enabled  bool `json:"enabled"`
	}
	type exportStation struct {
		GSID     int          `json:"gs_id"`
		Location string       `json:"location"`
		Lat      float64      `json:"lat"`
		Lon      float64      `json:"lon"`
		Freqs    []exportFreq `json:"frequencies"`
	}

	var buf []byte
	for _, gs := range s.stations {
		freqs := make([]exportFreq, len(gs.Freqs))
		for i, f := range gs.Freqs {
			freqs[i] = exportFreq{
				FreqKHz:  f.FreqKHz,
				Timeslot: f.Timeslot,
				Enabled:  activeKHz[int64(f.FreqKHz)],
			}
		}
		row := exportStation{
			GSID:     gs.GSID,
			Location: gs.Location,
			Lat:      gs.Lat,
			Lon:      gs.Lon,
			Freqs:    freqs,
		}
		b, err := json.Marshal(row)
		if err != nil {
			continue
		}
		buf = append(buf, b...)
		buf = append(buf, '\n')
	}
	return buf
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
