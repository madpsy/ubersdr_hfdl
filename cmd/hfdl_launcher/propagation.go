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
}

// PropSnapshot is the full propagation payload served at GET /propagation.
type PropSnapshot struct {
	// Paths is a flat list of all currently known propagation paths.
	Paths []PropPath `json:"paths"`
	// ByGS maps gs_id → list of aircraft keys that can hear it.
	ByGS map[int][]string `json:"by_gs"`
	// ByAircraft maps aircraft_key → list of gs_ids it can hear.
	ByAircraft map[string][]int `json:"by_aircraft"`
}
