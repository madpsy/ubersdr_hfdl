package main

// ---------------------------------------------------------------------------
// HFDL propagation data — derived from HFNPDU Frequency Data (type 213)
//
// When an aircraft sends a Frequency Data message, it reports which ground
// stations it can currently hear and at what signal level on each frequency.
// This gives us a real-time propagation matrix: which GS → aircraft paths
// are currently open.
//
// dumphfdl JSON shape (hfnpdu.freq_data[]):
//   [
//     {
//       "gs": { "id": 7 },
//       "freqs": [
//         { "id": 3, "freq": 8927000, "timeslot": 12, "sig_level": -42.1 },
//         ...
//       ]
//     },
//     ...
//   ]
// ---------------------------------------------------------------------------

// freqDataGSFreq is one frequency entry inside a freq_data element.
type freqDataGSFreq struct {
	ID       int     `json:"id"`
	Freq     int64   `json:"freq"` // Hz
	Timeslot int     `json:"timeslot"`
	SigLevel float64 `json:"sig_level"` // dBFS (may be 0 if not reported)
}

// freqDataEntry is one element of the hfnpdu.freq_data array.
type freqDataEntry struct {
	GS struct {
		ID int `json:"id"`
	} `json:"gs"`
	Freqs []freqDataGSFreq `json:"freqs"`
}

// SigSample is one signal-strength observation recorded whenever a message
// arrives from an aircraft at a known position.  Unlike PropPath (which keeps
// only the latest reading per aircraft→GS pair), SigSample entries are stored
// in a ring buffer so the heatmap has a dense point cloud covering many hours
// of flight activity.
//
// h.SigLevel is the signal level measured by YOUR receiver for that aircraft
// transmission, and Lat/Lon is the aircraft's self-reported position from the
// same HFNPDU.  Together they map "where in the world can my receiver hear
// aircraft on this frequency, and how well."
type SigSample struct {
	Lat      float64 `json:"lat"`
	Lon      float64 `json:"lon"`
	FreqKHz  int64   `json:"freq_khz"`
	SigLevel float64 `json:"sig_level"`
	Time     int64   `json:"time"`
}

// PropPath represents one aircraft → GS propagation path.
// It is the unit stored in statsStore.propagation.
type PropPath struct {
	AircraftKey string  `json:"aircraft_key"` // ICAO or reg
	ICAO        string  `json:"icao,omitempty"`
	Reg         string  `json:"reg,omitempty"`
	Flight      string  `json:"flight,omitempty"`
	GSID        int     `json:"gs_id"`
	GSLocation  string  `json:"gs_location"`
	FreqKHz     int64   `json:"freq_khz"`  // best/last heard frequency
	SigLevel    float64 `json:"sig_level"` // dBFS
	LastSeen    int64   `json:"last_seen"` // unix seconds
	// Aircraft position at time of last freq_data report.
	// Populated server-side in propagationSnapshot() by joining against
	// statsStore.aircraft so the frontend can place signal samples on a map
	// without a client-side join (which would miss aircraft older than 30 min).
	AcLat float64 `json:"ac_lat,omitempty"`
	AcLon float64 `json:"ac_lon,omitempty"`
}

// GridCell is one pre-averaged signal cell in the 2°×2° spatial grid served
// at GET /propagation/grid.  The server bins all SigSample entries by
// (latBin, lonBin, bandMHz) and averages the signal levels so the frontend
// receives a compact, ready-to-render dataset instead of raw samples.
type GridCell struct {
	Lat      float64 `json:"lat"`       // cell centre latitude
	Lon      float64 `json:"lon"`       // cell centre longitude
	FreqKHz  int64   `json:"freq_khz"`  // representative frequency (most common in cell)
	BandMHz  int     `json:"band_mhz"`  // MHz band (floor(freq_khz/1000))
	SigLevel float64 `json:"sig_level"` // average dBFS across all samples in cell
	Count    int     `json:"count"`     // number of raw samples averaged
}

// GridSnapshot is the payload served at GET /propagation/grid.
type GridSnapshot struct {
	Cells     []GridCell `json:"cells"`
	CellDeg   float64    `json:"cell_deg"`   // grid cell size in degrees (always 2.0)
	SampleN   int        `json:"sample_n"`   // total raw samples used
	UpdatedAt int64      `json:"updated_at"` // unix seconds
}

// PropSnapshot is the full propagation payload served at GET /propagation.
type PropSnapshot struct {
	// Paths is a flat list of all currently known propagation paths.
	Paths []PropPath `json:"paths"`
	// ByGS maps gs_id → list of aircraft keys that can hear it.
	ByGS map[int][]string `json:"by_gs"`
	// ByAircraft maps aircraft_key → list of gs_ids it can hear.
	ByAircraft map[string][]int `json:"by_aircraft"`
	// Samples is a rolling window of raw signal observations from every
	// positioned aircraft message.  Used by the heatmap and contour renderers
	// in the frontend; provides far more data points than Paths alone.
	Samples []SigSample `json:"samples"`
}
