package main

// ---------------------------------------------------------------------------
// HFDL network state — derived from SPDU gs_status[] beacons
//
// Every SPDU (ground station squitter) carries a gs_status array of up to 3
// entries, each describing one ground station's currently active frequency
// slots and UTC sync status.  This file defines the types and the
// networkGSState map that is maintained inside statsStore.
// ---------------------------------------------------------------------------

// spduGSFreq is one frequency entry inside a gs_status element.
// dumphfdl JSON: { "id": 3, "freq": 8927000, "timeslot": 12 }
type spduGSFreq struct {
	ID       int   `json:"id"`
	Freq     int64 `json:"freq"` // Hz
	Timeslot int   `json:"timeslot"`
}

// spduGSStatus is one element of the gs_status array in an SPDU.
// dumphfdl JSON: { "gs": { "id": 7, "utc_sync": true }, "freqs": [...] }
type spduGSStatus struct {
	GS struct {
		ID      int  `json:"id"`
		UTCSync bool `json:"utc_sync"`
	} `json:"gs"`
	Freqs []spduGSFreq `json:"freqs"`
}

// NetworkGSState is the aggregated view of one ground station derived from
// SPDU beacons.  It is served via GET /network and merged into
// GET /groundstations.
type NetworkGSState struct {
	GSID           int     `json:"gs_id"`
	Location       string  `json:"location"`
	UTCSync        bool    `json:"utc_sync"`
	ActiveFreqsHz  []int64 `json:"active_freqs_hz"`  // currently advertised active freqs (Hz)
	ActiveFreqsKHz []int64 `json:"active_freqs_khz"` // same in kHz for convenience
	SPDULastSeen   int64   `json:"spdu_last_seen"`   // unix seconds of last SPDU that mentioned this GS
	SPDUActive     bool    `json:"spdu_active"`      // true if seen in last 10 minutes
}
