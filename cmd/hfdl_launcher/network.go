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
// dumphfdl JSON: { "id": 2, "freq": 8927.0 }
// "id" is the slot index (0-based bitmask position).
// "freq" is the actual frequency in kHz (float64), only present when the
// system table is loaded by dumphfdl.  If the system table is not loaded,
// only "id" is present and freq will be 0.
type spduGSFreq struct {
	ID   int     `json:"id"`
	Freq float64 `json:"freq"` // kHz (already in kHz, not Hz)
}

// spduGSStatus is one element of the gs_status array in an SPDU.
// dumphfdl JSON: { "gs": { "id": 7 }, "utc_sync": true, "freqs": [...] }
// Note: utc_sync is a sibling of "gs", NOT nested inside it.
type spduGSStatus struct {
	GS struct {
		ID int `json:"id"`
	} `json:"gs"`
	UTCSync bool         `json:"utc_sync"`
	Freqs   []spduGSFreq `json:"freqs"`
}

// NetworkGSState is the aggregated view of one ground station derived from
// SPDU beacons.  It is served via GET /network and merged into
// GET /groundstations.
type NetworkGSState struct {
	GSID           int     `json:"gs_id"`
	Location       string  `json:"location"`
	UTCSync        bool    `json:"utc_sync"`
	ActiveFreqsKHz []int64 `json:"active_freqs_khz"` // advertised active freqs in kHz
	SPDULastSeen   int64   `json:"spdu_last_seen"`   // unix seconds of last SPDU that mentioned this GS
	SPDUActive     bool    `json:"spdu_active"`      // true if seen in last 10 minutes
}
